package gateway

import (
	"context"
	"errors"
	"net/netip"
	"strings"
	"testing"

	"github.com/gentility-ai/gentility/connector/internal/tsnode"
)

func TestAttributorNamesTheAddressTheConnectionCameFrom(t *testing.T) {
	remote := netip.MustParseAddrPort("[fd06:6e7:0:1::2]:41000")
	local := netip.MustParseAddr("fd06:6e7:0:1::1")

	attribute := Attributor(func(context.Context, netip.AddrPort, netip.Addr) (tsnode.Peer, error) {
		return tsnode.Peer{
			Name: "n1.acme.overlay.internal.",
			Addresses: []netip.Addr{
				netip.MustParseAddr("fd06:6e7:0:1::2"),
				netip.MustParseAddr("fd06:6e7:0:2::2"),
			},
		}, nil
	})

	peer, err := attribute(context.Background(), remote, local)
	if err != nil {
		t.Fatalf("attributing: %v", err)
	}
	if peer != remote.Addr() {
		t.Errorf("attributed %s, want the address the connection came from, %s", peer, remote.Addr())
	}
}

// WhoIs holds the org boundary, and its refusal is the gateway's refusal. A
// cross-org peer never becomes an address this gateway would sign.
func TestAttributorPassesWhoIsRefusalsThrough(t *testing.T) {
	attribute := Attributor(func(context.Context, netip.AddrPort, netip.Addr) (tsnode.Peer, error) {
		return tsnode.Peer{}, tsnode.ErrCrossOrgPeer
	})

	_, err := attribute(context.Background(),
		netip.MustParseAddrPort("[fd06:6e7:0:2::2]:41000"),
		netip.MustParseAddr("fd06:6e7:0:1::1"))

	if !errors.Is(err, tsnode.ErrCrossOrgPeer) {
		t.Errorf("attributing a cross-org peer answered %v", err)
	}
}

// The question WhoIs does not ask: the peer it named does not hold the address
// the connection actually came from. Nothing downstream would notice — the
// listener resolves a node from the address alone — so it is refused here.
func TestAttributorRefusesAnAddressThePeerDoesNotHold(t *testing.T) {
	attribute := Attributor(func(context.Context, netip.AddrPort, netip.Addr) (tsnode.Peer, error) {
		return tsnode.Peer{
			Name:      "someone-else.acme.overlay.internal.",
			Addresses: []netip.Addr{netip.MustParseAddr("fd06:6e7:0:1::9")},
		}, nil
	})

	_, err := attribute(context.Background(),
		netip.MustParseAddrPort("[fd06:6e7:0:1::2]:41000"),
		netip.MustParseAddr("fd06:6e7:0:1::1"))

	if err == nil {
		t.Fatal("an address the attributed peer does not hold was accepted")
	}
	if !strings.Contains(err.Error(), "fd06:6e7:0:1::2") {
		t.Errorf("the refusal does not say which address it was about: %v", err)
	}
}

// A peer with no addresses at all is the same refusal, spelled the way an
// empty netmap entry would arrive.
func TestAttributorRefusesAPeerWithNoAddresses(t *testing.T) {
	attribute := Attributor(func(context.Context, netip.AddrPort, netip.Addr) (tsnode.Peer, error) {
		return tsnode.Peer{Name: "nameless"}, nil
	})

	if _, err := attribute(context.Background(),
		netip.MustParseAddrPort("[fd06:6e7:0:1::2]:41000"),
		netip.MustParseAddr("fd06:6e7:0:1::1")); err == nil {
		t.Error("a peer holding no addresses was attributed anyway")
	}
}
