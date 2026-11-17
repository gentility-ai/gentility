package gateway

import (
	"context"
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gentility-ai/gentility/connector/internal/config"
	"github.com/gentility-ai/gentility/connector/internal/tsnode"
)

// The lease API is the app's ONE way onto the overlay.
//
// The app holds no overlay address of its own — this gateway is the node — so
// to reach a service on a customer's network it asks here for a lease: a
// listener on the deploy's docker network that accepts exactly ONE TCP
// connection, only from the address the asking came from, and is gone on that
// accept or at expiry, whichever is first. The gateway dials the service's
// overlay address with its own source address in THAT service's organization
// pinned, and copies bytes both ways.
//
// One connection per lease is tidiness; what authenticates the caller is the
// bearer, and what binds the lease to it is the source address. A caller
// wanting a second connection asks for a second lease, which is what "one
// lease per physical connection attempt" means operationally. **There is no
// renew and no release** — either would be a way for the app to hold a
// listener open, which is the property the design gave up on purpose.
//
// The lease API accepts a signed bearer and a service id:
//
//	POST /lease
//	authorization: Bearer <base64url(payload).base64url(mac)>
//	{"node_service_id": "<uuid>"}
//
//	200 {"host": "<docker-network host>", "port": <int>, "expires_in": <seconds>}
//	401  the bearer is not one this gateway accepts, for any reason at all
//	403  a service the config document carries that this gateway may not reach
//	404  a service id the config document does not carry
//	429  all 64 lease slots are in use (capacity)
//	500  the ephemeral lease listener could not be bound (fd/port exhaustion)
//	503  no config document yet
//
// **The refusals carry a status and nothing else.** A body is something a
// caller could steer, and an error that echoed the request would be an error
// something could fill with the request's own Authorization header — which is
// how a secret reaches a log without anybody writing a line that logs one.
const (
	// LeasePath is the one route. It is a constant because the app's client
	// hard-codes the same string and two spellings of one path is one path.
	LeasePath = "/lease"

	// leaseTTL is how long a lease's listener lives, used or not.
	//
	// Ten seconds is the caller's whole budget to dial: the app and this
	// container are one hop apart on a docker network, so anything a caller
	// cannot do in ten seconds is a caller that has stopped, and a listener
	// left for a caller that has stopped is a listener nobody is coming back
	// for.
	leaseTTL = 10 * time.Second

	// leaseWindow is how far either side of now a bearer may have been minted,
	// matching the lease token format's own sixty seconds.
	// Symmetric, because the app and the gateway are two containers with two
	// clocks and one running a second fast is not an attacker.
	leaseWindow = 60 * time.Second

	// leaseBodyLimit caps the request. The body is one uuid in one field, so
	// four kilobytes is already three orders of magnitude of headroom, and a
	// limit is what stops an authenticated-looking caller streaming a body at
	// a decoder before its bearer has been read.
	leaseBodyLimit = 4 << 10

	// leaseSlots bounds how many leases may be outstanding at once.
	//
	// Each one is a bound socket and a goroutine, and a lease is held for as
	// long as its connection lives, so this is the ceiling on what one gateway
	// will carry for the app. Above it the answer is 429, which is the answer
	// that means "ask again": a caller that retries in a second finds a slot,
	// and the alternative — an unbounded map of listeners driven by an
	// authenticated caller — is a caller that can exhaust this process's file
	// descriptors.
	leaseSlots = 64

	// leaseDialTimeout bounds the onward dial onto the overlay. The netstack
	// answers or it does not; an org whose packet filter drops the SYN is an
	// eight-second timeout, and this is a little above it so the refusal that
	// arrives is the real one.
	leaseDialTimeout = 10 * time.Second
)

// The lease bearer's key derivation: `Plug.Crypto.KeyGenerator.generate/2`'s
// defaults again — PBKDF2-HMAC-SHA256, 1000 iterations, 32 octets — under this
// protocol's own salt.
//
// The salt is the domain separation, and it is the whole difference between
// this and the cert bearer above. That one authenticates the gateway to the
// app; this authenticates the app to the gateway. Same secret, same
// derivation, opposite directions — so a bearer captured on one hop is not a
// message on the other, and `TestLeaseRefusesTheCertBearer` is the pin.
const (
	leaseSalt    = "overlay gateway lease v1"
	leaseSubject = "lease"
)

