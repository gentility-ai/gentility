package egress

import (
	"context"
	"net"
	"net/netip"
	"reflect"
	"testing"

	"github.com/gentility-ai/gentility/connector/internal/config"
	"github.com/gentility-ai/gentility/connector/internal/gateway"
	"github.com/gentility-ai/gentility/connector/internal/tsnode"
)

// NewConfig is a pure reading of the config document: everything the relay acts
// on comes from the document, and nothing here decides anything. The node is
// nil in these tests because the only thing NewConfig asks of it is the method
// value behind Peers, which these never call.

func TestNewConfigCarriesTheDocumentsDataHalf(t *testing.T) {
	document := &config.Document{
		Grants: []tsnode.ConfigGrant{
			{Org: "acme", Address: "fd06:6e7:0:1::2"},
			{Org: "beta", Address: "fd06:6e7:0:2::2"},
		},
		Services: []config.Service{
			{Kind: "egress", Org: "acme", Port: 4180, State: "declared"},
			{Kind: "egress", Org: "beta", Port: 4181, State: "approved"},
			{Kind: "gateway", Org: "acme", Port: 4443, State: "approved"},
		},
	}

	cfg := NewConfig(document, nil, nil)

	wantAddresses := map[string]string{
		"acme": "fd06:6e7:0:1::2",
		"beta": "fd06:6e7:0:2::2",
	}
	if !reflect.DeepEqual(cfg.Addresses, wantAddresses) {
		t.Fatalf("Addresses = %v, want %v", cfg.Addresses, wantAddresses)
	}

	// Every service, in the document's order and with its state intact:
	// StartServices is the one that judges kind and approval, so a Config that
	// pre-filtered would be deciding on the mothership's behalf.
	wantServices := []ServiceConfig{
		{Kind: "egress", Org: "acme", Port: 4180, State: "declared"},
		{Kind: "egress", Org: "beta", Port: 4181, State: "approved"},
		{Kind: "gateway", Org: "acme", Port: 4443, State: "approved"},
	}
	if !reflect.DeepEqual(cfg.Services, wantServices) {
		t.Fatalf("Services = %v, want %v", cfg.Services, wantServices)
	}

	if cfg.Peers == nil || cfg.Dial == nil || cfg.Lookup == nil {
		t.Fatalf("a seam was left nil: Peers=%v Dial=%v Lookup=%v",
			cfg.Peers != nil, cfg.Dial != nil, cfg.Lookup != nil)
	}
}

func TestNewConfigLiteralAllow(t *testing.T) {
	rows := []struct {
		name string
		// egress is the document's egress block: nil for a pure relay server
		// node, which holds no path to a relay of its own.
		egress *config.Egress
		want   []string
	}{
		{"no egress block fail-closes on literals", nil, nil},
		{"an egress block with no literals allows none", &config.Egress{}, nil},
		{
			"the block's literals are what the relay acts on",
			&config.Egress{LiteralAllow: []string{"203.0.113.4", "198.51.100.7"}},
			[]string{"203.0.113.4", "198.51.100.7"},
		},
	}

	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			cfg := NewConfig(&config.Document{Egress: row.egress}, nil, nil)
			if !reflect.DeepEqual(cfg.LiteralAllow, row.want) {
				t.Fatalf("LiteralAllow = %v, want %v", cfg.LiteralAllow, row.want)
			}
		})
	}
}

// overlayPeers is fix A: the relay's WhoIs must mirror the gateway's Attributor
// — return the address the connection ACTUALLY came from, having verified the
// head attributes it to the peer, and refuse anything else. These tests drive
// the adapter over the same fake whois shape internal/gateway's own tests use,
// so a node holding addresses in two orgs is attributed to the one it connected
// from and never to a neighbour's.

func peersFor(local netip.Addr, whois func(context.Context, netip.AddrPort, netip.Addr) (tsnode.Peer, error)) overlayPeers {
	return overlayPeers{attribute: gateway.Attributor(whois), local: local}
}

func TestOverlayPeersReturnsTheConnectingAddress(t *testing.T) {
	local := netip.MustParseAddr("fd06:6e7:0:1::1")
	remote := &net.TCPAddr{IP: net.ParseIP("fd06:6e7:0:1::2"), Port: 41000}

	peers := peersFor(local, func(context.Context, netip.AddrPort, netip.Addr) (tsnode.Peer, error) {
		// The same node holds an address in a second org; the loose answer would
		// be to return one of these rather than the one it connected from.
		return tsnode.Peer{
			Name: "n2.acme.overlay.internal.",
			Addresses: []netip.Addr{
				netip.MustParseAddr("fd06:6e7:0:1::2"),
				netip.MustParseAddr("fd06:6e7:0:2::2"),
			},
		}, nil
	})

	got, err := peers.WhoIs(context.Background(), remote)
	if err != nil {
		t.Fatalf("WhoIs: %v", err)
	}
	if want := netip.MustParseAddr("fd06:6e7:0:1::2"); got != want {
		t.Fatalf("attributed %s, want the connecting address %s (not a neighbour's)", got, want)
	}
}

func TestOverlayPeersRefusesAnAddressThePeerDoesNotHold(t *testing.T) {
	local := netip.MustParseAddr("fd06:6e7:0:1::1")
	remote := &net.TCPAddr{IP: net.ParseIP("fd06:6e7:0:1::2"), Port: 41000}

	peers := peersFor(local, func(context.Context, netip.AddrPort, netip.Addr) (tsnode.Peer, error) {
		return tsnode.Peer{
			Name:      "someone-else.acme.overlay.internal.",
			Addresses: []netip.Addr{netip.MustParseAddr("fd06:6e7:0:1::9")},
		}, nil
	})

	if _, err := peers.WhoIs(context.Background(), remote); err == nil {
		t.Fatal("an address the attributed peer does not hold was accepted — the relay would misattribute it")
	}
}

func TestOverlayPeersPassesCrossOrgRefusalThrough(t *testing.T) {
	local := netip.MustParseAddr("fd06:6e7:0:1::1")
	remote := &net.TCPAddr{IP: net.ParseIP("fd06:6e7:0:2::2"), Port: 41000}

	peers := peersFor(local, func(context.Context, netip.AddrPort, netip.Addr) (tsnode.Peer, error) {
		return tsnode.Peer{}, tsnode.ErrCrossOrgPeer
	})

	if _, err := peers.WhoIs(context.Background(), remote); err == nil {
		t.Fatal("a cross-org peer was attributed anyway")
	}
}
