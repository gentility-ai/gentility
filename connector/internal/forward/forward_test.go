package forward

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gentility-ai/gentility/connector/internal/config"
	"github.com/gentility-ai/gentility/connector/internal/exitcodes"
	"github.com/gentility-ai/gentility/connector/internal/tsnode"
)

const (
	nodeAddress    = "fd06:6e7:0:a::5"
	gatewayAddress = "fd06:6e7:0:a::1"
	otherAddress   = "fd06:6e7:0:a::9"
)

func TestParseTakesAKindAndAnAbsoluteSocket(t *testing.T) {
	spec, err := Parse("vmrunner=/run/vmrunner/door.sock")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if spec != (Spec{Kind: "vmrunner", Socket: "/run/vmrunner/door.sock"}) {
		t.Fatalf("Parse = %+v", spec)
	}
}

func TestParseRefusesWhatItCannotCarry(t *testing.T) {
	for value, says := range map[string]string{
		"vmrunner":                   "wants <kind>=<socket>",
		"=/run/door.sock":            "wants <kind>=<socket>",
		"vmrunner=run/door.sock":     "wants <kind>=<socket>",
		"vmrunner=/run/../door.sock": "wants <kind>=<socket>",
		"vmrunner=":                  "wants <kind>=<socket>",
		"egress=/run/door.sock":      "forwards only vmrunner",
		"ssh=/run/door.sock":         "forwards only vmrunner",
	} {
		_, err := Parse(value)
		if err == nil || !strings.Contains(err.Error(), says) {
			t.Errorf("Parse(%q) = %v, want a refusal saying %q", value, err, says)
			continue
		}
		if code, fatal := exitcodes.Of(err); !fatal || code != exitcodes.Usage {
			t.Errorf("Parse(%q) exits (%d, %v), want (%d, true)", value, code, fatal, exitcodes.Usage)
		}
	}
}

// harness is a Forwarder over loopback: Listen binds 127.0.0.1 and records
// what it was asked for, Attribute answers whoever the test says the peer is,
// and the socket is a real unix socket that echoes in upper case.
type harness struct {
	t         *testing.T
	forwarder *Forwarder
	socket    string
	peer      atomic.Value // netip.Addr the next connection is attributed to
	dials     atomic.Int32

	mu    sync.Mutex
	asked []netip.AddrPort
	bound net.Listener
}

func newHarness(t *testing.T) *harness {
	t.Helper()

	// Short: a unix socket path is limited to about a hundred bytes, and
	// t.TempDir is longer than that on macOS.
	dir, err := os.MkdirTemp("", "fwd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })

	h := &harness{t: t, socket: filepath.Join(dir, "door.sock")}
	h.peer.Store(netip.MustParseAddr(gatewayAddress))
	h.forwarder = &Forwarder{
		Spec: Spec{Kind: "vmrunner", Socket: h.socket},
		Listen: func(addr netip.Addr, port uint16) (net.Listener, error) {
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				return nil, err
			}
			h.mu.Lock()
			h.asked = append(h.asked, netip.AddrPortFrom(addr, port))
			h.bound = ln
			h.mu.Unlock()
			return ln, nil
		},
		Attribute: func(ctx context.Context, remote netip.AddrPort, local netip.Addr) (netip.Addr, error) {
			return h.peer.Load().(netip.Addr), nil
		},
		Log: t.Logf,
	}
	h.forwarder.Dial = func(ctx context.Context, socket string) (net.Conn, error) {
		h.dials.Add(1)
		return dialUnix(ctx, socket)
	}
	t.Cleanup(h.forwarder.Stop)
	return h
}

// echo serves the socket: each line comes back in upper case.
func (h *harness) echo() {
	h.t.Helper()
	ln, err := net.Listen("unix", h.socket)
	if err != nil {
		h.t.Fatal(err)
	}
	h.t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				lines := bufio.NewScanner(conn)
				for lines.Scan() {
					if _, err := io.WriteString(conn, strings.ToUpper(lines.Text())+"\n"); err != nil {
						return
					}
				}
			}()
		}
	}()
}

func (h *harness) apply(document *config.Document) error {
	return h.forwarder.Apply(context.Background(), document)
}

func (h *harness) listener() net.Listener {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.bound
}

func (h *harness) binds() []netip.AddrPort {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]netip.AddrPort(nil), h.asked...)
}

func (h *harness) connect() net.Conn {
	h.t.Helper()
	ln := h.listener()
	if ln == nil {
		h.t.Fatal("nothing is bound")
	}
	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		h.t.Fatal(err)
	}
	h.t.Cleanup(func() { conn.Close() })
	return conn
}