// The three refusals a Directory may answer with, and the three statuses they
// become. They are different operator problems: a 404 is a stale id on the
// app's side, a 403 is a grant missing on the head's, and a 503 is this
// gateway not yet knowing enough to say which.
var (
	// ErrNoDocument is a gateway that is serving but holds no config document
	// yet. It has no authority to answer either of the other two questions,
	// and a retry is the right response to it — which is not true of the
	// others.
	ErrNoDocument = errors.New("gateway: this node holds no config document yet")

	// ErrUnknownService is an id the config document does not carry.
	ErrUnknownService = errors.New("gateway: no such node service in this node's config document")

	// ErrNotReachable is an id the config document carries that this gateway
	// may not reach — not approved, no address, or no grant in its
	// organization.
	ErrNotReachable = errors.New("gateway: this gateway may not reach that node service")
)

// Target is where one lease's connection goes, and the address it must leave
// from.
//
// Source is not a convenience. A tsnet dial with no source pinned leaves from
// this node's primary address, and this gateway holds a grant in EVERY
// organization — so an unpinned dial reaches at most one of them and is
// silently dropped by every other organization's packet filter.
type Target struct {
	Org    string
	Source netip.Addr
	Dest   netip.AddrPort
}

// Directory answers where a node service is, for this gateway.
//
// An interface because the answer comes from the config document in
// production and from a table in the tests, and because the lease API's job —
// authenticate, resolve, bind, pipe — should be provable without a head, a
// netstack and a rendered document.
type Directory interface {
	Lookup(nodeServiceID string) (Target, error)
}

// Dialer opens one connection to dst from src. `(*tsnode.Node).DialBound` is
// it, and taking a func rather than the node is what lets every test below run
// without linking a tailnet.
type Dialer func(ctx context.Context, src netip.Addr, dst netip.AddrPort) (net.Conn, error)

// Services is the Directory backed by the leasable services from the config
// document this gateway last applied, and the grants that came with it.
//
// The renderer emits two separate lists: `services` (this node's own listeners)
// and `leasable` (every approved service on the far end of a live path the
// gateway holds as grantee). This directory resolves against `leasable` and
// only `leasable`. A service present only in `services` (e.g. the gateway's own
// listener) is not leasable.
type Services struct {
	mu       sync.RWMutex
	held     bool
	leasable []config.Service
	grants   []tsnode.Grant
}

// Apply records the leasable services from the document this gateway is now
// serving under.
//
// It takes the document rather than a list so it is the one place that picks
// `leasable` out of it: handed `services`, the gateway once leased only its
// own listeners and refused every service it was there to reach.
//
// It takes the grants already parsed, because the caller has to parse them
// anyway to hand them to the node — and a second parse would be a second
// chance for the directory and the dialler to disagree about which
// organizations this gateway is in.
func (s *Services) Apply(document *config.Document, grants []tsnode.Grant) {
	// Cloned, both of them: the caller keeps its slices, and a directory that
	// can be rewritten from outside the lock is a lease resolution that races
	// with whoever still holds the document.
	cloned := slices.Clone(document.Leasable)
	granted := slices.Clone(grants)

	s.mu.Lock()
	defer s.mu.Unlock()
	s.held = true
	s.leasable = cloned
	s.grants = granted
}

// Lookup resolves a node service id to the one connection a lease would make.
func (s *Services) Lookup(nodeServiceID string) (Target, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if !s.held {
		return Target{}, ErrNoDocument
	}

	for _, service := range s.leasable {
		if service.ID != nodeServiceID {
			continue
		}

		// Declared, or withdrawn, or anything else the mothership's vocabulary
		// grows: a service is leasable while it is approved and never
		// otherwise. The state is the mothership's word and this does not
		// second-guess it.
		if service.State != "approved" {
			return Target{}, fmt.Errorf("%w: it is %s, not approved", ErrNotReachable, service.State)
		}

		return s.target(service)
	}

	return Target{}, ErrUnknownService
}

