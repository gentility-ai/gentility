package phonenode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"sync"
	"time"

	"github.com/gentility-ai/gentility/connector/internal/config"
	"github.com/gentility-ai/gentility/connector/internal/enroll"
	"github.com/gentility-ai/gentility/connector/internal/exitcodes"
	"github.com/gentility-ai/gentility/connector/internal/report"
	"github.com/gentility-ai/gentility/connector/internal/state"
	"github.com/gentility-ai/gentility/connector/internal/tsnode"
)

// readyWithin is how long a join waits for its first config document. The
// spike measured the netmap at 5.6–7.1 s on a phone and the document under a
// second after it; a minute is a head that is not answering.
const readyWithin = 60 * time.Second

// session is one node that is up: its engine, its config channel, and the
// grants its last document gave it.
type session struct {
	node    *tsnode.Node
	configs *config.Client
	// report and closeNode are node.Status and node.Close; a test stands in
	// for both to hold a node's status or its shutdown.
	report    func(context.Context) (tsnode.Report, error)
	closeNode func()
	cancel    context.CancelFunc
	logf      func(string, ...any)
	nodeID    string

	// channelEnded carries the config channel's end to a join still waiting
	// for its first document; after that, endSession takes it.
	channelEnded chan error

	mu     sync.Mutex
	grants []tsnode.ConfigGrant
}

var (
	// mu guards the five below. It is never held across a join or a node's
	// shutdown: the app calls Status and Down while either is under way, and
	// the spike's bridge, which held it throughout, made both wait up to a
	// minute.
	mu        sync.Mutex
	current   *session
	joining   context.CancelFunc
	ended     string
	endedCode string
	// closing counts nodes taken out of current and not yet shut down. tsnet
	// keeps its node key in the state dir, so a join while one closes would
	// be two engines on one key: it is answered joining, and the app tries
	// again.
	closing int

	// bringUp is enroll.Join. A test replaces it to hold a join open.
	bringUp = enroll.Join
	// bringSession is bring. A test replaces it to hand up a session of its
	// own making.
	bringSession = bring
)

// Join brings a NEW node up from join material: the claim poll's allowed
// answer ({"enrollment":{…}}, other keys ignored) or the inner object. dir is
// the app's own directory for the node; it keeps the identity Rejoin comes
// back on. Blocks until the node holds a config document, or fails.
func Join(dir, hostname, enrollmentJSON string, log Logger) string {
	e, err := readEnrollment(enrollmentJSON)
	if err != nil {
		return failure(codeMalformed, err)
	}
	env, err := enroll.EnvFromEnrollment(e, enroll.EnrollmentOptions{StateDir: dir, Hostname: hostname})
	if err != nil {
		return failure(codeMalformed, err)
	}
	return up(env, state.Node{}, false, logger(log))
}

// Rejoin brings an ENROLLED node back on the identity in dir: no key, no
// exchange, as a server does after a reboot. hostname is not consulted; the
// stored identity names the node (enroll.EnvFromNode).
func Rejoin(dir, hostname string, log Logger) string {
	stored, err := (&state.Store{Dir: dir}).LoadNode()
	switch {
	case state.IsNotEnrolled(err):
		return failure(codeNotEnrolled, fmt.Errorf("%s holds no node identity: pair this phone again", dir))
	case err != nil:
		// Not "gone": a file the phone cannot read yet is still there, and
		// the app wipes a pairing on not_enrolled.
		return failure(codeUnreadable, err)
	}
	env, err := enroll.EnvFromNode(stored, enroll.EnrollmentOptions{StateDir: dir, Hostname: hostname})
	if err != nil {
		return failure(codeNotEnrolled, err)
	}
	return up(env, stored, true, logger(log))
}

// readEnrollment takes the material whole or wrapped, and refuses it without
// quoting it: it holds the pre-auth key and the nonce.
func readEnrollment(raw string) (state.Enrollment, error) {
	var wrapped struct {
		Enrollment *state.Enrollment `json:"enrollment"`
	}
	if err := json.Unmarshal([]byte(raw), &wrapped); err != nil {
		return state.Enrollment{}, errors.New("the join material is not JSON")
	}
	e := state.Enrollment{}
	if wrapped.Enrollment != nil {
		e = *wrapped.Enrollment
	} else if err := json.Unmarshal([]byte(raw), &e); err != nil {
		return state.Enrollment{}, errors.New("the join material is not an enrolment")
	}
	if e.PreauthKey == "" {
		return state.Enrollment{}, errors.New("the join material names no preauth_key")
	}
	return e, nil
}

