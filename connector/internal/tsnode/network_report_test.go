package tsnode

import (
	"net/netip"
	"testing"

	"tailscale.com/ipn/ipnstate"
	"tailscale.com/types/key"
)

func TestPeerPathsUsesEachPeerNotSelfRelay(t *testing.T) {
	for _, tc := range []struct {
		name  string
		peer  ipnstate.PeerStatus
		want  string
		relay string
		via   string
	}{
		{"direct despite DERP", ipnstate.PeerStatus{Active: true, CurAddr: "203.0.113.4:41641", Relay: "lhr"}, "direct", "", "203.0.113.4:41641"},
		{"DERP", ipnstate.PeerStatus{Active: true, Relay: "lhr"}, "derp", "lhr", ""},
		{"peer relay", ipnstate.PeerStatus{Active: true, PeerRelay: "relay:123:vni:1", Relay: "lhr"}, "peer_relay", "", ""},
		{"idle with old direct path", ipnstate.PeerStatus{CurAddr: "203.0.113.4:41641", Relay: "lhr"}, "idle", "", ""},
		{"active but unobserved", ipnstate.PeerStatus{Active: true}, "unknown", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			peer := tc.peer
			peer.TailscaleIPs = []netip.Addr{netip.MustParseAddr("fd06:6e7:0:2::4"), netip.MustParseAddr("fd06:6e7:0:3::7"), netip.MustParseAddr("100.64.0.1")}
			status := &ipnstate.Status{Self: &ipnstate.PeerStatus{Relay: "syd"}, Peer: map[key.NodePublic]*ipnstate.PeerStatus{key.NewNode().Public(): &peer}}
			got := report(status).PeerPaths
			if len(got) != 2 {
				t.Fatalf("want two org observations, got %v", got)
			}
			for _, p := range got {
				if p.Path != tc.want || p.Relay != tc.relay || p.Via != tc.via {
					t.Fatalf("wrong peer observation: %+v", p)
				}
			}
		})
	}
}

func TestLocalAddressReportsOnlyPrivatePrefixes(t *testing.T) {
	for _, tc := range []struct {
		address, host, subnet string
		include               bool
	}{
		{"192.168.1.42/24", "192.168.1.42", "192.168.1.0/24", true},
		{"10.4.0.9/16", "10.4.0.9", "10.4.0.0/16", true},
		{"172.17.0.1/16", "172.17.0.1", "172.17.0.0/16", true},
		{"fd12:3456::42/64", "fd12:3456::42", "fd12:3456::/64", true},
		{"2001:db8:1234::42/64", "", "", false},
		{"203.0.113.7/24", "", "", false},
		{"100.101.102.103/10", "", "", false},
		{"fe80::1234/64", "", "", false},
		{"169.254.1.1/16", "", "", false},
		{"127.0.0.1/8", "", "", false},
		{"::1/128", "", "", false},
		{"fd06:6e7:0:2::4/64", "", "", false},
		{"0.0.0.0/0", "", "", false},
	} {
		entry, included := localAddress("en0", netip.MustParsePrefix(tc.address))
		if included != tc.include {
			t.Fatalf("%s: include=%v", tc.address, included)
		}
		if included && (entry.Address != tc.host || entry.Subnet != tc.subnet || entry.Interface != "en0") {
			t.Fatalf("%s: %+v", tc.address, entry)
		}
	}
}

func TestPeerPathsAreCappedActiveFirst(t *testing.T) {
	peers := map[key.NodePublic]*ipnstate.PeerStatus{}
	for i := range maxPeerPaths + 10 {
		address := netip.AddrFrom16([16]byte{0xfd, 0x06, 0x06, 0xe7, 14: byte(i >> 8), 15: byte(i)})
		peers[key.NewNode().Public()] = &ipnstate.PeerStatus{
			Active:       i >= 10,
			CurAddr:      "203.0.113.4:41641",
			TailscaleIPs: []netip.Addr{address},
		}
	}

	paths, err := peerPaths(&ipnstate.Status{Peer: peers})
	if len(paths) != maxPeerPaths || err == nil {
		t.Fatalf("want %d paths and an error, got %d and %v", maxPeerPaths, len(paths), err)
	}
	for _, p := range paths {
		if p.Path != "direct" {
			t.Fatalf("an idle peer displaced an active one: %+v", p)
		}
	}
}
