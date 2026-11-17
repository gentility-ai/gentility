package tsnode

// The refusal hooks: where the overlay sees a refused connection, handed to
// internal/refusal (which knows nothing of tailscale) without waiting. Three
// places see one. A destination's packet filter drops an inbound SYN no rule
// admits (refusalHook, through an ipnext extension); a sender's engine drops
// an outbound SYN no peer owns (onNoPeer, genscale's engine hook); and a
// peer's TSMP "rejected" notice says this node's own SYN was refused there
// (rejectedLogger).

import (
	"net/netip"

	"github.com/gentility-ai/gentility/connector/internal/refusal"

	"tailscale.com/ipn/ipnext"
	"tailscale.com/net/packet"
	"tailscale.com/net/tsaddr"
	"tailscale.com/net/tstun"
	"tailscale.com/tsd"
	"tailscale.com/types/ipproto"
	"tailscale.com/types/logger"
	"tailscale.com/wgengine/filter"
)

const refusalExtensionName = "gentility-refusal"

// The extension is how a filter hook reaches every packet filter the
// backend builds (LocalBackend copies the hooks onto each one), in both
// interface and userspace mode. Registered at init, as ipnext requires.
func init() {
	ipnext.RegisterExtension(refusalExtensionName, func(_ logger.Logf, sb ipnext.SafeBackend) (ipnext.Extension, error) {
		return &refusalExtension{sys: sb.Sys()}, nil
	})
}

type refusalExtension struct{ sys *tsd.System }

func (e *refusalExtension) Name() string { return refusalExtensionName }

// Init runs once, when the backend starts, after the engine has installed
// its own inbound pre-filter (the connection tracker); rejectedLogger chains
// in front of it rather than replacing it.
func (e *refusalExtension) Init(host ipnext.Host) error {
	tun, ok := e.sys.Tun.GetOK()
	if !ok {
		return ipnext.SkipExtension
	}
	host.Hooks().Filter.IngressAllowHooks.Add(refusalHook(func(a netip.Addr) (uint16, bool) {
		if f := tun.PeerAPIPort; f != nil {
			return f(a)
		}
		return 0, false
	}))
	// Safe as a plain write: Init runs before Start, with no peers and so no inbound packets yet.
	tun.PreFilterPacketInboundFromWireGuard = rejectedLogger(tun.PreFilterPacketInboundFromWireGuard)
	return nil
}

func (e *refusalExtension) Shutdown() error { return nil }

// refusalHook is the filter hook: it runs only when no rule admitted the
// packet (filter.Filter.IngressAllowHooks), records a refused connection
// attempt, and never admits anything. Not an attempt: anything but a TCP SYN
// without ACK; filter.Check's synthetic packets (source port 0); a SYN to
// port 0, which is never a real connect and which the mothership refuses; a
// SYN to an address outside 6net, which no edge names; and a SYN to the
// PeerAPI port, which tstun lets through after the filter has run.
func refusalHook(peerAPIPort func(netip.Addr) (uint16, bool)) filter.PacketMatch {
	return func(p packet.Parsed) (bool, string) {
		if !isConnect(&p) || p.Src.Port() == 0 || !tsaddr.TailscaleULARange().Contains(p.Dst.Addr()) {
			return false, ""
		}
		if port, ok := peerAPIPort(p.Dst.Addr()); ok && port == p.Dst.Port() {
			return false, ""
		}
		refusal.Offer(refusal.Attempt{Side: refusal.Destination, Src: p.Src.Addr(), Dst: p.Dst.Addr(), Port: p.Dst.Port()})
		return false, ""
	}
}

// onNoPeer is wgengine.Config.OnNoPeer: an outbound SYN to a 6net address
// no peer owns — this node holds no grant there at all. Anything else that
// finds no peer (multicast, another overlay's range, a non-SYN, port 0) is
// not a refused connection. pkt is borrowed: nothing here keeps it.
func onNoPeer(src, dst netip.Addr, pkt []byte) {
	if !tsaddr.TailscaleULARange().Contains(dst) {
		return
	}
	var p packet.Parsed
	p.Decode(pkt)
	if !isConnect(&p) {
		return
	}
	refusal.Offer(refusal.Attempt{Side: refusal.Source, Src: src, Dst: dst, Port: p.Dst.Port()})
}

// isConnect is a TCP SYN without ACK to a real port: the first packet of a
// connection.
func isConnect(p *packet.Parsed) bool {
	return p.IPProto == ipproto.TCP && p.IsTCPSyn() && p.Dst.Port() != 0
}

// rejectedLogger chains in front of whatever already filters inbound
// packets before the rules (the engine's own connection tracker), and hands
// a peer's TSMP "rejected" notice to the Reporter to log. The notice is then
// passed on unchanged.
func rejectedLogger(prev tstun.FilterFunc) tstun.FilterFunc {
	return func(p *packet.Parsed, w *tstun.Wrapper) filter.Response {
		if p.IPProto == ipproto.TSMP {
			if h, ok := p.AsTailscaleRejectedHeader(); ok && !h.MaybeBroken && h.Proto == ipproto.TCP {
				refusal.Offer(refusal.Attempt{Side: refusal.Rejected, Src: h.Src.Addr(), Dst: h.Dst.Addr(), Port: h.Dst.Port()})
			}
		}
		if prev != nil {
			return prev(p, w)
		}
		return filter.Accept
	}
}