// target is the address half: where the service listens, and which of this
// gateway's own addresses may dial it.
//
// The organization is decided by the destination's /64 and not by the
// document's `org` label — `tsnode.SourceFor` is the authority on that, and it
// is the same function every other outbound connection in this binary goes
// through. The label is carried onto the Target for diagnostics only.
func (s *Services) target(service config.Service) (Target, error) {
	address, err := netip.ParseAddr(service.Address)
	if err != nil {
		return Target{}, fmt.Errorf("%w: it carries no overlay address", ErrNotReachable)
	}

	if service.Port <= 0 || service.Port > 65535 {
		return Target{}, fmt.Errorf("%w: %d is not a port", ErrNotReachable, service.Port)
	}

	source, err := tsnode.SourceFor(s.grants, address)
	if err != nil {
		return Target{}, fmt.Errorf("%w: %s", ErrNotReachable, err)
	}

	return Target{
		Org:    service.Org,
		Source: source.Address,
		Dest:   netip.AddrPortFrom(address, uint16(service.Port)),
	}, nil
}

// Lease is the gateway's outbound door.
//
// One instance serves the whole app: what differs per request is the service
// asked for and the address the asking came from, both of which arrive on the
// request.
type Lease struct {
	// Now is the clock the bearer's window is judged against. Exported for the
	// tests, which would otherwise have to wait out a minute to prove the
	// window closes.
	//
	// A lease listener's own life is real time and is deliberately not read
	// from here: a socket deadline is a fact about a socket, and a test that
	// moved this clock backwards would otherwise be asking the kernel for a
	// deadline in the past.
	Now func() time.Time

	// TTL is how long a lease's listener lives. Exported so a test need not
	// spend ten seconds proving that it expires.
	TTL time.Duration

	secrets   [][]byte
	directory Directory
	dial      Dialer
	logf      func(string, ...any)
	slots     chan struct{}
	mux       *http.ServeMux
}

// NewLease builds the door.
//
// secrets is `[current, previous]` — one entry today, because the gateway
// holds a single OVERLAY_INGRESS_SECRET, and a slice because the app's side is
// already a list and a rotation is a window in which the two containers hold
// different current secrets. Every entry is tried and the walk does not stop
// early; see leaseMACIsOurs.
//
// logf is where a refusal is said out loud. Nil is silence, which is what a
// test wants. It is never given a secret, a bearer or a body to print.
func NewLease(secrets [][]byte, directory Directory, dial Dialer, logf func(string, ...any)) *Lease {
	if logf == nil {
		logf = func(string, ...any) {}
	}

	lease := &Lease{
		Now:       time.Now,
		TTL:       leaseTTL,
		secrets:   secrets,
		directory: directory,
		dial:      dial,
		logf:      logf,
		slots:     make(chan struct{}, leaseSlots),
	}

	// A mux rather than a hand-rolled check, so a wrong method is a 405 and a
	// wrong path is a 404 without this handler having an opinion about either.
	lease.mux = http.NewServeMux()
	lease.mux.HandleFunc(http.MethodPost+" "+LeasePath, lease.handle)

	return lease
}

// ServeHTTP makes a Lease the handler `Serve` runs.
func (l *Lease) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	l.mux.ServeHTTP(writer, request)
}

// leaseAsk is the request body: one field, and nothing else.
//
// Nothing else on purpose. The gateway resolves the organization, the address
// and the source address to pin from its own config document, so a field
// naming any of them would be a field a bearer-holder could steer.
type leaseAsk struct {
	NodeServiceID string `json:"node_service_id"`
}

// leaseGranted is the answer: where to dial on the docker network, and how
// long it will be there.
type leaseGranted struct {
	Host      string `json:"host"`
	Port      int    `json:"port"`
	ExpiresIn int    `json:"expires_in"`
}

