package egress

import (
	"context"
	"fmt"
	"net"
	"net/netip"

	"github.com/gentility-ai/gentility/connector/internal/config"
	"github.com/gentility-ai/gentility/connector/internal/gateway"
	"github.com/gentility-ai/gentility/connector/internal/tsnode"
)

// This file is the seam between a config document and the relay: everything a
// caller needs to turn "what the mothership says this node may serve" into a
// running relay, and nothing about how the caller is supervised.
//
// It has one home because it has one meaning. Both `connector egress` (the
// live-test command) and `connector serve` (the long-lived server) wire the
// same relay from the same document, and a second copy of this would be a
// second answer to what the relay is allowed to do.

// NewConfig turns a config document into the relay wiring's Config, filling the
// tsnet-backed seams with the real implementations.
func NewConfig(document *config.Document, node *tsnode.Node, admit *AdmitClient) Config {
	addresses := make(map[string]string, len(document.Grants))
	for _, grant := range document.Grants {
		addresses[grant.Org] = grant.Address
	}

	services := make([]ServiceConfig, 0, len(document.Services))
	for _, service := range document.Services {
		services = append(services, ServiceConfig{
			Kind:  service.Kind,
			Org:   service.Org,
			Port:  service.Port,
			State: service.State,
		})
	}

	// LiteralAllow is a permission the relay acts on, not display like
	// preflight_allow. The config document carries it in the egress block, which
	// the renderer emits for a node that holds a path to a relay; a pure relay
	// server node has none, and its LiteralAllow is then empty, which fail-closes
	// a bare IP literal — literals are refused unless allowlisted.
	var literals []string
	if document.Egress != nil {
		literals = document.Egress.LiteralAllow
	}

	attribute := gateway.Attributor(node.WhoIs)

	return Config{
		Services:  services,
		Addresses: addresses,
		Admit:     admit,
		Peers: func(local netip.Addr) PeerResolver {
			return overlayPeers{attribute: attribute, local: local}
		},
		// An internet dialler, not the overlay netstack: the relay forwards to
		// internet-routable destinations only.
		Dial:         (&net.Dialer{}).DialContext,
		Lookup:       resolveHost,
		LiteralAllow: literals,
	}
}

// OverlayListeners is the real ListenerFactory: one explicit per-address
// listener through the netstack, which tsnode already refuses to make a
// wildcard.
type OverlayListeners struct{ Node *tsnode.Node }

func (l OverlayListeners) Listen(addr netip.Addr, port uint16) (net.Listener, error) {
	return l.Node.Listen(addr, port)
}

// overlayPeers is the relay's real peer attribution (fix A). It reuses
// internal/gateway's Attributor unchanged: the same three questions the gateway
// asks of an inbound overlay connection are the ones a signature — or here, an
// admit decision — must be safe to make.
//
// The head knows the peer and the peer reached an org it holds a grant in
// (tsnode.WhoIs answers those two); Attributor answers the third and the
// load-bearing one — the address the connection ACTUALLY came from is one the
// head says that peer holds — and returns THAT address, never a canonical or
// primary one. A node holding addresses in more than one org and connecting
// from addrA is attributed as addrA, so Admit judges it against addrA's org
// and not a neighbour's. Anything else is refused, which the relay treats as an
// unattributable peer.
type overlayPeers struct {
	attribute gateway.Attribute
	local     netip.Addr
}

func (o overlayPeers) WhoIs(ctx context.Context, remote net.Addr) (netip.Addr, error) {
	ap, err := netip.ParseAddrPort(remote.String())
	if err != nil {
		return netip.Addr{}, fmt.Errorf("egress: peer address %q is not an overlay address and port: %w", remote, err)
	}
	return o.attribute(ctx, ap, o.local)
}

// resolveHost resolves a CONNECT hostname to its addresses, once. The relay
// dials exactly what this returns and never re-resolves, so there is no
// DNS-rebinding window between the hygiene check and the dial.
func resolveHost(ctx context.Context, host string) ([]netip.Addr, error) {
	return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
}