// roundTrip writes a line and answers what comes back, or the error.
func roundTrip(conn net.Conn, line string) (string, error) {
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.WriteString(conn, line+"\n"); err != nil {
		return "", err
	}
	return bufio.NewReader(conn).ReadString('\n')
}

// closed says whether the far end closed conn: a read that ends in EOF or a
// reset, rather than a timeout.
func closed(conn net.Conn) bool {
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, err := conn.Read(make([]byte, 1))
	var timeout net.Error
	return err != nil && !(errors.As(err, &timeout) && timeout.Timeout())
}

func document(services ...config.Service) *config.Document {
	return &config.Document{
		Grants: []tsnode.ConfigGrant{
			{Org: "elsewhere", Address: "fd06:6e7:0:b::5"},
			{Org: "platform", Address: nodeAddress},
		},
		Services: services,
		Gateway:  config.Gateway{Address: gatewayAddress, Port: 443},
	}
}

func vmrunner(state string, forwardable bool) config.Service {
	return config.Service{
		ID: "svc-1", Kind: "vmrunner", Org: "platform", Port: 7443, State: state,
		Address: nodeAddress, Forwardable: forwardable,
	}
}

func wantUsage(t *testing.T, err error, says string) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), says) {
		t.Fatalf("Apply = %v, want a refusal saying %q", err, says)
	}
	if code, fatal := exitcodes.Of(err); !fatal || code != exitcodes.Usage {
		t.Fatalf("the refusal exits (%d, %v), want (%d, true)", code, fatal, exitcodes.Usage)
	}
}

func TestTheFirstDocumentMustMarkExactlyOneEntry(t *testing.T) {
	for name, tc := range map[string]struct {
		services []config.Service
		says     string
	}{
		"no entry": {nil, "declares no forwardable vmrunner service"},
		"an entry without the mark": {
			[]config.Service{vmrunner("approved", false)},
			"declares no forwardable vmrunner service",
		},
		"a marked entry of another kind": {
			[]config.Service{{ID: "svc-2", Kind: "ssh", Org: "platform", Port: 22, State: "approved", Forwardable: true}},
			"declares no forwardable vmrunner service",
		},
		"two marked entries": {
			[]config.Service{vmrunner("approved", true), vmrunner("approved", true)},
			"marks 2 forwardable vmrunner services",
		},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			wantUsage(t, h.apply(document(tc.services...)), tc.says)
			if binds := h.binds(); len(binds) != 0 {
				t.Fatalf("a refused start bound %v", binds)
			}
		})
	}
}

func TestADeclaredServiceBindsNothingUntilApproved(t *testing.T) {
	h := newHarness(t)

	if err := h.apply(document(vmrunner("declared", true))); err != nil {
		t.Fatalf("a declared, marked service was refused: %v", err)
	}
	if binds := h.binds(); len(binds) != 0 {
		t.Fatalf("a declared service was bound on %v", binds)
	}

	if err := h.apply(document(vmrunner("approved", true))); err != nil {
		t.Fatal(err)
	}
	want := netip.AddrPortFrom(netip.MustParseAddr(nodeAddress), 7443)
	if binds := h.binds(); len(binds) != 1 || binds[0] != want {
		t.Fatalf("bound %v, want exactly [%s]: the node's own address in the service's organization", binds, want)
	}
}

func TestTheGatewayReachesTheSocket(t *testing.T) {
	h := newHarness(t)
	h.echo()
	if err := h.apply(document(vmrunner("approved", true))); err != nil {
		t.Fatal(err)
	}

	got, err := roundTrip(h.connect(), "inventory")
	if err != nil {
		t.Fatalf("the gateway's connection: %v", err)
	}
	if got != "INVENTORY\n" {
		t.Fatalf("the socket answered %q, want the bytes carried both ways", got)
	}
}

func TestAPeerThatIsNotTheGatewayIsClosedBeforeTheSocketIsDialled(t *testing.T) {
	h := newHarness(t)
	h.echo()
	if err := h.apply(document(vmrunner("approved", true))); err != nil {
		t.Fatal(err)
	}

	h.peer.Store(netip.MustParseAddr(otherAddress))
	if !closed(h.connect()) {
		t.Fatal("a peer that is not the gateway was left open")
	}
	if n := h.dials.Load(); n != 0 {
		t.Fatalf("the socket was dialled %d time(s) for a peer that is not the gateway", n)
	}
}

func TestAnUnattributablePeerIsClosed(t *testing.T) {
	h := newHarness(t)
	h.echo()
	h.forwarder.Attribute = func(context.Context, netip.AddrPort, netip.Addr) (netip.Addr, error) {
		return netip.Addr{}, errors.New("the head knows no such peer")
	}
	if err := h.apply(document(vmrunner("approved", true))); err != nil {
		t.Fatal(err)
	}

	if !closed(h.connect()) {
		t.Fatal("a peer the head could not attribute was left open")
	}
	if n := h.dials.Load(); n != 0 {
		t.Fatalf("the socket was dialled %d time(s)", n)
	}
}

