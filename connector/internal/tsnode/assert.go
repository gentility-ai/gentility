package tsnode

import (
	"context"
	"fmt"
	"net/netip"

	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"tailscale.com/tsnet"
	"tailscale.com/wgengine/netstack"
)

// The compile-time half of the assertion. If a library bump removes or
// re-signs DialContextTCPWithBind, this method expression stops compiling and
// the build fails at the earliest possible moment — before anything ships.
//
// It is deliberately spelled with the full signature so a CHANGED signature
// fails here too, rather than compiling and misbehaving at runtime.
var _ func(*netstack.Impl, context.Context, netip.Addr, netip.AddrPort) (*gonet.TCPConn, error) = (*netstack.Impl).DialContextTCPWithBind

// AssertNetstack is the runtime half: tsnet.Server.Sys().Netstack.Get() is
// typed as a four-method interface, and the value being a *netstack.Impl is
// what makes DialBound possible. That is not promised by tsnet's API, so a
// node says so out loud at startup rather than discovering it on the first
// cross-org dial.
func AssertNetstack(srv *tsnet.Server) error {
	_, err := netstackImpl(srv)
	return err
}

// netstackImpl reaches the concrete netstack behind tsnet's four-method
// interface, or says why it cannot. Every path out of here names
// DialContextTCPWithBind, because that method is the only reason the reach
// exists and the only thing lost when it fails.
//
// It never asks for a value that has not been set: tsnet.Server.Sys() is nil
// until the server is brought up, and tsd.SubSystem.Get panics rather than
// answering. A node that is merely not started yet must get an error it can
// report, not a stack trace.
func netstackImpl(srv *tsnet.Server) (*netstack.Impl, error) {
	sys := srv.Sys()
	if sys == nil {
		return nil, fmt.Errorf(
			"tsnode: this tsnet server has no subsystems, so DialContextTCPWithBind is " +
				"unreachable — it was never brought up",
		)
	}

	value, ok := sys.Netstack.GetOK()
	if !ok {
		return nil, fmt.Errorf(
			"tsnode: this tsnet server has no netstack, so DialContextTCPWithBind is " +
				"unreachable — it was never brought up",
		)
	}

	impl, ok := value.(*netstack.Impl)
	if !ok {
		return nil, fmt.Errorf(
			"tsnode: Sys().Netstack is %T, not *netstack.Impl — DialContextTCPWithBind is "+
				"unreachable, so every outbound connection would leave from this node's "+
				"primary address and be dropped by other orgs' filters. The pinned "+
				"tailscale version has moved; do not ship this binary",
			value,
		)
	}

	return impl, nil
}
