package tsnode

import (
	"net/netip"
	"testing"

	"github.com/gentility-ai/gentility/connector/internal/refusal"

	"tailscale.com/net/packet"
	"tailscale.com/net/tstun"
	"tailscale.com/types/ipproto"
	"tailscale.com/wgengine/filter"
)

func tcp6(src, dst netip.Addr, sport, dport uint16, flags packet.TCPFlag) []byte {
	tcp := make([]byte, 20)
	tcp[0], tcp[1] = byte(sport>>8), byte(sport)
	tcp[2], tcp[3] = byte(dport>>8), byte(dport)
	tcp[12] = 5 << 4
	tcp[13] = byte(flags)
	return packet.Generate(packet.IP6Header{IPProto: ipproto.TCP, Src: src, Dst: dst}, tcp)
}

func udp6(src, dst netip.Addr, sport, dport uint16) []byte {
	udp := make([]byte, 8)
	udp[0], udp[1] = byte(sport>>8), byte(sport)
	udp[2], udp[3] = byte(dport>>8), byte(dport)
	udp[5] = 8
	return packet.Generate(packet.IP6Header{IPProto: ipproto.UDP, Src: src, Dst: dst}, udp)
}

func parsed(b []byte) packet.Parsed {
	var p packet.Parsed
	p.Decode(b)
	return p
}

var (
	refusalMac   = netip.MustParseAddr("fd06:6e7:0:2::4")
	refusalCedar = netip.MustParseAddr("fd06:6e7:0:2::5")
)

// drain empties refusal's shared queue so each test starts clean.
func drain() []refusal.Attempt {
	var got []refusal.Attempt
	for {
		select {
		case a := <-refusal.Attempts():
			got = append(got, a)
		default:
			refusal.Lost()
			return got
		}
	}
}

func noPeerAPI(netip.Addr) (uint16, bool) { return 0, false }

func TestTheFilterHookRecordsARefusedSYNAndNeverAdmits(t *testing.T) {
	drain()
	hook := refusalHook(noPeerAPI)
	if ok, _ := hook(parsed(tcp6(refusalMac, refusalCedar, 51000, 8080, packet.TCPSyn))); ok {
		t.Fatal("the hook admitted a packet")
	}
	got := drain()
	if len(got) != 1 || got[0] != (refusal.Attempt{Side: refusal.Destination, Src: refusalMac, Dst: refusalCedar, Port: 8080}) {
		t.Fatalf("got %+v", got)
	}
}

func TestTheFilterHookIgnoresWhatIsNotARefusedConnection(t *testing.T) {
	drain()
	peerAPI := func(a netip.Addr) (uint16, bool) { return 45000, a == refusalCedar }
	hook := refusalHook(peerAPI)
	for name, b := range map[string][]byte{
		"syn-ack":      tcp6(refusalMac, refusalCedar, 51000, 8080, packet.TCPSynAck),
		"rst":          tcp6(refusalMac, refusalCedar, 51000, 8080, packet.TCPRst),
		"ack":          tcp6(refusalMac, refusalCedar, 51000, 8080, packet.TCPAck),
		"peerapi":      tcp6(refusalMac, refusalCedar, 51000, 45000, packet.TCPSyn),
		"synthetic":    tcp6(refusalMac, refusalCedar, 0, 8080, packet.TCPSyn), // filter.Check's
		"dest-port-0":  tcp6(refusalMac, refusalCedar, 51000, 0, packet.TCPSyn),
		"udp":          udp6(refusalMac, refusalCedar, 51000, 8080),
		"outside-6net": tcp6(refusalMac, netip.MustParseAddr("fd7a:115c:a1e0::1"), 51000, 8080, packet.TCPSyn),
	} {
		hook(parsed(b))
		if got := drain(); len(got) != 0 {
			t.Errorf("%s recorded %+v", name, got)
		}
	}
	// filter.Check itself, end to end: its packet has source port 0.
	f := filter.NewAllowNone(nil, nil)
	f.IngressAllowHooks = []filter.PacketMatch{hook}
	f.CheckTCP(refusalMac, refusalCedar, 8080)
	if got := drain(); len(got) != 0 {
		t.Errorf("filter.CheckTCP recorded %+v", got)
	}
}

func TestNoPeerRecordsOnly6netSYNs(t *testing.T) {
	drain()
	outside := netip.MustParseAddr("fd7a:115c:a1e0::1")
	onNoPeer(refusalMac, refusalCedar, tcp6(refusalMac, refusalCedar, 51000, 22, packet.TCPSyn))
	onNoPeer(refusalMac, outside, tcp6(refusalMac, outside, 51000, 22, packet.TCPSyn))
	onNoPeer(refusalMac, refusalCedar, tcp6(refusalMac, refusalCedar, 51000, 22, packet.TCPAck))
	onNoPeer(refusalMac, refusalCedar, tcp6(refusalMac, refusalCedar, 51000, 0, packet.TCPSyn))
	onNoPeer(refusalMac, refusalCedar, udp6(refusalMac, refusalCedar, 51000, 22))
	got := drain()
	if len(got) != 1 || got[0] != (refusal.Attempt{Side: refusal.Source, Src: refusalMac, Dst: refusalCedar, Port: 22}) {
		t.Fatalf("got %+v", got)
	}
}

func TestAPeersRejectedNoticeIsHandedOnAndPassedToTheNextFilter(t *testing.T) {
	drain()
	rj := packet.TailscaleRejectedHeader{
		IPSrc: refusalCedar, IPDst: refusalMac,
		Src: netip.AddrPortFrom(refusalMac, 51000), Dst: netip.AddrPortFrom(refusalCedar, 8080),
		Proto: ipproto.TCP, Reason: packet.RejectedDueToACLs,
	}
	called := false
	next := func(*packet.Parsed, *tstun.Wrapper) filter.Response { called = true; return filter.DropSilently }
	p := parsed(packet.Generate(rj, nil))
	if res := rejectedLogger(next)(&p, nil); res != filter.DropSilently || !called {
		t.Fatalf("the previous filter was not chained: %v %v", res, called)
	}
	got := drain()
	if len(got) != 1 || got[0] != (refusal.Attempt{Side: refusal.Rejected, Src: refusalMac, Dst: refusalCedar, Port: 8080}) {
		t.Fatalf("got %+v", got)
	}
}