func TestAMissingSocketClosesTheConnection(t *testing.T) {
	h := newHarness(t)
	if err := h.apply(document(vmrunner("approved", true))); err != nil {
		t.Fatal(err)
	}

	if !closed(h.connect()) {
		t.Fatal("a connection to a missing socket was left open")
	}
	if n := h.dials.Load(); n != 1 {
		t.Fatalf("the socket was dialled %d times, want once and no retry", n)
	}
}

// The gateway is read from the current document on every connection: a peer
// that was the gateway is closed once the document names another.
func TestAGatewayThatMovedIsReadFromTheCurrentDocument(t *testing.T) {
	h := newHarness(t)
	h.echo()
	if err := h.apply(document(vmrunner("approved", true))); err != nil {
		t.Fatal(err)
	}
	if _, err := roundTrip(h.connect(), "before"); err != nil {
		t.Fatalf("the gateway before it moved: %v", err)
	}

	moved := document(vmrunner("approved", true))
	moved.Gateway.Address = otherAddress
	if err := h.apply(moved); err != nil {
		t.Fatal(err)
	}

	if !closed(h.connect()) {
		t.Fatal("the old gateway was let in after the document named another")
	}
	h.peer.Store(netip.MustParseAddr(otherAddress))
	if _, err := roundTrip(h.connect(), "after"); err != nil {
		t.Fatalf("the moved gateway: %v", err)
	}
}

// A later document without the entry unbinds and closes what is open, and
// the connector runs on; the entry returning binds again.
func TestALaterDocumentWithoutTheMarkUnbindsAndTheMarkReturningBindsAgain(t *testing.T) {
	h := newHarness(t)
	h.echo()
	if err := h.apply(document(vmrunner("approved", true))); err != nil {
		t.Fatal(err)
	}
	open := h.connect()
	if _, err := roundTrip(open, "open"); err != nil {
		t.Fatal(err)
	}
	first := h.listener()

	if err := h.apply(document(vmrunner("approved", false))); err != nil {
		t.Fatalf("a later document without the mark was fatal: %v", err)
	}
	if !closed(open) {
		t.Fatal("an open forwarded connection outlived the mark")
	}
	if _, err := net.Dial("tcp", first.Addr().String()); err == nil {
		t.Fatal("the listener outlived the mark")
	}

	if err := h.apply(document(vmrunner("approved", true))); err != nil {
		t.Fatal(err)
	}
	if len(h.binds()) != 2 {
		t.Fatalf("binds = %v, want a second bind once the mark returned", h.binds())
	}
	if _, err := roundTrip(h.connect(), "again"); err != nil {
		t.Fatalf("after the mark returned: %v", err)
	}
}

func TestApprovedGoingBackToDeclaredUnbinds(t *testing.T) {
	h := newHarness(t)
	h.echo()
	if err := h.apply(document(vmrunner("approved", true))); err != nil {
		t.Fatal(err)
	}
	open := h.connect()

	if err := h.apply(document(vmrunner("declared", true))); err != nil {
		t.Fatal(err)
	}
	if !closed(open) {
		t.Fatal("a connection outlived its service's approval")
	}
}

func TestALaterDocumentWithTwoMarkedEntriesUnbinds(t *testing.T) {
	h := newHarness(t)
	h.echo()
	if err := h.apply(document(vmrunner("approved", true))); err != nil {
		t.Fatal(err)
	}
	open := h.connect()

	if err := h.apply(document(vmrunner("approved", true), vmrunner("approved", true))); err != nil {
		t.Fatalf("a later document with two marked entries was fatal: %v", err)
	}
	if !closed(open) {
		t.Fatal("two marked entries left the forward bound; neither is to be picked")
	}
}

// The same document again is no rebind: the open connection carries on.
func TestTheSameDocumentAgainKeepsTheBinding(t *testing.T) {
	h := newHarness(t)
	h.echo()
	if err := h.apply(document(vmrunner("approved", true))); err != nil {
		t.Fatal(err)
	}
	open := h.connect()
	if _, err := roundTrip(open, "one"); err != nil {
		t.Fatal(err)
	}

	if err := h.apply(document(vmrunner("approved", true))); err != nil {
		t.Fatal(err)
	}
	if _, err := roundTrip(open, "two"); err != nil {
		t.Fatalf("the same document again broke an open connection: %v", err)
	}
	if len(h.binds()) != 1 {
		t.Fatalf("binds = %v, want one", h.binds())
	}
}