// handle is one lease, from the bearer to the bound socket.
func (l *Lease) handle(writer http.ResponseWriter, request *http.Request) {
	asked, ok := leaseAsked(request)
	if !ok {
		// A 401 and not a 400. Without an id there is nothing to check the
		// bearer's own id against, so this is a bearer that has not been shown
		// to authorize anything — which is the same refusal, and telling an
		// unauthenticated caller which of the two it got wrong tells it
		// something about what this gateway holds.
		l.refuse(writer, http.StatusUnauthorized, "the request did not name one node service")
		return
	}

	if err := VerifyLeaseBearer(l.secrets, bearerOf(request), asked, l.Now()); err != nil {
		l.refuse(writer, http.StatusUnauthorized, "the bearer is not one this gateway accepts")
		return
	}

	target, err := l.directory.Lookup(asked)
	switch {
	case errors.Is(err, ErrNoDocument):
		l.refuse(writer, http.StatusServiceUnavailable, "no config document yet")
		return
	case errors.Is(err, ErrUnknownService):
		l.refuse(writer, http.StatusNotFound, "no such node service here")
		return
	case errors.Is(err, ErrNotReachable):
		l.refuse(writer, http.StatusForbidden, "that node service is not reachable from here")
		return
	case err != nil:
		// A directory that failed some other way has not said this lease is
		// allowed, and an unrecognised refusal is not a reason to grant one.
		l.refuse(writer, http.StatusServiceUnavailable, "the directory could not answer")
		return
	}

	requester, err := netip.ParseAddrPort(request.RemoteAddr)
	if err != nil {
		l.refuse(writer, http.StatusServiceUnavailable, "this request has no source address to bind the lease to")
		return
	}

	// The lease is bound on the address this request actually reached, which
	// is by construction an address the caller can dial: its own packet
	// arrived there. That is why the accessory may bind the API on 0.0.0.0 —
	// a container learns its docker address at runtime and cannot name it in
	// an env file — without the answer ever being a wildcard.
	local, err := localAddr(request)
	if err != nil {
		l.refuse(writer, http.StatusServiceUnavailable, "this request carries no local address to bind the lease on")
		return
	}

	select {
	case l.slots <- struct{}{}:
	default:
		l.refuse(writer, http.StatusTooManyRequests, "too many leases are outstanding")
		return
	}

	listener, err := net.Listen("tcp", netip.AddrPortFrom(local, 0).String())
	if err != nil {
		<-l.slots
		l.logf("gateway: binding a lease listener on %s: %v", local, err)
		l.refuse(writer, http.StatusInternalServerError, "the lease listener could not be bound")
		return
	}

	bound, err := netip.ParseAddrPort(listener.Addr().String())
	if err != nil {
		_ = listener.Close()
		<-l.slots
		l.refuse(writer, http.StatusServiceUnavailable, "the lease listener bound no address")
		return
	}

	// WithoutCancel because the request's context is cancelled the moment this
	// response is written, and the listener has to outlive the answer that
	// names it. What bounds it instead is its expiry, which is the whole of a
	// lease's life anyway.
	//
	// The slot is taken here and given back here, so the pair is one thing
	// read in one place — and it is given back only when the lease is really
	// over, the piped connection included: what leaseSlots bounds is what this
	// gateway is carrying, and a connection still open is still being carried.
	go func() {
		defer func() { <-l.slots }()
		l.hold(context.WithoutCancel(request.Context()), listener, requester.Addr(), target, time.Now().Add(l.TTL))
	}()

	writer.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(writer).Encode(leaseGranted{
		Host:      bound.Addr().String(),
		Port:      int(bound.Port()),
		ExpiresIn: int(l.TTL / time.Second),
	}); err != nil {
		// The lease is already running and will expire on its own. There is
		// nothing to take back and nobody left to tell.
		l.logf("gateway: answering a lease on %s: %v", bound, err)
	}
}

