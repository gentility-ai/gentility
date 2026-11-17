package egress

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakePeers struct{ addr netip.Addr }

func (f fakePeers) WhoIs(_ context.Context, _ net.Addr) (netip.Addr, error) { return f.addr, nil }

// invalidPeers models a buggy or hostile WhoIs that answers no error but a
// zero/invalid address — the fail-open FIX 1(b) closes.
type invalidPeers struct{}

func (invalidPeers) WhoIs(_ context.Context, _ net.Addr) (netip.Addr, error) {
	return netip.Addr{}, nil
}

// A relay under test speaks plain TCP; only its peer attribution and its
// outbound dialler are swapped.
func newTestRelay(t *testing.T, admitBody string) (*Relay, *closeRecorder, net.Listener) {
	t.Helper()

	rec := &closeRecorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/close") {
			rec.record(r)
			w.WriteHeader(http.StatusNoContent)
			return
		}
		_, _ = w.Write([]byte(admitBody))
	}))
	t.Cleanup(srv.Close)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	relay := &Relay{
		Peers: fakePeers{addr: netip.MustParseAddr("fd06:6e7:0:1::b")},
		Admit: NewAdmitClient(srv.Client(), srv.URL, "cred"),
		Dial:  func(ctx context.Context, network, addr string) (net.Conn, error) { return net.Dial(network, addr) },
		Lookup: func(_ context.Context, _ string) ([]netip.Addr, error) {
			return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
		},
	}
	return relay, rec, ln
}

type closeRecorder struct {
	mu   sync.Mutex
	body string
}

func (c *closeRecorder) record(r *http.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	b, _ := io.ReadAll(r.Body)
	c.body = string(b)
}

func (c *closeRecorder) seen() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.body
}

// connect writes one CONNECT request to a dialled relay and returns the reader
// positioned right after the status line, plus that status line.
func connect(t *testing.T, ln net.Listener, target string) (*bufio.Reader, net.Conn, string) {
	t.Helper()
	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.WriteString(c, "CONNECT "+target+" HTTP/1.1\r\nHost: "+target+"\r\n\r\n")
	br := bufio.NewReader(c)
	status, _ := br.ReadString('\n')
	return br, c, status
}

func TestRelayRefusesWhenAdmitRefuses(t *testing.T) {
	dialled := false
	relay, _, ln := newTestRelay(t, `{"decision":"refuse","reason":"not_allowlisted","fact_id":"f"}`)
	relay.Dial = func(context.Context, string, string) (net.Conn, error) {
		dialled = true
		return nil, nil
	}
	go relay.Serve(ln)

	_, c, status := connect(t, ln, "evil.example:443")
	defer c.Close()

	if !strings.Contains(status, "403") {
		t.Fatalf("refused CONNECT status = %q, want 403", status)
	}
	if dialled {
		t.Fatal("a CONNECT refused by admit must never be dialled")
	}
}

func TestRelayRefusesWhenAdmitIsUnreachable(t *testing.T) {
	relay, _, ln := newTestRelay(t, "")
	relay.Admit = NewAdmitClient(http.DefaultClient, "http://127.0.0.1:1", "cred")
	go relay.Serve(ln)

	_, c, status := connect(t, ln, "example.com:443")
	defer c.Close()

	if !strings.Contains(status, "502") {
		t.Fatalf("unreachable-mothership status = %q, want 502", status)
	}
}

// An IP-literal target no org allowlisted is refused by CheckTarget before the
// mothership is asked. Admit points at an unreachable address on purpose: were
// CheckTarget skipped, the admit call would fail and this would be a 502, so a
// 403 is proof the literal was refused first — and Dial is never reached.
func TestRelayRefusesAnUnallowlistedLiteralBeforeAdmit(t *testing.T) {
	dialled := false
	relay, _, ln := newTestRelay(t, "")
	relay.Admit = NewAdmitClient(http.DefaultClient, "http://127.0.0.1:1", "cred")
	relay.Dial = func(context.Context, string, string) (net.Conn, error) {
		dialled = true
		return nil, nil
	}
	go relay.Serve(ln)

	_, c, status := connect(t, ln, "93.184.216.34:443")
	defer c.Close()

	if !strings.Contains(status, "403") {
		t.Fatalf("unallowlisted literal status = %q, want 403 (not 502)", status)
	}
	if dialled {
		t.Fatal("a literal refused by CheckTarget must never be dialled")
	}
}