func up(env enroll.Env, stored state.Node, enrolled bool, logf func(string, ...any)) string {
	mu.Lock()
	switch {
	case current != nil:
		mu.Unlock()
		return failure(codeAlreadyUp, errors.New("a node is already up: call Down first"))
	case joining != nil:
		mu.Unlock()
		return failure(codeJoining, errors.New("a join is already under way"))
	case closing > 0:
		mu.Unlock()
		return failure(codeJoining, errors.New("the last node is still closing: try again"))
	}
	ctx, cancel := context.WithCancel(context.Background())
	joining = cancel
	ended, endedCode = "", ""
	mu.Unlock()

	s, result, err := bringSession(ctx, cancel, env, stored, enrolled, logf)

	// The outcome is decided under mu, so a Down sees the join or the node,
	// never neither; a node that is not kept is shut down after it.
	var retire *session
	mu.Lock()
	joining = nil
	if err == nil && ctx.Err() != nil {
		// Down came in after the node was up and before it was current.
		retire, err = s, ctx.Err()
	}
	if err == nil {
		select {
		case why := <-s.channelEnded:
			// The channel ended between the first document and here, while s
			// was not yet current, so endSession could not take it down.
			retire, err = s, fmt.Errorf("the config channel ended as the node came up: %w", why)
		default:
			current = s
		}
	}
	if retire != nil {
		closing++
	}
	mu.Unlock()

	if err == nil {
		return answer(result)
	}
	calledOff := ctx.Err() != nil
	cancel()
	if retire != nil {
		shutDown(retire)
	}
	if calledOff {
		logf("phonenode: the join was called off by Down")
		return failure(codeCancelled, errors.New("Down was called before the node came up"))
	}
	logf("phonenode: the join failed: %v", err)
	return failure(classify(err), err)
}

// bring is the join itself, in the spike's order, under ctx: Down cancels it.
func bring(ctx context.Context, cancel context.CancelFunc, env enroll.Env, stored state.Node, enrolled bool, logf func(string, ...any)) (*session, map[string]any, error) {
	t0 := time.Now()
	node, addresses, err := bringUp(ctx, env, logf)
	if err != nil {
		return nil, nil, fmt.Errorf("join: %w", err)
	}
	netmap := time.Since(t0)
	logf("phonenode: netmap in %s, %d address(es)", netmap.Round(time.Millisecond), len(addresses))

	store := &state.Store{Dir: env.StateDir}
	base, client, err := enroll.Mothership(env, stored, node, addresses)
	if err != nil {
		node.Close()
		return nil, nil, fmt.Errorf("mothership hop: %w", err)
	}

	var exchange time.Duration
	if !enrolled {
		t1 := time.Now()
		if stored, err = enroll.Exchange(ctx, client, base, env, store); err != nil {
			node.Close()
			return nil, nil, fmt.Errorf("exchange: %w", err)
		}
		exchange = time.Since(t1)
		logf("phonenode: exchanged in %s, node %s", exchange.Round(time.Millisecond), stored.NodeID)
	}

	s := &session{
		node: node, report: node.Status, closeNode: node.Close,
		cancel: cancel, logf: logf, nodeID: stored.NodeID, channelEnded: make(chan error, 1),
	}
	s.configs = config.New(base, stored, store, client)
	s.configs.Log = logf

	presence := &report.Reporter{
		BaseURL:    base,
		Credential: stored.NodeCredential,
		HTTP:       client,
		Status:     heartbeat(node, logf),
		Ready:      s.configs.Ready,
		Log:        logf,
	}
	go presence.Run(ctx)
	go s.follow(ctx, store)

	if err := s.awaitReady(ctx); err != nil {
		node.Close()
		return nil, nil, err
	}
	ready := time.Since(t0)
	logf("phonenode: up in %s", ready.Round(time.Millisecond))

	seen, _ := node.Status(ctx)
	result := map[string]any{
		"ok":          true,
		"addresses":   strs(addresses),
		"name":        seen.Name,
		"node_id":     stored.NodeID,
		"netmap_ms":   netmap.Milliseconds(),
		"exchange_ms": exchange.Milliseconds(),
		"ready_ms":    ready.Milliseconds(),
		"relayed":     seen.Relayed,
		"home_derp":   seen.HomeDERP,
	}
	if len(addresses) > 0 {
		result["address"] = addresses[0].String()
	}
	return s, result, nil
}

// follow is the config channel: every document's grants taken on (they are
// what a door's source is picked from) and kept for Status.
func (s *session) follow(ctx context.Context, store *state.Store) {
	err := s.configs.Run(ctx, func(document *config.Document) error {
		if err := document.Adopt(s.node, store); err != nil {
			return err
		}
		s.mu.Lock()
		s.grants = slices.Clone(document.Grants)
		s.mu.Unlock()
		s.logf("phonenode: config rev %d applied, %d grant(s)", document.Revision, len(document.Grants))
		return nil
	})
	if ctx.Err() != nil {
		return
	}
	s.logf("phonenode: the config channel ended: %v", err)
	s.channelEnded <- err
	endSession(s, err)
}

