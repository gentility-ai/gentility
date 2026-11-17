package gateway

import (
	"context"
	"fmt"
	"net/netip"
	"slices"

	"github.com/gentility-ai/gentility/connector/internal/tsnode"
)

// Attribute names the overlay address a connection came from, or refuses it.
//
// remote is the peer as the connection gives it; local is the address of the
// listener it reached, which is what names the organization the connection is
// in. Both are needed because attribution and the org boundary are two
// questions and the second one is the whole reason a multi-tenant gateway can
// hold one listener per address rather than one process per tenant.
//
// A refusal closes the connection. There is no "attribute it as best we can":
// the address this answers is the address the gateway is about to sign, and a
// signature over a guess is worse than no connection.
type Attribute func(ctx context.Context, remote netip.AddrPort, local netip.Addr) (netip.Addr, error)

// Attributor is the gateway's attribution, built over tsnode's.
//
// whois is `(*tsnode.Node).WhoIs` — a method value in the connector, a plain
// function in the tests. It answers two of the three questions: the head knows
// this peer, and the peer reached a listener in an organization it holds a
// grant in (tsnode.ErrCrossOrgPeer is the refusal for the second).
//
// The third question is this function's, and it is the one that makes the
// answer safe to sign: is the address the connection ACTUALLY came from one
// the head says that peer holds? WhoIs resolved the peer from that address, so
// in the ordinary case it is — but "in the ordinary case" is not a property to
// build a signature on, and the listener downstream resolves a node from the
// address alone.
func Attributor(whois func(context.Context, netip.AddrPort, netip.Addr) (tsnode.Peer, error)) Attribute {
	return func(ctx context.Context, remote netip.AddrPort, local netip.Addr) (netip.Addr, error) {
		peer, err := whois(ctx, remote, local)
		if err != nil {
			return netip.Addr{}, err
		}

		address := remote.Addr()
		if !slices.Contains(peer.Addresses, address) {
			return netip.Addr{}, fmt.Errorf(
				"gateway: the connection from %s is attributed to %q, which the head says holds %v — "+
					"so there is no address here this gateway is willing to sign",
				address, peer.Name, peer.Addresses,
			)
		}

		return address, nil
	}
}
