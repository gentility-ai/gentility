package egress

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// PeerResolver attributes an inbound overlay connection to the address that
// opened it. In production this is tsnet's WhoIs, behind internal/tsnode; in
// tests it is a struct literal. It is the ONLY thing the relay trusts about
// who is calling — never a header, never the CONNECT body.
//
// WhoIs MUST return the overlay address the connection actually came from,
// having verified the head attributes that address to the peer — NOT a
// canonical or primary address. A node can hold addresses in more than one
// organization; returning a different address the peer also holds would
// misattribute the connection to the wrong organization's egress policy, since
// the returned address is what feeds Admit and Admit judges it against that
// org's allowlist. A node holding [addrA(orgA), addrB(orgB)] and connecting
// from addrA must be attributed as addrA: a loose WhoIs answering addrB would
// have the connection judged against orgB's policy.
//
// The implementation mirrors internal/gateway's Attributor
// (internal/gateway/peer.go): verify remote.Addr() is in the peer's attributed
// addresses, and return that same remote.Addr() — never a substitute. A
// zero/invalid Addr is treated as an unattributable peer and refused.
type PeerResolver interface {
	WhoIs(ctx context.Context, remote net.Addr) (netip.Addr, error)
}

// Relay is the `egress` service: a CONNECT proxy on the node's overlay address.
//
// Everything tsnet-shaped is behind a field so the relay tests with httptest
// and ordinary sockets (tsnet is imported only by internal/tsnode):
// Peers is the attribution, Admit is the synchronous question to Gentility,
// Lookup resolves the destination hostname, Hygiene refuses a resolved address
// out of bounds, and Dial opens the pinned outbound connection. wiring.go
// supplies the tsnet implementations of Peers, Lookup and Dial.
type Relay struct {
	Peers   PeerResolver
	Admit   *AdmitClient
	Dial    func(ctx context.Context, network, addr string) (net.Conn, error)
	Lookup  func(ctx context.Context, host string) ([]netip.Addr, error)
	Hygiene func(addrs []netip.Addr) error
	// LiteralAllow are the org's allowlist entries that are IP literals; an
	// IP-literal CONNECT target is refused unless it is one of these. It is
	// sourced from the config document's egress.literal_allow by the service
	// wiring — the relay itself caches no allowlist.
	LiteralAllow []string

	// HeaderDeadline bounds how long an accepted connection may take to send its
	// CONNECT request; zero means defaultHeaderDeadline. Like Hygiene it exists
	// so the test can drive the path in milliseconds — nothing configures it.
	HeaderDeadline time.Duration

	wg sync.WaitGroup

	// mu guards the two connection sets and the shutdown flag.
	//
	// accepted holds every connection the accept loop has handed to handle,
	// from the moment it is accepted until handle returns. A connection blocked
	// in http.ReadRequest is already counted in wg but is not yet a tunnel, so
	// without this set Shutdown would not close it and WaitIdle would never
	// return: a peer that connects and sends nothing would stall the drain, and
	// with it serve's whole reconcile.
	//
	// conns holds the client and upstream conns of every established tunnel so
	// Shutdown can force-close them: an idle-but-open tunnel has no other
	// interruption point, its copy loops block until one of these is closed.
	mu       sync.Mutex
	accepted map[net.Conn]struct{}
	conns    map[net.Conn]struct{}
	shutdown bool
}

// defaultHeaderDeadline bounds header reading. Generous — an honest client
// writes its CONNECT the moment the overlay connection is up, even over a
// relayed path — but finite, so a peer that connects and then says nothing is
// dropped rather than holding a goroutine for as long as it likes.
const defaultHeaderDeadline = 10 * time.Second

// WaitIdle blocks until every in-flight tunnel has reported its close. It drains
// reports; it does not interrupt them. An open-but-idle tunnel never ends on its
// own — neither io.Copy returns — so on its own WaitIdle would block forever on
// such a tunnel. It completes only once something has force-closed the in-flight
// tunnels: call Shutdown first (or close the listener and then Shutdown), then
// WaitIdle to drain the close reports. `connector` uses it on shutdown so a
// report is not lost to exit.
func (r *Relay) WaitIdle() { r.wg.Wait() }

// Shutdown force-closes every connection the relay has accepted — established
// tunnels and connections still reading their request headers alike — so their
// blocking reads return, any close report fires, and WaitIdle can drain.
// Without it a single open tunnel (neither direction ever reaching EOF), or a
// single peer that connects and says nothing, would block WaitIdle forever:
// those reads have no other interruption point, and the connector owns no
// handle on the individual conns, only the relay does. After Shutdown no new
// tunnel is admitted. ctx bounds the wait for the outstanding close reports.
func (r *Relay) Shutdown(ctx context.Context) error {
	r.mu.Lock()
	r.shutdown = true
	for c := range r.accepted {
		_ = c.Close()
	}
	for c := range r.conns {
		_ = c.Close()
	}
	r.mu.Unlock()

	done := make(chan struct{})
	go func() {
		r.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// accept registers a freshly accepted connection so Shutdown can close it while
// it is still reading headers — before there is a tunnel for track to know
// about. If Shutdown has already swept the set the connection is closed at
// once, so it is never left open past a shutdown that will not come round again.
func (r *Relay) accept(c net.Conn) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.accepted == nil {
		r.accepted = make(map[net.Conn]struct{})
	}
	r.accepted[c] = struct{}{}
	if r.shutdown {
		_ = c.Close()
	}
}

func (r *Relay) unaccept(c net.Conn) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.accepted, c)
}

// track registers a tunnel's conns so Shutdown can force-close them. It returns
// false if Shutdown has already begun, in which case the caller closes the
// conns itself rather than leaving them open past a Shutdown that has already
// swept the set.
func (r *Relay) track(conns ...net.Conn) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.conns == nil {
		r.conns = make(map[net.Conn]struct{})
	}
	for _, c := range conns {
		r.conns[c] = struct{}{}
	}
	return !r.shutdown
}