// hold runs one lease: the requester's single connection, or nothing.
//
// The listener is closed on the first connection from the requester's own
// address, and at expiry whether one arrived or not. A connection from any
// other address is closed and the listener goes on waiting — the source
// binding is a boundary, not a race that a stranger dialling first should be
// able to win.
//
// The deadline is the only thing that ends an unused lease, and it is enough:
// ctx cannot be cancelled — it is the request's, detached, because the request
// is over before the lease begins — so what bounds this is the expiry the
// caller was told about and, at the outside, the process exiting. Ten seconds
// is short enough that "at the outside" never matters.
func (l *Lease) hold(ctx context.Context, listener net.Listener, requester netip.Addr, target Target, expiry time.Time) {
	defer listener.Close()

	if deadlined, ok := listener.(interface{ SetDeadline(time.Time) error }); ok {
		_ = deadlined.SetDeadline(expiry)
	} else {
		// The deadline is load-bearing: it is the only thing that ends an unused
		// lease. If the type assertion fails, the slot would leak forever.
		l.logf("gateway: lease listener does not support SetDeadline, closing immediately")
		return
	}

	var refusedCount int

	for {
		conn, err := listener.Accept()
		if err != nil {
			// The deadline, or the process on its way out. Either way this
			// lease was never used, which is ordinary: a caller may give up.
			if refusedCount > 0 {
				l.logf("gateway: a lease bound to %s refused %d connection(s) from other sources", requester, refusedCount)
			}
			return
		}

		from, err := netip.ParseAddrPort(conn.RemoteAddr().String())
		if err != nil || from.Addr().Unmap() != requester.Unmap() {
			refusedCount++
			_ = conn.Close()
			continue
		}

		_ = listener.Close()
		if refusedCount > 0 {
			l.logf("gateway: a lease bound to %s refused %d connection(s) from other sources", requester, refusedCount)
		}
		l.forward(ctx, conn, target)

		return
	}
}

// forward is the lease's one connection: dial the service on the overlay from
// this gateway's address in that service's organization, and copy.
func (l *Lease) forward(ctx context.Context, client net.Conn, target Target) {
	defer client.Close()

	// Its own deadline, and not the process's: a dial either answers or is
	// dropped by a packet filter, and inheriting a context that lives as long
	// as the gateway would make the second case a goroutine that never ends.
	dialCtx, cancel := context.WithTimeout(ctx, leaseDialTimeout)
	defer cancel()

	upstream, err := l.dial(dialCtx, target.Source, target.Dest)
	if err != nil {
		l.logf("gateway: a lease to %s in %s could not be dialled: %v", target.Dest, target.Org, err)
		return
	}
	defer upstream.Close()

	pipe(client, upstream)
}

// pipe copies both ways until both ends are done.
//
// Each direction closes only its own write half when its source reaches EOF,
// so a protocol that half-closes — which is most of them — is not cut short in
// the direction that is still talking. Nothing here reads what it copies.
func pipe(a, b net.Conn) {
	var wg sync.WaitGroup
	wg.Add(2)

	copyOneWay := func(dst, src net.Conn) {
		defer wg.Done()
		_, _ = io.Copy(dst, src)
		if half, ok := dst.(interface{ CloseWrite() error }); ok {
			_ = half.CloseWrite()
			return
		}
		_ = dst.Close()
	}

	go copyOneWay(a, b)
	go copyOneWay(b, a)

	wg.Wait()
}

// refuse answers with a status and nothing else.
//
// why is a fixed string chosen at the call site and reaches the log alone. It
// is never the request, never the bearer, and never anything an upstream could
// have filled.
func (l *Lease) refuse(writer http.ResponseWriter, status int, why string) {
	l.logf("gateway: lease refused %d: %s", status, why)
	writer.WriteHeader(status)
}

// leaseAsked reads the one field the body carries.
//
// Unknown fields are refused rather than ignored: the contract is that this
// body names a service and nothing else, and a decoder that skipped what it
// did not recognise would be a decoder that could not tell a future field from
// a caller trying one.
func leaseAsked(request *http.Request) (string, bool) {
	decoder := json.NewDecoder(io.LimitReader(request.Body, leaseBodyLimit))
	decoder.DisallowUnknownFields()

	var asked leaseAsk
	if err := decoder.Decode(&asked); err != nil {
		return "", false
	}
	if asked.NodeServiceID == "" {
		return "", false
	}

	return asked.NodeServiceID, true
}

