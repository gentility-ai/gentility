package gateway

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"testing"
	"time"
)

// A rolling `kamal deploy` replaces the app container and leaves the gateway
// alone. That makes the gateway's inbound
// listener outlive the thing it proxies to, and these tests are about the seam
// between the two: the app goes away, the app comes back on the same network
// alias, and the only thing a node should ever see is a few seconds of 502.
//
// The two failures they exist to catch:
//
//   - a gateway that dies with its upstream, which would take every
//     organization off the overlay on every deploy of the app;
//   - a gateway that keeps serving from the old container's keep-alive
//     connection, which would be 502s that never end until somebody restarted
//     it.
//
// Neither is answered with a retry: a 502 for the seconds the swap takes is the
// honest answer, and reconnection makes a blip a delay rather than a
// lost case.

// gatewayUnderTest is one gateway process that does not restart for the length
// of a test: the address a node dials, a client that trusts its leaf, the TLS
// config behind that client for the assertions that dial by hand, and the
// channel Serve reports on — silence on it is the listener still being up.
type gatewayUnderTest struct {
	address string
	client  *http.Client
	tls     *tls.Config
	stopped <-chan error
}

// rolling stands the inbound door behind TLS on a loopback address, the way
// Listen does on an overlay one, and hands back everything a test needs to
// treat it as a process it is not allowed to restart.
func rolling(t *testing.T, upstream string) gatewayUnderTest {
	t.Helper()

	target, err := url.Parse(upstream)
	if err != nil {
		t.Fatalf("parsing the upstream: %v", err)
	}

	inbound := NewInbound(target, func(context.Context, netip.AddrPort, netip.Addr) (netip.Addr, error) {
		return netip.MustParseAddr(peerAddress), nil
	}, testSecret, nil)

	serverTLS, roots := serving(t)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("binding the gateway's listener: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	stopped := make(chan error, 1)
	go func() { stopped <- Serve(ctx, tls.NewListener(ln, serverTLS), inbound) }()

	clientTLS := nodeTLS(roots)

	transport := &http.Transport{TLSClientConfig: clientTLS}
	t.Cleanup(transport.CloseIdleConnections)

	return gatewayUnderTest{
		address: ln.Addr().String(),
		client:  &http.Client{Transport: transport},
		tls:     clientTLS,
		stopped: stopped,
	}
}

// nodeTLS is what a joined node verifies the gateway with: the pinned root from
// its config document, and the one hostname the leaf is issued for.
func nodeTLS(roots *x509.CertPool) *tls.Config {
	return &tls.Config{
		RootCAs:    roots,
		ServerName: "gentility-overlay.internal",
		MinVersion: tls.VersionTLS13,
	}
}

// appContainer serves the app's overlay listener — admission check and all — on
// ln. It is the thing a deploy replaces, so it is deliberately given the
// listener rather than allowed to choose one: the next container has to arrive
// on the same address, which is what a kamal network alias does.
func appContainer(t *testing.T, ln net.Listener, handler http.HandlerFunc) *http.Server {
	t.Helper()

	server := &http.Server{
		Handler: verifying(testSecret, time.Now, handler),
		// The app's own number, not the gateway's: borrowing
		// `readHeaderTimeout` would make a change to the gateway's bound look
		// like it moved something in the thing behind it. Nothing here is
		// waiting 30 s for a header anyway.
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() { _ = server.Serve(ln) }()
	t.Cleanup(func() { _ = server.Close() })

	return server
}

// alias is the address the app's containers take turns holding.
func alias(t *testing.T) (net.Listener, string) {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("binding the app's network alias: %v", err)
	}

	return ln, ln.Addr().String()
}

// rebind takes the alias back for the next container. A deploy's new container
// arrives on the address the old one had, and a test that let the kernel pick a
// second port would be proving something easier than that.
func rebind(t *testing.T, address string) net.Listener {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for {
		ln, err := net.Listen("tcp", address)
		if err == nil {
			return ln
		}
		if time.Now().After(deadline) {
			t.Fatalf("the next container could not take %s: %v", address, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// through is one request a node makes, read to the end and closed — so the
// connection goes back in the pool and the next request would reuse it, which
// is the state the swap has to survive.
func through(t *testing.T, gateway gatewayUnderTest, path string) (int, string) {
	t.Helper()

	response, err := gateway.client.Get("https://" + gateway.address + path)
	if err != nil {
		t.Fatalf("requesting %s through the gateway: %v", path, err)
	}
	defer response.Body.Close()

	read, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("reading the response to %s: %v", path, err)
	}

	return response.StatusCode, string(read)
}

// stillListening is the assertion the whole slice turns on: whatever the app
// container is doing, the gateway's own listener is up and a node can complete
// a TLS handshake against it. A handshake that fails here is the fleet off the
// network, not a deploy in progress.
func stillListening(t *testing.T, gateway gatewayUnderTest) {
	t.Helper()

	select {
	case err := <-gateway.stopped:
		t.Fatalf("the gateway stopped serving: %v", err)
	default:
	}

	conn, err := tls.Dial("tcp", gateway.address, gateway.tls)
	if err != nil {
		t.Fatalf("a node could not complete a TLS handshake with the gateway: %v", err)
	}
	defer conn.Close()

	if !conn.ConnectionState().HandshakeComplete {
		t.Error("the TLS connection to the gateway is open but its handshake never completed")
	}
}

// signed is the three peer headers as one container saw them.
type signed struct {
	peer, unixTs, signature string
}

func TestGatewaySurvivesAnUpstreamSwap(t *testing.T) {
	held, address := alias(t)

	first := appContainer(t, held, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "container A")
	})

	gateway := rolling(t, "http://"+address)

	if status, body := through(t, gateway, "/overlay/self/config"); status != http.StatusOK || body != "container A" {
		t.Fatalf("before the deploy the gateway answered %d %q, want 200 container A", status, body)
	}

	// The old container goes away, which is what every deploy of the app does.
	if err := first.Close(); err != nil {
		t.Fatalf("stopping the first container: %v", err)
	}

	if status, _ := through(t, gateway, "/overlay/self/config"); status != http.StatusBadGateway {
		t.Errorf("with no app behind the alias the gateway answered %d, want 502", status)
	}

	// And it is a 502 from a gateway that is still there, not a connection a
	// node could not make. The difference is the whole point: one is a deploy in
	// progress, the other is an outage.
	stillListening(t, gateway)

	// The new container takes the alias.
	seen := make(chan signed, 1)
	appContainer(t, rebind(t, address), func(w http.ResponseWriter, r *http.Request) {
		select {
		case seen <- signed{
			peer:      r.Header.Get(PeerHeader),
			unixTs:    r.Header.Get(PeerTsHeader),
			signature: r.Header.Get(PeerSigHeader),
		}:
		default:
		}
		fmt.Fprint(w, "container B")
	})

	// No restart in between: the gateway is the same process on the same
	// listener, and this is the first request it has served the new container.
	if status, body := through(t, gateway, "/overlay/self/config"); status != http.StatusOK || body != "container B" {
		t.Fatalf("after the deploy the gateway answered %d %q, want 200 container B", status, body)
	}
	stillListening(t, gateway)

	// The signature survived the swap. A 200 above already says so — the new
	// container runs the same admission check the app does and would have
	// answered 401 — but naming it here means a broken MAC reads as the secret
	// having been lost with the old container rather than as "container B said
	// the wrong thing".
	header := <-seen
	if header.peer != peerAddress {
		t.Errorf("the new container was told peer %q, want the attributed %q", header.peer, peerAddress)
	}
	if want := Sign(testSecret, peerAddress, mustParseInt(t, header.unixTs)); header.signature != want {
		t.Error("the signature the new container saw is not one over the attributed peer under the shared secret")
	}
}

// The pool is the other way a swap goes wrong. The first request leaves an idle
// keep-alive connection to the old container behind; the next one must not be
// served on it, and must not need the caller to notice and try again.
//
// "One request, one answer" holds for a REPLAYABLE request — the GET below, or
// anything whose body `GetBody` can produce a second time. A proxied POST that
// carries a body and no `GetBody` (exchange, presence) gets the honest 502
// instead, so a reconnect delays the case rather than losing it.
//
// It differs from the first test's third phase in the one way that matters: the
// request that has to succeed here is the FIRST one after the close, with no
// intervening 502 in between. The pool has therefore had no other reason to be
// flushed, so a success is the transport handling the dead connection and not a
// side effect of the previous assertion.
func TestGatewayDoesNotCacheUpstreamConnectionsPastAClose(t *testing.T) {
	held, address := alias(t)

	first := appContainer(t, held, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "container A")
	})

	gateway := rolling(t, "http://"+address)

	// Read to the end and closed, so the gateway's transport is now holding an
	// idle connection to this container.
	if status, body := through(t, gateway, "/overlay/self/config"); status != http.StatusOK || body != "container A" {
		t.Fatalf("the first container answered %d %q, want 200 container A", status, body)
	}

	// Swapped with no pause: nothing has had time to reap that idle connection
	// on age, so if the next request succeeds it is because a dead connection
	// was not handed to it.
	if err := first.Close(); err != nil {
		t.Fatalf("stopping the first container: %v", err)
	}

	served := make(chan struct{}, 4)
	appContainer(t, rebind(t, address), func(w http.ResponseWriter, r *http.Request) {
		served <- struct{}{}
		fmt.Fprint(w, "container B")
	})

	// ONE request, no loop, no retry in the caller — replayable, so the
	// transport's own replay is allowed to be what makes it work.
	status, body := through(t, gateway, "/overlay/self/config")
	if status != http.StatusOK || body != "container B" {
		t.Fatalf("the request after the swap answered %d %q, want 200 container B — "+
			"the gateway reused a connection to the container that went away", status, body)
	}
	if got := len(served); got != 1 {
		t.Errorf("the new container served %d requests for the node's one", got)
	}
}
