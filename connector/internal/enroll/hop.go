package enroll

import (
	"fmt"
	"net/http"
	"net/netip"
	"slices"

	"github.com/gentility-ai/gentility/connector/internal/state"
	"github.com/gentility-ai/gentility/connector/internal/tsnode"
	"github.com/gentility-ai/gentility/connector/internal/version"
)

// PeerHeader is the header the gateway sets to say which overlay address a
// connection came from. Gentility reads it and nothing else:
// the overlay ingress handler resolves the node from the address, so
// this is the whole of a node's identity before its credential is checked.
const PeerHeader = "X-Overlay-Peer"

// Mothership is where Gentility answers and the client every call to it is
// made with — the whole of the hop, resolved in one place.
//
// Every command reaches Gentility this way: `join`, `egress` and `proxy`
// through cmd/connector's identity, and `serve` through internal/serve. A
// second copy of it would be a second answer to what a connector is allowed to
// trust, so there is one.
//
// The CA the identity holds wins. It came from a config document this node
// applied, so a node that has been handed the gateway's CA must not fall back
// to the material its first join was allowed; OVERLAY_GATEWAY_CA is for that
// first exchange, before any document has arrived to hold one (see
// Env.GatewayCA). An identity that has not exchanged yet is state.Node{}, which
// leaves the env's CA as the only one there is.
//
// addresses are the ones the head has just attributed. They are read only on a
// direct hop, where this node has to name itself because there is no gateway to
// name it — see Direct.
func Mothership(env Env, identity state.Node, node *tsnode.Node, addresses []netip.Addr) (string, *http.Client, error) {
	caPEM := identity.CAPEM
	if caPEM == "" {
		caPEM = env.GatewayCA
	}

	base, tlsConfig, err := Gateway(env.GatewayAddr, caPEM, env.GatewayInsecure)
	if err != nil {
		return "", nil, err
	}

	dial := Dialer(node)
	var peer netip.Addr

	// No overlay hop and no gateway: the listener is on this machine, and
	// dialling it through the netstack would ask tsnode for the source address
	// of a destination that is in no organization's /64.
	if Direct(env.GatewayAddr) {
		dial = nil
		if peer, err = SelfPeer(addresses); err != nil {
			return "", nil, err
		}
	}

	return base, Client(dial, tlsConfig, peer), nil
}

// Direct reports whether Gentility is reached with no gateway in between.
//
// A loopback gateway address is the dev recipe — Gateway resolves it to plain
// http for the same reason — and it means the overlay listener is on this
// machine. Both of the things a gateway does then have nobody left to do them:
// the overlay hop a connection is dialled through, and the X-Overlay-Peer
// header that attributes it. So a direct hop dials the socket itself and names
// its own address.
//
// That is exactly what the development loopback handler exists to
// believe, and it believes it only from a loopback client on a deployment that
// has set OVERLAY_GATEWAY_INSECURE. A node cannot talk its way past a real
// gateway with this: it deletes every client-supplied X-Overlay-Peer before
// setting the one its own WhoIs attributed.
func Direct(addr netip.AddrPort) bool { return version.Connector == "dev" && addr.Addr().IsLoopback() }

// SelfPeer is the address a node names as its own on a direct hop.
//
// tsnode.SourceFor cannot answer this. It picks the source by the
// DESTINATION's /64, and a loopback listener has no /64 to pick by — the dev
// recipe's one listener stands for every organization at once.
//
// So the lowest address is taken. Every address this node holds resolves to
// this same node, which is all the exchange, the config pull and the heartbeat
// ask of one; and the lowest is stable, so a node names the same address on
// every restart rather than whichever the netmap happened to list first.
func SelfPeer(addresses []netip.Addr) (netip.Addr, error) {
	if len(addresses) == 0 {
		return netip.Addr{}, fmt.Errorf(
			"enroll: this node holds no overlay address, so it has nothing to name itself as",
		)
	}

	return slices.MinFunc(addresses, netip.Addr.Compare), nil
}

// peerHeader names this node on a hop with no gateway on it.
//
// It is a transport rather than a line in each of the three request builders
// because all three calls need it and only the client knows whether there is a
// gateway between this node and the listener. Everything else about a request
// stays where it is written.
type peerHeader struct {
	peer  string
	inner http.RoundTripper
}

// RoundTrip sets the header on a copy: a RoundTripper may not modify the
// request it is handed, and the caller may still be holding it.
func (p peerHeader) RoundTrip(request *http.Request) (*http.Response, error) {
	cloned := request.Clone(request.Context())
	cloned.Header.Set(PeerHeader, p.peer)

	return p.inner.RoundTrip(cloned)
}