// A WhoIs that answers an invalid/zero address is an unattributable peer: the
// relay refuses it before asking Admit, so an invalid src can never be judged
// against — and misattributed to — an org's egress policy. Admit points at an
// unreachable address on purpose: were the IsValid check skipped, the zero src
// would flow into an admit call that fails as a 502, so a 403 is proof the
// connection was refused before Admit, and Dial is never reached.
func TestRelayRefusesAnUnattributablePeer(t *testing.T) {
	dialled := false
	relay, _, ln := newTestRelay(t, "")
	relay.Peers = invalidPeers{}
	relay.Admit = NewAdmitClient(http.DefaultClient, "http://127.0.0.1:1", "cred")
	relay.Dial = func(context.Context, string, string) (net.Conn, error) {
		dialled = true
		return nil, nil
	}
	go relay.Serve(ln)

	_, c, status := connect(t, ln, "example.com:443")
	defer c.Close()

	if !strings.Contains(status, "403") {
		t.Fatalf("unattributable-peer status = %q, want 403 (not 502)", status)
	}
	if dialled {
		t.Fatal("a connection from an invalid src must never be dialled")
	}
}

// An open, idle tunnel — neither direction ever reaching EOF — must not make
// WaitIdle hang. Shutdown force-closes it so the copy loops return, the close
// report drains, and WaitIdle completes. The upstream accepts and holds its
// conn open, the client sends nothing after CONNECT: both io.Copy loops block
// until Shutdown closes the conns.
func TestRelayShutdownForceClosesAnOpenTunnel(t *testing.T) {
	upstream, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer upstream.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		c, err := upstream.Accept()
		if err != nil {
			return
		}
		accepted <- c
	}()

	relay, rec, ln := newTestRelay(t, `{"decision":"admit","fact_id":"open-1"}`)
	relay.Hygiene = func([]netip.Addr) error { return nil }
	_, portStr, _ := net.SplitHostPort(upstream.Addr().String())
	go relay.Serve(ln)

	_, c, status := connect(t, ln, "localhost:"+portStr)
	defer c.Close()
	if !strings.Contains(status, "200") {
		t.Fatalf("admitted CONNECT status = %q, want 200", status)
	}
	// The tunnel is established once the upstream side is accepted; now it is
	// open and idle, so neither copy loop will ever return on its own.
	up := <-accepted
	defer up.Close()

	done := make(chan error, 1)
	go func() { done <- relay.Shutdown(context.Background()) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Shutdown returned %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Shutdown hung on an open, idle tunnel")
	}

	// WaitIdle now completes because Shutdown force-closed the tunnel, and the
	// close report was not lost.
	relay.WaitIdle()
	if body := rec.seen(); !strings.Contains(body, `"fact_id":"open-1"`) {
		t.Fatalf("open tunnel force-closed but no close report: %s", body)
	}
}

// watchedListener hands out conns that announce their first Read, which is the
// read inside http.ReadRequest: once it has fired, the relay's goroutine is
// counted in the wait group and is blocked reading headers. That is the exact
// state the shutdown test needs, and waiting for it beats guessing at a sleep.
type watchedListener struct {
	net.Listener
	reading chan struct{}
	once    sync.Once
}

func (l *watchedListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return c, err
	}
	return &watchedConn{Conn: c, listener: l}, nil
}

type watchedConn struct {
	net.Conn
	listener *watchedListener
}

func (c *watchedConn) Read(b []byte) (int, error) {
	c.listener.once.Do(func() { close(c.listener.reading) })
	return c.Conn.Read(b)
}

// A peer that connects and sends no headers at all is still an accepted
// connection the relay must close on shutdown. It is not a tunnel, so `track`
// has never seen it — yet Serve has already counted it in the wait group, so a
// Shutdown that closes only tracked tunnels leaves this goroutine blocked in
// http.ReadRequest forever and WaitIdle never returns. That hangs `serve`'s
// reconcile drain on withdraw or rebind: one silent peer stalls the node.
func TestShutdownClosesAConnectionStillReadingHeaders(t *testing.T) {
	relay, _, raw := newTestRelay(t, "")
	ln := &watchedListener{Listener: raw, reading: make(chan struct{})}
	go relay.Serve(ln)

	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	// Say nothing. Wait until the relay is actually blocked reading our headers.
	select {
	case <-ln.reading:
	case <-time.After(3 * time.Second):
		t.Fatal("the relay never started reading the connection's headers")
	}

	// Exactly what serve does on withdraw or rebind: close the listener, then
	// Shutdown, then WaitIdle. Off the test goroutine, because on the unfixed
	// relay it never comes back.
	drained := make(chan error, 1)
	go func() {
		_ = raw.Close()
		err := relay.Shutdown(context.Background())
		relay.WaitIdle()
		drained <- err
	}()
	select {
	case err := <-drained:
		if err != nil {
			t.Fatalf("Shutdown returned %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the shutdown drain hung on a connection still reading headers")
	}
}

// Defence in depth: even with no shutdown, a peer that connects and then says
// nothing is dropped once the header deadline passes, so a slow loris cannot
// accumulate goroutines against the relay.
func TestASilentPeerIsDroppedByTheHeaderDeadline(t *testing.T) {
	relay, _, ln := newTestRelay(t, "")
	relay.HeaderDeadline = 100 * time.Millisecond
	go relay.Serve(ln)

	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	// Say nothing, and read: the relay closing on us is the EOF we want.
	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := c.Read(make([]byte, 1)); err == nil || errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("a silent peer's connection was not dropped by the header deadline (read err = %v)", err)
	}

	relay.WaitIdle()
}

