package serve

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/gentility-ai/gentility/connector/internal/config"
	"github.com/gentility-ai/gentility/connector/internal/egress"
	"github.com/gentility-ai/gentility/connector/internal/enroll"
	"github.com/gentility-ai/gentility/connector/internal/refusal"
	"github.com/gentility-ai/gentility/connector/internal/report"
	"github.com/gentility-ai/gentility/connector/internal/state"
	"github.com/gentility-ai/gentility/connector/internal/traffic"
	"github.com/gentility-ai/gentility/connector/internal/tsnode"
)

// Default is the real network: tsnet joins, a real exchange, and the two loops.
// log is where the whole of it narrates itself; nil is silence.
func Default(log func(format string, args ...any)) Deps {
	logf := log
	if logf == nil {
		logf = func(string, ...any) {}
	}

	return Deps{
		Join: func(ctx context.Context, env enroll.Env) (Session, error) {
			node, addresses, err := enroll.Join(ctx, env, overlay(logf))
			if err != nil {
				return Session{}, err
			}
			return Session{Node: node, Addresses: addresses, Close: node.Close}, nil
		},

		Exchange: func(ctx context.Context, env enroll.Env, store *state.Store, session Session) (state.Node, error) {
			// state.Node{} rather than the identity: there is none yet, so the
			// CA this first exchange pins is the one the enrollment carried.
			base, client, err := enroll.Mothership(env, state.Node{}, session.Node, session.Addresses)
			if err != nil {
				return state.Node{}, err
			}
			return enroll.Exchange(ctx, client, base, env, store)
		},

		Attend: func(ctx context.Context, opts Options, a Attendance) error {
			return attend(ctx, opts, a, logf)
		},
		Log: log,
	}
}

// overlay is where the library speaks: every magicsock, control and netmap
// line, prefixed so it stays tellable from the connector's own narration, and
// limited per format string — a burst of five, then about one every ten
// seconds — so a chatty state cannot fill the disk. The library exempts its
// own disco lines from that limit (its rateFree list), and magicsock's "now
// using <addr>" path change is one, so every path change passes, unlimited,
// and lands here with its time (the connector stamps the line). tsnet's
// UserLogf is the same logf, so its startup narration and a login-needed
// join's auth URL arrive prefixed too; the URL carries no key. Some library
// formats end in a newline, and the connector adds its own, so the format's is
// dropped: no blank lines.
func overlay(logf func(string, ...any)) func(string, ...any) {
	prefixed := func(format string, args ...any) {
		logf("overlay: "+strings.TrimSuffix(format, "\n"), args...)
	}
	return tsnode.RateLimit(prefixed, 10*time.Second, 5, 100)
}

// attend is the long-lived half: presence saying what this node is and offers,
// the config channel saying what it may serve, and the relay that serves it.
//
// It returns when ctx is cancelled or the config channel is refused fatally.
func attend(ctx context.Context, opts Options, a Attendance, logf func(string, ...any)) error {
	base, client, err := enroll.Mothership(a.Env, a.Identity, a.Session.Node, a.Session.Addresses)
	if err != nil {
		return err
	}

	treeContext, stopTree := context.WithCancel(ctx)
	trees := &treeAttendance{}
	defer func() { stopTree(); trees.wait() }()

	configs := config.New(base, a.Identity, a.Store, client)
	configs.Log = func(format string, args ...any) { logf(format, args...) }

	advertised := &advertisement{services: offers(opts)}

	// What `connector status` will read. The config loop fills in the document
	// half; the heartbeat is what keeps the whole of it fresh.
	documents := newLastDocument()

	presence := &report.Reporter{
		BaseURL:    base,
		Credential: a.Identity.NodeCredential,
		HTTP:       client,
		Status:     status(a, advertised, documents, logf),
		Ready:      configs.Ready,
		Log:        func(format string, args ...any) { logf(format, args...) },
	}
	go presence.Run(ctx)

	// What this node's filter refused, and what it had no path for, once a
	// minute (internal/refusal). Its own endpoint, not a heartbeat field.
	refused := &refusal.Reporter{BaseURL: base, Credential: a.Identity.NodeCredential, HTTP: client, Log: logf}
	go refused.Run(ctx)

	// Per-peer bytes, sampled once a second and sent in batches
	// (internal/traffic). Its own endpoint; the answer says how often.
	counted := &traffic.Reporter{
		BaseURL:    base,
		Credential: a.Identity.NodeCredential,
		HTTP:       client,
		Read:       a.Session.Node.PeerCounters,
		Ready:      configs.Ready,
		Log:        logf,
	}
	go counted.Run(ctx)

	relay := &relayGeneration{}
	defer relay.stop()

	admit := egress.NewAdmitClient(client, base, a.Identity.NodeCredential)

	var current *config.Document
	refreshDiscovery := func() {
		if current != nil {
			_ = trees.apply(treeContext, a, current, base, client, logf)
		}
	}
	configs.OnUnchanged = refreshDiscovery
	return configs.Run(ctx, func(document *config.Document) error {
		if err := document.Adopt(a.Session.Node, a.Store); err != nil {
			return err
		}

		if err := act(ctx, reconcile(document, relay.serving()), document, relay, advertised, a, admit, logf); err != nil {
			return err
		}

		// After the document has been acted on, so what a person reads is what
		// this node is doing and not what it was about to try.
		// A tree refusal cannot stop the connector's config or presence loops.
		current = document
		refreshDiscovery()
		documents.set(fromDocument(document))
		note(ctx, a, advertised, documents, logf)

		logf("config rev %d applied", document.Revision)
		return nil
	})
}

