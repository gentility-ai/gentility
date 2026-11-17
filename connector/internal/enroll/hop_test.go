package enroll

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/gentility-ai/gentility/connector/internal/state"
)

// Mothership is the one place four commands resolve the hop to Gentility, so
// what is asserted here is the composition and nothing Gateway already answers
// on its own (see TestGatewayBaseURL): which of the two CAs is used, and what a
// hop with no gateway on it is wired as.
func TestMothershipResolvesTheHop(t *testing.T) {
	developmentBuild(t)
	gateway := netip.MustParseAddrPort("[fd06:6e7:0:1::1]:443")
	loopback := netip.MustParseAddrPort("127.0.0.1:4006")
	addresses := []netip.Addr{
		netip.MustParseAddr("fd06:6e7:0:2::a"),
		netip.MustParseAddr("fd06:6e7:0:1::b"),
	}
	caPEM := selfSignedCA(t)

	for _, c := range []struct {
		name      string
		env       Env
		identity  state.Node
		addresses []netip.Addr
		wantErr   bool
		wantBase  string
		wantPeer  string
		// wantOverlay is whether the client dials through the overlay. A direct
		// hop must NOT: see Direct.
		wantOverlay bool
	}{
		{
			// The CA a config document delivered wins over the one the
			// enrollment carried: an env CA nothing could read would refuse if
			// it were reached, and it is not.
			name:        "the CA in state wins over the env's",
			env:         Env{GatewayAddr: gateway, GatewayCA: "not a certificate"},
			identity:    state.Node{CAPEM: caPEM},
			wantBase:    "https://[fd06:6e7:0:1::1]:443",
			wantOverlay: true,
		},
		{
			// And before any document has arrived there is only the env's.
			name:        "the env's CA is what the first exchange has",
			env:         Env{GatewayAddr: gateway, GatewayCA: caPEM},
			wantBase:    "https://[fd06:6e7:0:1::1]:443",
			wantOverlay: true,
		},
		{
			name:    "an unpinned gateway with no explicit opt-in is refused",
			env:     Env{GatewayAddr: gateway},
			wantErr: true,
		},
		{
			// No gateway to dial through and none to attribute the connection,
			// so the socket is dialled directly and this node names itself.
			name:      "a direct hop dials the socket and names this node",
			env:       Env{GatewayAddr: loopback},
			addresses: addresses,
			wantBase:  "http://127.0.0.1:4006",
			wantPeer:  "fd06:6e7:0:1::b",
		},
		{
			name:    "a direct hop by a node holding no address is refused",
			env:     Env{GatewayAddr: loopback},
			wantErr: true,
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			base, client, err := Mothership(c.env, c.identity, nil, c.addresses)
			if c.wantErr {
				if err == nil {
					t.Fatalf("Mothership accepted %+v", c.env)
				}
				return
			}
			if err != nil {
				t.Fatalf("Mothership: %v", err)
			}
			if base != c.wantBase {
				t.Errorf("base = %q, want %q", base, c.wantBase)
			}

			transport, peer := unwrap(t, client)
			if peer != c.wantPeer {
				t.Errorf("peer header = %q, want %q", peer, c.wantPeer)
			}
			if got := dialsOverlay(t, transport); got != c.wantOverlay {
				t.Errorf("dials through the overlay = %v, want %v", got, c.wantOverlay)
			}
		})
	}
}

// dialsOverlay asks the transport which dialler it holds, by handing it an
// address that is not one.
//
// The presence of a DialContext answers nothing: Client clones
// http.DefaultTransport, which already has the machine's own. Only Dialer's
// refusal is prefixed "enroll: " — the machine's dialler complains about a
// missing port — and neither touches the network to say so.
func dialsOverlay(t *testing.T, transport *http.Transport) bool {
	t.Helper()

	_, err := transport.DialContext(context.Background(), "tcp", "not-an-address")
	if err == nil {
		t.Fatal("a dial of \"not-an-address\" was accepted")
	}

	return strings.HasPrefix(err.Error(), "enroll: ")
}

// unwrap takes the transport apart the way Client assembled it: a peerHeader
// around an *http.Transport on a direct hop, and the transport alone through a
// gateway.
func unwrap(t *testing.T, client *http.Client) (*http.Transport, string) {
	t.Helper()

	switch inner := client.Transport.(type) {
	case *http.Transport:
		return inner, ""
	case peerHeader:
		return inner.inner.(*http.Transport), inner.peer
	default:
		t.Fatalf("unexpected transport type %T", client.Transport)
		return nil, ""
	}
}