func TestRelayRefusesAPrivateResolution(t *testing.T) {
	dialled := false
	relay, _, ln := newTestRelay(t, `{"decision":"admit","fact_id":"f"}`)
	relay.Lookup = func(_ context.Context, _ string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("192.168.1.1")}, nil
	}
	relay.Dial = func(context.Context, string, string) (net.Conn, error) {
		dialled = true
		return nil, nil
	}
	go relay.Serve(ln)

	_, c, status := connect(t, ln, "router.local:443")
	defer c.Close()

	if !strings.Contains(status, "403") {
		t.Fatalf("private-resolution status = %q, want 403", status)
	}
	if dialled {
		t.Fatal("a resolution refused by CheckResolved must never be tunnelled")
	}
}

// Pinned resolution: the relay dials the exact addresses CheckResolved cleared,
// never a re-resolution of the hostname. The hostname would resolve to nothing
// dialable here — Dial is fed the pinned addr, and the test asserts it is the
// one Lookup returned.
func TestRelayDialsThePinnedResolvedAddresses(t *testing.T) {
	pinned := netip.MustParseAddr("93.184.216.34")

	var dialledAddr string
	relay, _, ln := newTestRelay(t, `{"decision":"admit","fact_id":"f"}`)
	relay.Hygiene = func([]netip.Addr) error { return nil }
	relay.Lookup = func(_ context.Context, host string) ([]netip.Addr, error) {
		if host != "example.com" {
			t.Errorf("Lookup asked for %q, not the CONNECT host", host)
		}
		return []netip.Addr{pinned}, nil
	}
	relay.Dial = func(_ context.Context, _, addr string) (net.Conn, error) {
		dialledAddr = addr
		return nil, io.EOF // fail the dial: we only care what address it tried
	}
	go relay.Serve(ln)

	_, c, _ := connect(t, ln, "example.com:443")
	defer c.Close()
	relay.WaitIdle()

	if want := net.JoinHostPort(pinned.String(), "443"); dialledAddr != want {
		t.Fatalf("dialled %q, want the pinned %q", dialledAddr, want)
	}
}

func TestRelayTunnelsAndCountsBytes(t *testing.T) {
	// An upstream that echoes, so both directions have a count.
	upstream, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer upstream.Close()
	go func() {
		c, err := upstream.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		buf := make([]byte, 5)
		_, _ = io.ReadFull(c, buf)
		_, _ = c.Write([]byte("PONGPONG"))
	}()

	relay, rec, ln := newTestRelay(t, `{"decision":"admit","fact_id":"f-42"}`)
	// The echo upstream lives on 127.0.0.1, which CheckResolved rightly refuses.
	// A seam the production path never takes is cheaper than weakening `refused`:
	// only this tunnelling test disarms hygiene; every refusal test keeps it.
	relay.Hygiene = func([]netip.Addr) error { return nil }
	_, portStr, _ := net.SplitHostPort(upstream.Addr().String())
	go relay.Serve(ln)

	c, _ := net.Dial("tcp", ln.Addr().String())
	defer c.Close()

	_, _ = io.WriteString(c, "CONNECT localhost:"+portStr+" HTTP/1.1\r\nHost: localhost:"+portStr+"\r\n\r\n")
	br := bufio.NewReader(c)
	status, _ := br.ReadString('\n')
	if !strings.Contains(status, "200") {
		t.Fatalf("admitted CONNECT status = %q, want 200", status)
	}
	for {
		line, _ := br.ReadString('\n')
		if line == "\r\n" || line == "" {
			break
		}
	}

	_, _ = c.Write([]byte("PING!"))
	out := make([]byte, 8)
	_, _ = io.ReadFull(br, out)
	if string(out) != "PONGPONG" {
		t.Fatalf("tunnelled reply = %q", out)
	}
	_ = c.Close()

	relay.WaitIdle()
	if body := rec.seen(); !strings.Contains(body, `"fact_id":"f-42"`) ||
		!strings.Contains(body, `"bytes_to_host":5`) ||
		!strings.Contains(body, `"bytes_from_host":8`) {
		t.Fatalf("close report = %s", body)
	}
}