// offers is what this node advertises on every heartbeat: the egress relay, on
// a server, and nothing at all on a person's machine.
//
// The mothership's presence parser is all-or-nothing — one kind it will not
// accept and every heartbeat is refused — so nothing else goes in here, and
// stopAdvertising empties it. A person's machine (interface mode) runs no
// relay, and a user node may not declare one, so offering it got every one of
// its heartbeats answered 422.
func offers(opts Options) []report.Service {
	if opts.Interface {
		return nil
	}
	return []report.Service{{Kind: "egress", Port: opts.EgressPort}}
}

// act carries out one decision. Everything here is what reconcile decided, and
// nothing here decides anything.
func act(
	ctx context.Context,
	d decision,
	document *config.Document,
	relay *relayGeneration,
	advertised *advertisement,
	a Attendance,
	admit *egress.AdmitClient,
	logf func(string, ...any),
) error {
	if d.do != stopAdvertising {
		// Whatever this node offers, it goes on offering: a floor that rose and
		// fell again leaves a node that can advertise once more.
		advertised.restore()
	}

	switch d.do {
	case noop:
		return nil

	case stopAdvertising:
		advertised.clear()
		logf("this connector is below the mothership's capver floor: advertising nothing, serving on")
		return nil

	case stopRelay:
		relay.stop()
		logf("egress: no longer approved, relay stopped")
		return nil

	case restartRelay:
		// Drained before the new generation binds: a rebind that raced its own
		// teardown would find the address still held by the listener it is
		// replacing.
		relay.stop()
		fallthrough

	case startRelay:
		cfg := egress.NewConfig(document, a.Session.Node, admit)
		if err := relay.start(ctx, cfg, a.Session.Node, d.listeners); err != nil {
			return err
		}
		logf("egress: serving %s", bound(d.listeners))
		return nil

	default:
		return nil
	}
}

// advertisement is what presence says this node offers. The heartbeat loop
// reads it while the config loop writes it, so it is guarded.
type advertisement struct {
	mu       sync.Mutex
	services []report.Service
	cleared  bool
}

func (s *advertisement) clear() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cleared = true
}

func (s *advertisement) restore() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cleared = false
}

func (s *advertisement) offered() []report.Service {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cleared {
		return nil
	}
	return s.services
}

// relayGeneration is the relay this node currently runs, and the context that
// ends it.
//
// A generation rather than a relay: a rebind is a stop and a start, because
// each listener is bound to one port on one address and there is nothing to
// move — and the generation is the whole set at once, because that is what
// egress.StartServices binds and what reconcile compares. Its own context,
// derived from the process's, is what lets a service be withdrawn without
// taking the node down with it.
type relayGeneration struct {
	cancel    context.CancelFunc
	relays    []*egress.Relay
	listeners []listener
}

func (g *relayGeneration) serving() serving {
	return serving{up: g.cancel != nil, listeners: g.listeners}
}

// start binds the generation and records the set it was bound with — the set
// the decision compared, which is the same set StartServices reads back out of
// the same document, so the next document is compared against what is actually
// listening.
func (g *relayGeneration) start(ctx context.Context, cfg egress.Config, node *tsnode.Node, listeners []listener) error {
	ctx, cancel := context.WithCancel(ctx)

	relays, err := egress.StartServices(ctx, cfg, egress.OverlayListeners{Node: node})
	if err != nil {
		cancel()
		return err
	}

	g.cancel, g.relays, g.listeners = cancel, relays, listeners
	return nil
}

// stop closes the generation's listeners and drains the close reports of any
// tunnel still open, so a byte count is not lost to a rebind or to exit.
func (g *relayGeneration) stop() {
	if g.cancel == nil {
		return
	}

	g.cancel()
	for _, relay := range g.relays {
		relay.WaitIdle()
	}
	g.cancel, g.relays, g.listeners = nil, nil, nil
}

// status is what the heartbeat says: this node's own path to the overlay, and
// what it offers.
//
// It is also where the status snapshot gets its freshness. The beat is the one
// thing here that happens on a clock — a config document arrives only when
// something in the organization changes — so a snapshot written by applies
// alone would read as stale on a node that is perfectly well. A snapshot that
// could not be written is a line in the log and never a skipped beat: presence
// is what makes this node visible, and it does not depend on a local file.
func status(
	a Attendance,
	advertised *advertisement,
	documents *lastDocument,
	logf func(string, ...any),
) func(context.Context) (report.Status, error) {
	return func(ctx context.Context) (report.Status, error) {
		seen, err := a.Session.Node.DiagnosticStatus(ctx)
		if err != nil {
			return report.Status{}, err
		}
		if seen.ObservationsErr != nil {
			logf("network observations incomplete: %v", seen.ObservationsErr)
		}

		offered := advertised.offered()

		if err := record(a.Store, documents.get(), seen, offered, time.Now()); err != nil {
			logf("could not write the status snapshot: %v", err)
		}

		addresses := make([]string, 0, len(seen.Addresses))
		for _, address := range seen.Addresses {
			addresses = append(addresses, address.String())
		}

		return report.Status{
			NetworkReportVersion: 1,
			PeerPaths:            seen.PeerPaths,
			LocalAddresses:       seen.LocalAddresses,
			HomeDERP:             seen.HomeDERP,
			Endpoint:             seen.Endpoint,
			Relayed:              seen.Relayed,
			Addresses:            addresses,
			Services:             offered,
		}, nil
	}
}
