package tsnode

import (
	"net/netip"
	"testing"
	"time"

	"tailscale.com/ipn/ipnstate"
	"tailscale.com/types/key"
)

// A peer holds one WireGuard counter pair however many organizations it is
// in. Its bytes go to one address — its lowest in a /64 this node also holds —
// and a peer sharing no organization with this node is not reported at all.
func TestPeerCountersNameEachPeerByOneSharedOrgAddress(t *testing.T) {
	self := &ipnstate.PeerStatus{TailscaleIPs: []netip.Addr{
		netip.MustParseAddr("fd06:6e7:0:3::4"),
		netip.MustParseAddr("fd06:6e7:0:2::4"),
		netip.MustParseAddr("100.64.0.9"),
	}}
	handshake := time.Unix(1_793_433_590, 0)
	gateway := &ipnstate.PeerStatus{
		TailscaleIPs: []netip.Addr{
			netip.MustParseAddr("fd06:6e7:0:3::1"),
			netip.MustParseAddr("fd06:6e7:0:2::1"),
			netip.MustParseAddr("fd06:6e7:0:7::1"),
			netip.MustParseAddr("100.64.0.1"),
		},
		RxBytes: 1200, TxBytes: 300, LastHandshake: handshake,
	}
	stranger := &ipnstate.PeerStatus{
		TailscaleIPs: []netip.Addr{netip.MustParseAddr("fd06:6e7:0:9::7")},
		RxBytes:      5,
	}
	status := &ipnstate.Status{Self: self, Peer: map[key.NodePublic]*ipnstate.PeerStatus{
		key.NewNode().Public(): gateway,
		key.NewNode().Public(): stranger,
	}}

	got := peerCounters(status)
	if len(got) != 1 {
		t.Fatalf("want one counted peer, got %+v", got)
	}
	if got[0].Address != "fd06:6e7:0:2::1" || got[0].Rx != 1200 || got[0].Tx != 300 || !got[0].LastHandshake.Equal(handshake) {
		t.Fatalf("wrong counter: %+v", got[0])
	}
}