func (r *Relay) untrack(conns ...net.Conn) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, c := range conns {
		delete(r.conns, c)
	}
}

func (r *Relay) Serve(ln net.Listener) error {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return err
		}
		// Counted, then registered: whatever Shutdown finds in the set, WaitIdle
		// is already waiting for.
		r.wg.Add(1)
		r.accept(conn)
		go func() {
			defer r.wg.Done()
			defer r.unaccept(conn)
			r.handle(conn)
		}()
	}
}

func (r *Relay) handle(client net.Conn) {
	defer client.Close()
	ctx := context.Background()

	// The request must arrive within the header deadline; a peer that connects
	// and says nothing is dropped instead of held. The deadline is lifted the
	// moment the request is in hand — a tunnel's own reads are unbounded by
	// design, and everything past this point is on the relay's clock, not the
	// peer's.
	deadline := r.HeaderDeadline
	if deadline == 0 {
		deadline = defaultHeaderDeadline
	}
	_ = client.SetReadDeadline(time.Now().Add(deadline))

	br := bufio.NewReader(client)
	req, err := http.ReadRequest(br)
	if err != nil {
		return
	}
	_ = client.SetReadDeadline(time.Time{})
	if req.Method != http.MethodConnect {
		writeStatus(client, http.StatusMethodNotAllowed, "only CONNECT")
		return
	}

	host, portStr, err := net.SplitHostPort(req.Host)
	if err != nil {
		writeStatus(client, http.StatusBadRequest, "bad target")
		return
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		writeStatus(client, http.StatusBadRequest, "bad port")
		return
	}

	// An IP literal is refused before anything else is asked, unless the org
	// allowlisted that exact literal, so a bare literal never even reaches the
	// mothership as a question.
	if err := CheckTarget(host, r.LiteralAllow); err != nil {
		writeStatus(client, http.StatusForbidden, err.Error())
		return
	}

	src, err := r.Peers.WhoIs(ctx, client.RemoteAddr())
	// Defence in depth: even a buggy WhoIs that answers no error must not feed a
	// zero/invalid address to Admit, where it would be judged against — and
	// misattributed to — an organization's egress policy. An invalid src is an
	// unattributable peer, refused before any question is asked.
	if err != nil || !src.IsValid() {
		writeStatus(client, http.StatusForbidden, "unattributable peer")
		return
	}

	// Authoritative, every time, with no cached answer to fall back on.
	decision, err := r.Admit.Admit(ctx, src, host, port)
	if err != nil {
		writeStatus(client, http.StatusBadGateway, "admit unavailable")
		return
	}
	if !decision.Admitted {
		writeStatus(client, http.StatusForbidden, "refused: "+decision.Reason)
		return
	}

	// Resolved ONCE, and the addresses we checked are the addresses we dial —
	// there is no second lookup for the transport to disagree with.
	addrs, err := r.Lookup(ctx, host)
	if err != nil {
		writeStatus(client, http.StatusBadGateway, "resolution failed")
		return
	}
	hygiene := r.Hygiene
	if hygiene == nil {
		hygiene = CheckResolved
	}
	if err := hygiene(addrs); err != nil {
		writeStatus(client, http.StatusForbidden, err.Error())
		_ = r.Admit.Close(ctx, decision.FactID, 0, 0, "hygiene_refused")
		return
	}

	upstream, err := r.dialPinned(ctx, addrs, port)
	if err != nil {
		writeStatus(client, http.StatusBadGateway, "upstream unreachable")
		_ = r.Admit.Close(ctx, decision.FactID, 0, 0, "upstream_unreachable")
		return
	}
	defer upstream.Close()

	writeStatus(client, http.StatusOK, "")

	// From here the tunnel is established and its copy loops block until a conn
	// closes. Register both conns so Shutdown can force-close them; if Shutdown
	// already began, close them now so the loops return at once.
	if !r.track(client, upstream) {
		_ = client.Close()
		_ = upstream.Close()
	}
	defer r.untrack(client, upstream)

	var toHost, fromHost int64
	done := make(chan struct{}, 2)

	go func() {
		n, _ := io.Copy(upstream, br)
		atomic.AddInt64(&toHost, n)
		done <- struct{}{}
	}()
	go func() {
		n, _ := io.Copy(client, upstream)
		atomic.AddInt64(&fromHost, n)
		done <- struct{}{}
	}()

	<-done
	_ = upstream.Close()
	_ = client.Close()
	<-done

	_ = r.Admit.Close(ctx, decision.FactID, atomic.LoadInt64(&toHost), atomic.LoadInt64(&fromHost), "eof")
}

// dialPinned dials exactly the addresses CheckResolved just cleared, in order,
// and never re-resolves the hostname. That is the security invariant: a second
// lookup for the transport is a DNS-rebinding window in which the name that
// passed hygiene resolves to 127.0.0.1 for the dial.
func (r *Relay) dialPinned(ctx context.Context, addrs []netip.Addr, port int) (net.Conn, error) {
	var lastErr error
	for _, addr := range addrs {
		conn, err := r.Dial(ctx, "tcp", net.JoinHostPort(addr.String(), strconv.Itoa(port)))
		if err == nil {
			return conn, nil
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("egress: no addresses to dial")
	}
	return nil, lastErr
}

func writeStatus(w io.Writer, code int, reason string) {
	body := ""
	if reason != "" {
		body = "X-Egress-Reason: " + reason + "\r\n"
	}
	fmt.Fprintf(w, "HTTP/1.1 %d %s\r\n%s\r\n", code, http.StatusText(code), body)
}