// selfSignedCA is a certificate Gateway will accept as a pinned CA. Nothing
// here is verified against it; what matters is that it parses, so that a case
// which reached the OTHER CA would refuse and be seen to.
func selfSignedCA(t *testing.T) string {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "overlay test CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
	}

	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}

	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

// The dev recipe's listener is on this machine; a real gateway is an overlay
// address. What separates them is the only thing that decides whether this
// node dials through the overlay and whether it has to name itself.
func TestDirectIsTheHopWithNoGatewayOnIt(t *testing.T) {
	developmentBuild(t)
	for _, addr := range []string{"127.0.0.1:4006", "[::1]:4006"} {
		if !Direct(netip.MustParseAddrPort(addr)) {
			t.Errorf("%s is a loopback listener and no gateway can be in front of it", addr)
		}
	}

	if Direct(netip.MustParseAddrPort("[fd06:6e7:0:1::1]:443")) {
		t.Error("an overlay gateway address was read as a direct hop")
	}
}

// A node with grants in two organizations names one of them, the same one
// every time: the listener a loopback address stands for is every
// organization's at once, so there is nothing in the destination to pick by.
func TestSelfPeerIsTheLowestAddressAndIsStable(t *testing.T) {
	addresses := []netip.Addr{
		netip.MustParseAddr("fd06:6e7:0:2::a"),
		netip.MustParseAddr("fd06:6e7:0:1::b"),
	}

	peer, err := SelfPeer(addresses)
	if err != nil {
		t.Fatalf("SelfPeer: %v", err)
	}
	if peer.String() != "fd06:6e7:0:1::b" {
		t.Fatalf("SelfPeer = %s, want the lowest of the two", peer)
	}

	reversed := []netip.Addr{addresses[1], addresses[0]}
	again, err := SelfPeer(reversed)
	if err != nil {
		t.Fatalf("SelfPeer: %v", err)
	}
	if again != peer {
		t.Fatalf("SelfPeer answered %s and then %s for the same two addresses", peer, again)
	}
}

// A node that holds no address has nothing to be attributed by, and saying so
// is better than sending an empty header for the ingress to refuse.
func TestSelfPeerRefusesANodeWithNoAddress(t *testing.T) {
	if _, err := SelfPeer(nil); err == nil {
		t.Fatal("a node with no overlay address was allowed to name itself")
	}
}

// The whole point of the direct hop: Gentility's ingress reads
// X-Overlay-Peer and resolves the node from it, and on this hop there is no
// gateway to set it.
func TestClientNamesThisNodeOnADirectHop(t *testing.T) {
	var seen []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Values(PeerHeader)
	}))
	t.Cleanup(server.Close)

	peer := netip.MustParseAddr("fd06:6e7:0:1::b")
	client := Client(nil, nil, peer)

	request, err := http.NewRequestWithContext(context.Background(), http.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	response.Body.Close()

	if len(seen) != 1 || seen[0] != peer.String() {
		t.Fatalf("the listener saw %v for %s, want exactly the node's own address", seen, PeerHeader)
	}
}

// And through a gateway it sends none. A gateway deletes what a client sent
// before setting what its WhoIs attributed, so a header here would be a claim
// this node is not entitled to make and that nothing would read.
func TestClientNamesNobodyThroughAGateway(t *testing.T) {
	var seen []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Values(PeerHeader)
	}))
	t.Cleanup(server.Close)

	client := Client(nil, nil, netip.Addr{})

	request, err := http.NewRequestWithContext(context.Background(), http.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	response.Body.Close()

	if len(seen) != 0 {
		t.Fatalf("the listener saw %v for %s, want nothing", seen, PeerHeader)
	}
}

// The connector's control-plane client must never route its own gateway/head
// traffic through an ambient proxy: the build env it launches inside points
// HTTPS_PROXY at the connector's own preflight port, which it binds only AFTER
// this client has bootstrapped. So the transport's Proxy is nil, regardless of
// hop, and never DefaultTransport's ProxyFromEnvironment.
func TestClientNeverHonoursAnAmbientProxy(t *testing.T) {
	for _, peer := range []netip.Addr{
		netip.MustParseAddr("fd06:6e7:0:1::b"),
		{},
	} {
		transport, _ := unwrap(t, Client(nil, nil, peer))

		if transport.Proxy != nil {
			t.Fatalf("Client transport for peer %v honours an ambient proxy; want Proxy == nil", peer)
		}
	}
}
