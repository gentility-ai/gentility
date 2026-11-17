package tsnode

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"testing"
	"time"

	"tailscale.com/ipn/ipnstate"
	"tailscale.com/net/netcheck"
	"tailscale.com/tailcfg"
	"tailscale.com/types/key"
	"tailscale.com/types/netmap"
)

func TestUDPProbeRetainsTheProbeTimeAndUnknown(t *testing.T) {
	if udpProbe(nil) != nil {
		t.Fatal("missing probe became a negative result")
	}
	when := time.Now().Add(-time.Hour)
	got := udpProbe(&netcheck.Report{Now: when, UDP: true, IPv4: true, GlobalV4: netip.MustParseAddrPort("192.0.2.4:5678")})
	if !got.ObservedAt.Equal(when) || !got.UDP || !got.IPv4 || got.GlobalV4 != "192.0.2.4:5678" || got.GlobalV6 != "" {
		t.Fatalf("probe changed: %+v", got)
	}
}

func TestPrivateDiagnosticCapsKnownEndpointsAndPeerList(t *testing.T) {
	peers := map[key.NodePublic]*ipnstate.PeerStatus{}
	for i := 0; i < maxPeerPaths+1; i++ {
		peer := &ipnstate.PeerStatus{Active: true, TailscaleIPs: []netip.Addr{netip.MustParseAddr(fmt.Sprintf("fd06:6e7:0:1::%x", i+1))}}
		for j := 0; j < 40; j++ {
			peer.Addrs = append(peer.Addrs, fmt.Sprintf("192.0.2.1:%d", 10000+j))
		}
		peers[key.NewNode().Public()] = peer
	}
	got := report(&ipnstate.Status{Self: &ipnstate.PeerStatus{}, Peer: peers})
	got.PeerDiagnostics = peerDiagnostics(candidateMap(peers), got.PeerPaths)
	if !got.PeerPathsIncomplete || len(got.PeerDiagnostics) != maxPeerPaths {
		t.Fatalf("peer cap not marked: %d", len(got.PeerDiagnostics))
	}
	for _, peer := range got.PeerDiagnostics {
		if len(peer.Endpoints) != 32 || !peer.EndpointsTruncated {
			t.Fatalf("endpoint cap not marked: %+v", peer)
		}
	}
}

func TestPrivateDiagnosticDistinguishesIdleCandidateFromCurrentPath(t *testing.T) {
	peer := &ipnstate.PeerStatus{
		TailscaleIPs: []netip.Addr{netip.MustParseAddr("fd06:6e7:0:1::b")},
		CurAddr:      "192.0.2.4:5678", Addrs: []string{"192.0.2.4:5678", "192.0.2.4:5678"},
	}
	s := &ipnstate.Status{Self: &ipnstate.PeerStatus{}, Peer: map[key.NodePublic]*ipnstate.PeerStatus{key.NewNode().Public(): peer}}
	got := peerDiagnostics(candidateMap(s.Peer), report(s).PeerPaths)
	if len(got) != 1 || got[0].Path != "idle" || got[0].Via != "" || len(got[0].Endpoints) != 1 {
		t.Fatalf("idle endpoint was presented as active: %+v", got)
	}
	peer.Active = true
	got = peerDiagnostics(candidateMap(s.Peer), report(s).PeerPaths)
	if got[0].Path != "direct" || got[0].Via != peer.CurAddr {
		t.Fatalf("missing direct path: %+v", got)
	}
}

// Fixtures put candidates in the network map, as the real daemon does.
func candidateMap(peers map[key.NodePublic]*ipnstate.PeerStatus) *netmap.NetworkMap {
	nm := &netmap.NetworkMap{}
	for key, peer := range peers {
		n := &tailcfg.Node{Key: key}
		for _, address := range peer.TailscaleIPs {
			n.Addresses = append(n.Addresses, netip.PrefixFrom(address, address.BitLen()))
		}
		for _, endpoint := range peer.Addrs {
			n.Endpoints = append(n.Endpoints, netip.MustParseAddrPort(endpoint))
		}
		nm.Peers = append(nm.Peers, n.View())
	}
	return nm
}

func TestPrivateCandidatesDistinguishUnavailableFromEmpty(t *testing.T) {
	path := PeerPath{Address: "fd06:6e7:0:1::b", Path: "idle"}
	for _, nm := range []*netmap.NetworkMap{nil, {}} {
		if got := peerDiagnostics(nm, []PeerPath{path}); !got[0].EndpointsUnavailable {
			t.Fatal("missing map peer reported empty candidates")
		}
	}
	nm := candidateMap(map[key.NodePublic]*ipnstate.PeerStatus{key.NewNode().Public(): {TailscaleIPs: []netip.Addr{netip.MustParseAddr(path.Address)}}})
	if got := peerDiagnostics(nm, []PeerPath{path}); got[0].EndpointsUnavailable || len(got[0].Endpoints) != 0 {
		t.Fatal("known empty map reported unavailable")
	}
}

func TestCandidateReadFailureRemainsUnavailable(t *testing.T) {
	failure := errors.New("map read failed")
	n := &Node{
		status: func(context.Context) (*ipnstate.Status, error) {
			return &ipnstate.Status{
				Self: &ipnstate.PeerStatus{},
				Peer: map[key.NodePublic]*ipnstate.PeerStatus{key.NewNode().Public(): {TailscaleIPs: []netip.Addr{netip.MustParseAddr("fd06:6e7:0:1::b")}}},
			}, nil
		},
		peerMap: func(context.Context) (*netmap.NetworkMap, error) { return nil, failure },
	}
	got, err := n.DiagnosticStatus(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !errors.Is(got.ObservationsErr, failure) || len(got.PeerDiagnostics) != 1 || !got.PeerDiagnostics[0].EndpointsUnavailable {
		t.Fatalf("candidate failure hidden: %+v", got)
	}
	if got.PeerPathsIncomplete {
		t.Fatal("candidate read failure made path inventory incomplete")
	}
}

func TestStatusDoesNotReadPrivatePeerMap(t *testing.T) {
	reads := 0
	n := &Node{
		status: func(context.Context) (*ipnstate.Status, error) {
			return &ipnstate.Status{Self: &ipnstate.PeerStatus{}}, nil
		},
		peerMap: func(context.Context) (*netmap.NetworkMap, error) {
			reads++
			return &netmap.NetworkMap{}, nil
		},
	}
	if _, err := n.Status(context.Background()); err != nil {
		t.Fatal(err)
	}
	if reads != 0 {
		t.Fatalf("ordinary status read private peer map %d times", reads)
	}
	if _, err := n.DiagnosticStatus(context.Background()); err != nil {
		t.Fatal(err)
	}
	if reads != 1 {
		t.Fatalf("diagnostic status read private peer map %d times, want 1", reads)
	}
}
