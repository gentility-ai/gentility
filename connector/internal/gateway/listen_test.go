package gateway

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"sync"
	"testing"
	"time"
)

// binder stands in for tsnode.Node: it hands out real loopback listeners for
// whatever addresses it is currently holding, and closes the ones that go
// away, which is what ListenAll does for a revoked grant.
type binder struct {
	t *testing.T

	mu        sync.Mutex
	addresses []netip.Addr
	fail      error
	bound     map[netip.Addr]net.Listener
}

func newBinder(t *testing.T, addresses ...netip.Addr) *binder {
	b := &binder{t: t, addresses: addresses, bound: map[netip.Addr]net.Listener{}}
	t.Cleanup(func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		for _, ln := range b.bound {
			ln.Close()
		}
	})

	return b
}

func (b *binder) hold(addresses ...netip.Addr) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.addresses = addresses
}

func (b *binder) breaks(err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.fail = err
}

func (b *binder) Addresses(context.Context) ([]netip.Addr, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.fail != nil {
		return nil, b.fail
	}

	return append([]netip.Addr(nil), b.addresses...), nil
}

func (b *binder) ListenAll(addresses []netip.Addr, port uint16) (map[netip.Addr]net.Listener, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if port != Port {
		b.t.Errorf("ListenAll was asked for port %d, want %d", port, Port)
	}

	want := map[netip.Addr]bool{}
	for _, address := range addresses {
		want[address] = true
	}

	for address, ln := range b.bound {
		if !want[address] {
			ln.Close()
			delete(b.bound, address)
		}
	}

	opened := map[netip.Addr]net.Listener{}
	for _, address := range addresses {
		if _, held := b.bound[address]; held {
			continue
		}

		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return opened, err
		}
		b.bound[address] = ln
		opened[address] = ln
	}

	return opened, nil
}

func (b *binder) addr(address netip.Addr) (string, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()

	ln, held := b.bound[address]
	if !held {
		return "", false
	}

	return ln.Addr().String(), true
}

// serving is the TLS material a node would verify against: one leaf the test
// root issued, and the pool holding that root — which is what a node's pinned
// ca_pem amounts to.
func serving(t *testing.T) (*tls.Config, *x509.CertPool) {
	t.Helper()

	leaf := mint(t, "gentility-overlay.internal", testAddresses,
		time.Now().Add(-time.Hour), time.Now().Add(time.Hour))

	certificate, err := tls.X509KeyPair([]byte(leaf.certificatePEM), []byte(leaf.privateKeyPEM))
	if err != nil {
		t.Fatalf("building the test certificate: %v", err)
	}

	return &tls.Config{
		Certificates: []tls.Certificate{certificate},
		MinVersion:   tls.VersionTLS13,
		NextProtos:   []string{"http/1.1"},
	}, pool(t)
}

func TestListenServesTLSOnEveryAddressTheNodeHolds(t *testing.T) {
	upstream := listener(t, testSecret, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, r.Header.Get(PeerHeader))
	})

	target, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatalf("parsing the upstream: %v", err)
	}

	inbound := NewInbound(target, func(context.Context, netip.AddrPort, netip.Addr) (netip.Addr, error) {
		return netip.MustParseAddr(peerAddress), nil
	}, testSecret, nil)

	first := netip.MustParseAddr("fd06:6e7:0:1::1")
	second := netip.MustParseAddr("fd06:6e7:0:2::1")
	binder := newBinder(t, first, second)

	tlsConfig, roots := serving(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go Listen(ctx, binder, tlsConfig, inbound, nil)

	client := &http.Client{Transport: &http.Transport{
		TLSClientConfig: &tls.Config{
			RootCAs:    roots,
			ServerName: "gentility-overlay.internal",
			MinVersion: tls.VersionTLS13,
		},
	}}

	for _, address := range []netip.Addr{first, second} {
		var served string
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if bound, held := binder.addr(address); held {
				served = bound
				break
			}
			time.Sleep(time.Millisecond)
		}
		if served == "" {
			t.Fatalf("%s was never bound", address)
		}

		response, err := client.Get("https://" + served + "/overlay/self/config")
		if err != nil {
			t.Fatalf("dialling the listener for %s: %v", address, err)
		}
		defer response.Body.Close()

		if response.StatusCode != http.StatusOK {
			t.Errorf("the listener for %s answered %d", address, response.StatusCode)
		}
	}
}

// An address that goes away takes its listener with it, and the loop carries
// on serving the ones that are left. A revoked grant must not take down the
// organizations that still hold one.
func TestListenLetsGoOfARevokedAddress(t *testing.T) {
	upstream := listener(t, testSecret, func(w http.ResponseWriter, r *http.Request) {})
	target, _ := url.Parse(upstream.URL)

	inbound := NewInbound(target, func(context.Context, netip.AddrPort, netip.Addr) (netip.Addr, error) {
		return netip.MustParseAddr(peerAddress), nil
	}, testSecret, nil)

	revoked := netip.MustParseAddr("fd06:6e7:0:2::1")
	kept := netip.MustParseAddr("fd06:6e7:0:1::1")
	binder := newBinder(t, kept, revoked)

	tlsConfig, _ := serving(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go Listen(ctx, binder, tlsConfig, inbound, nil)

	var address string
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if bound, held := binder.addr(revoked); held {
			address = bound
			break
		}
		time.Sleep(time.Millisecond)
	}
	if address == "" {
		t.Fatal("the address to be revoked was never bound")
	}

	binder.hold(kept)

	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", address, time.Second)
		if err != nil {
			if _, held := binder.addr(kept); !held {
				t.Fatal("the kept address lost its listener too")
			}
			return
		}
		conn.Close()
		time.Sleep(10 * time.Millisecond)
	}

	t.Error("the revoked address is still accepting connections")
}

// The netmap being briefly unreadable is not a reason to stop serving what is
// already bound, or to stop asking.
func TestListenSurvivesAnAddressReadThatFails(t *testing.T) {
	upstream := listener(t, testSecret, func(w http.ResponseWriter, r *http.Request) {})
	target, _ := url.Parse(upstream.URL)

	inbound := NewInbound(target, func(context.Context, netip.AddrPort, netip.Addr) (netip.Addr, error) {
		return netip.MustParseAddr(peerAddress), nil
	}, testSecret, nil)

	address := netip.MustParseAddr("fd06:6e7:0:1::1")
	binder := newBinder(t, address)
	binder.breaks(errors.New("the local API is not answering"))

	tlsConfig, _ := serving(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	complaints := make(chan string, 8)
	go Listen(ctx, binder, tlsConfig, inbound, func(format string, args ...any) {
		select {
		case complaints <- fmt.Sprintf(format, args...):
		default:
		}
	})

	select {
	case <-complaints:
	case <-time.After(5 * time.Second):
		t.Fatal("the loop said nothing about an address read it could not do")
	}

	binder.breaks(nil)

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, held := binder.addr(address); held {
			return
		}
		time.Sleep(time.Millisecond)
	}

	t.Error("the loop never bound the address once the netmap could be read again")
}