func (s *session) awaitReady(ctx context.Context) error {
	deadline := time.NewTimer(readyWithin)
	defer deadline.Stop()
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for !s.configs.Ready() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-s.channelEnded:
			return fmt.Errorf("the config channel ended before its first document: %w", err)
		case <-deadline.C:
			return fmt.Errorf("no config document within %s", readyWithin)
		case <-tick.C:
		}
	}
	return nil
}

func (s *session) shutdown() {
	s.cancel()
	s.closeNode()
}

// shutDown shuts down a node the caller counted in closing under mu, with mu
// not held, and then lets the next join go ahead.
func shutDown(s *session) {
	s.shutdown()
	mu.Lock()
	closing--
	mu.Unlock()
}

// endSession takes down a node whose config channel ended on its own — most
// often the mothership refusing its credential because the phone was
// removed — and leaves the reason for Status.
func endSession(s *session, err error) {
	mu.Lock()
	if current != s {
		mu.Unlock()
		return
	}
	current = nil
	closing++
	ended, endedCode = err.Error(), classify(err)
	mu.Unlock()
	closeDoors(reasonNodeDown)
	shutDown(s)
}

// classify says what the app does next about a failed join or an ended node.
func classify(err error) string {
	code, _ := exitcodes.Of(err)
	switch {
	case errors.Is(err, tsnode.ErrNoIdentity):
		return codeNotEnrolled
	case errors.Is(err, tsnode.ErrRejoinRefused), code == exitcodes.EnrollRefused:
		return codeRevoked
	case code == exitcodes.MaterialExpired:
		return codeExpired
	default:
		return codeJoinFailed
	}
}

// Status reports the node as it stands. It never waits on a join or on a
// node's shutdown: a node still closing reads down.
func Status() string {
	mu.Lock()
	s, isJoining, why, whyCode := current, joining != nil, ended, endedCode
	mu.Unlock()
	switch {
	case s != nil:
		return s.status()
	case isJoining:
		return answer(map[string]any{"state": "joining"})
	case why != "":
		return answer(map[string]any{"state": "down", "ended": why, "ended_code": whyCode})
	default:
		return answer(map[string]any{"state": "down"})
	}
}

func (s *session) status() string {
	s.mu.Lock()
	grants := append([]tsnode.ConfigGrant{}, s.grants...)
	s.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	seen, err := s.report(ctx)
	if err != nil {
		return answer(map[string]any{
			"state":     "up",
			"error":     err.Error(),
			"code":      codeStatusUnknown,
			"addresses": []string{},
			"grants":    grants,
			"node_id":   s.nodeID,
		})
	}

	addresses := strs(seen.Addresses)
	out := map[string]any{
		"state":     "up",
		"addresses": addresses,
		"grants":    grants,
		"name":      seen.Name,
		"node_id":   s.nodeID,
		"ready":     s.configs.Ready(),
		"revision":  s.configs.Revision(),
		"relayed":   seen.Relayed,
		"home_derp": seen.HomeDERP,
	}
	if len(addresses) > 0 {
		out["address"] = addresses[0]
	}
	return answer(out)
}

// Down closes the node, or calls off the join under way. The identity stays
// in dir for Rejoin. A join called off is still unwinding when Down returns,
// so a Join or Rejoin straight after may be answered joining: try again.
func Down() string {
	mu.Lock()
	s, callOff := current, joining
	current = nil
	if s != nil {
		closing++
	}
	ended, endedCode = "", ""
	mu.Unlock()

	if callOff != nil {
		callOff()
	}
	// Before the node: a door dialled over it has nothing left to speak on.
	closeDoors(reasonNodeDown)
	if s != nil {
		shutDown(s)
	}
	return answer(map[string]any{"ok": true})
}

func heartbeat(node *tsnode.Node, logf func(string, ...any)) func(context.Context) (report.Status, error) {
	return func(ctx context.Context) (report.Status, error) {
		seen, err := node.Status(ctx)
		if err != nil {
			return report.Status{}, err
		}
		if seen.ObservationsErr != nil {
			logf("phonenode: network observations incomplete: %v", seen.ObservationsErr)
		}
		return report.Status{
			NetworkReportVersion: 1,
			PeerPaths:            seen.PeerPaths,
			LocalAddresses:       seen.LocalAddresses,
			HomeDERP:             seen.HomeDERP,
			Endpoint:             seen.Endpoint,
			Relayed:              seen.Relayed,
			Addresses:            strs(seen.Addresses),
		}, nil
	}
}

func strs(addresses []netip.Addr) []string {
	out := make([]string, 0, len(addresses))
	for _, a := range addresses {
		out = append(out, a.String())
	}
	return out
}