// bearerOf is the token, or the empty string. An absent header and a malformed
// one are the same thing here: neither is a bearer, and VerifyLeaseBearer has
// one refusal anyway.
func bearerOf(request *http.Request) string {
	const prefix = "Bearer "

	header := request.Header.Get("Authorization")
	if len(header) < len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return ""
	}

	return header[len(prefix):]
}

// ErrInvalidLeaseBearer is the one refusal VerifyLeaseBearer has.
//
// One, for the reason the lease token verifier has one:
// a caller that cannot produce the MAC learns nothing from being told which
// way it failed, and telling it would say which of a rotation's two secrets
// was tried, or which of two service ids this deployment happens to hold.
var ErrInvalidLeaseBearer = errors.New("gateway: the lease bearer is not one this gateway accepts")

// VerifyLeaseBearer is the gateway's half of
// the lease token format, byte for byte:
//
//	payload = "lease:" ++ <unix seconds> ++ ":" ++ <node_service_id>
//	key     = PBKDF2-HMAC-SHA256(secret, "overlay gateway lease v1", 1000, 32)
//	bearer  = base64url(payload) ++ "." ++ base64url(HMAC-SHA256(key, payload))
//
// Both halves unpadded and url-safe, joined by one dot.
//
// **The payload names the service, and it must be the service the body asked
// for.** A bearer is spendable on exactly the request it was minted for, so a
// bearer lifted from anywhere — a log line, a proxy, a future hop less private
// than one plaintext docker hop — is a lease to that one service rather than
// to every service this gateway would have leased to the app.
//
// now is the clock the ±60 s window is judged against.
func VerifyLeaseBearer(secrets [][]byte, token, nodeServiceID string, now time.Time) error {
	encodedPayload, encodedMAC, split := strings.Cut(token, ".")
	if !split {
		return ErrInvalidLeaseBearer
	}

	payload, err := base64.RawURLEncoding.DecodeString(encodedPayload)
	if err != nil {
		return ErrInvalidLeaseBearer
	}

	presented, err := base64.RawURLEncoding.DecodeString(encodedMAC)
	if err != nil {
		return ErrInvalidLeaseBearer
	}

	// Verify the MAC first: what the payload says is
	// only worth reading once this deployment is known to have written it.
	if !leaseMACIsOurs(secrets, payload, presented) {
		return ErrInvalidLeaseBearer
	}

	subject, rest, split := strings.Cut(string(payload), ":")
	if !split || subject != leaseSubject {
		return ErrInvalidLeaseBearer
	}

	mintedAt, id, split := strings.Cut(rest, ":")
	if !split {
		return ErrInvalidLeaseBearer
	}

	if subtle.ConstantTimeCompare([]byte(id), []byte(nodeServiceID)) != 1 {
		return ErrInvalidLeaseBearer
	}

	seconds, err := strconv.ParseInt(mintedAt, 10, 64)
	if err != nil {
		return ErrInvalidLeaseBearer
	}

	drift := now.Sub(time.Unix(seconds, 0))
	if drift < 0 {
		drift = -drift
	}
	if drift > leaseWindow {
		return ErrInvalidLeaseBearer
	}

	return nil
}

// leaseMACIsOurs asks every configured secret, and asks all of them.
//
// Reduced rather than returned early, for the reason
// the peer-signature verifier walks its own list to the end: a
// short-circuit would answer sooner for the current secret than for the
// previous one, which during a rotation is a timing signal about which of the
// two a bearer was minted with.
func leaseMACIsOurs(secrets [][]byte, payload, presented []byte) bool {
	ours := false

	for _, secret := range secrets {
		key, err := pbkdf2.Key(sha256.New, string(secret), []byte(leaseSalt), bearerIterations, bearerKeyLength)
		if err != nil {
			continue
		}

		mac := hmac.New(sha256.New, key)
		mac.Write(payload)
		ours = hmac.Equal(presented, mac.Sum(nil)) || ours
	}

	return ours
}
