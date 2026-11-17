package egress

import (
	"context"
	"net"
	"net/netip"
	"sync"
	"testing"
	"time"
)

// blockingListener is a fake overlay listener: Accept blocks until Close, the
// way a real listener with no callers does, so a relay's Serve loop parks
// instead of spinning. Closing it once is enough — a second Close is a no-op,
// matching net.Listener.
type blockingListener struct {
	once   sync.Once
	closed chan struct{}
}

func newBlockingListener() *blockingListener {
	return &blockingListener{closed: make(chan struct{})}
}

func (l *blockingListener) Accept() (net.Conn, error) {
	<-l.closed
	return nil, net.ErrClosed
}

func (l *blockingListener) Close() error {
	l.once.Do(func() { close(l.closed) })
	return nil
}

func (l *blockingListener) Addr() net.Addr { return &net.TCPAddr{} }

// fakeListenerFactory records the one address+port a relay was bound to. It is
// the seam that lets the wiring be proved without a head, a netstack and ninety
// seconds — the point of ListenerFactory.
type fakeListenerFactory struct {
	listened string
	ln       *blockingListener
}

func (f *fakeListenerFactory) Listen(addr netip.Addr, port uint16) (net.Listener, error) {
	f.listened = netip.AddrPortFrom(addr, port).String()
	f.ln = newBlockingListener()
	return f.ln, nil
}

// The relay listens on the node's OWN overlay address in the org that hosts the
// service, per grant — not on a wildcard. A wildcard tsnet listener knows only
// the primary address, so a node with two grants would silently
// refuse the second org's peers. This asserts an explicit address is passed.
func TestServiceListensOnTheDeclaredAddressAndPort(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	fake := &fakeListenerFactory{}
	cfg := Config{
		Services:  []ServiceConfig{{Kind: "egress", Org: "acme", Port: 8443, State: "approved"}},
		Addresses: map[string]string{"acme": "fd06:6e7:0:1::a"},
	}

	relays, err := StartServices(ctx, cfg, fake)
	if err != nil {
		t.Fatal(err)
	}
	if len(relays) != 1 {
		t.Fatalf("started %d relays, want 1", len(relays))
	}
	if want := "[fd06:6e7:0:1::a]:8443"; fake.listened != want {
		t.Fatalf("listened on %q, want the granted address %q", fake.listened, want)
	}
}

func TestServiceIgnoresAnUnapprovedDeclaration(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	fake := &fakeListenerFactory{}
	cfg := Config{
		Services:  []ServiceConfig{{Kind: "egress", Org: "acme", Port: 8443, State: "declared"}},
		Addresses: map[string]string{"acme": "fd06:6e7:0:1::a"},
	}

	relays, err := StartServices(ctx, cfg, fake)
	if err != nil {
		t.Fatal(err)
	}
	if len(relays) != 0 || fake.listened != "" {
		t.Fatal("a declared-but-unapproved service must not be served")
	}
}

// A kind that is not egress is not this wiring's to serve.
func TestServiceIgnoresNonEgressServices(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	fake := &fakeListenerFactory{}
	cfg := Config{
		Services:  []ServiceConfig{{Kind: "preview", Org: "acme", Port: 3000, State: "approved"}},
		Addresses: map[string]string{"acme": "fd06:6e7:0:1::a"},
	}

	if _, err := StartServices(ctx, cfg, fake); err != nil {
		t.Fatal(err)
	}
	if fake.listened != "" {
		t.Fatal("a preview service must not be served by the egress wiring")
	}
}

// An approved egress service the node holds no address for is a document that
// cannot be served, not a relay bound to a guessed address.
func TestServiceRefusesAnApprovedServiceWithNoAddress(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	fake := &fakeListenerFactory{}
	cfg := Config{
		Services:  []ServiceConfig{{Kind: "egress", Org: "acme", Port: 8443, State: "approved"}},
		Addresses: map[string]string{},
	}

	if _, err := StartServices(ctx, cfg, fake); err == nil {
		t.Fatal("an approved service with no granted address was served anyway")
	}
}

// Cancelling ctx closes the relay's listener so its accept loop returns.
func TestServiceStopsOnContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	fake := &fakeListenerFactory{}
	cfg := Config{
		Services:  []ServiceConfig{{Kind: "egress", Org: "acme", Port: 8443, State: "approved"}},
		Addresses: map[string]string{"acme": "fd06:6e7:0:1::a"},
	}

	relays, err := StartServices(ctx, cfg, fake)
	if err != nil {
		t.Fatal(err)
	}

	cancel()

	done := make(chan struct{})
	go func() {
		for _, r := range relays {
			r.WaitIdle()
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("relays did not drain after ctx cancel")
	}
}
