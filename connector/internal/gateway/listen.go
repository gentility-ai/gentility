package gateway

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"net/netip"
	"time"
)

// reconcileEvery is how often the gateway re-reads the addresses it holds.
//
// A revoked grant takes an address away in under a second (the spike measured
// 907 ms) and nothing pushes that fact at the node — noticing is the node's
// job. A second is short enough that a revoked organization stops being served
// about as fast as its address stops existing, and cheap enough to do forever:
// the read is a local API call inside this process.
const reconcileEvery = time.Second

// Binder is the part of tsnode.Node this loop uses.
//
// An interface rather than the concrete node so that what this loop does —
// bind what appeared, serve it, let go of what went away — can be proved
// without a head, a netstack and ninety seconds.
type Binder interface {
	Addresses(ctx context.Context) ([]netip.Addr, error)
	ListenAll(addresses []netip.Addr, port uint16) (map[netip.Addr]net.Listener, error)
}

// Listen keeps one TLS listener bound per address this gateway holds, and
// serves handler on each, until ctx is done.
//
// **Explicitly per address, never a wildcard.** A wildcard tsnet listener on a
// node holding several addresses answers on exactly one of them, and which one
// depends on netmap ordering — so a wildcard gateway would serve one tenant
// and refuse the rest in a millisecond each. The gateway holds a
// grant in every organization, which makes it the sharpest case of that.
//
// ListenAll is what closes a listener for an address that has gone; the
// goroutine serving it ends quietly when it does.
//
// A bind that fails is logged and left for the next turn. The gateway is then
// serving a subset of its addresses, which is the truth, and a loop that
// stopped on it would take down the organizations that were working.
func Listen(ctx context.Context, binder Binder, tlsConfig *tls.Config, handler http.Handler, logf func(string, ...any)) {
	if logf == nil {
		logf = func(string, ...any) {}
	}

	ticker := time.NewTicker(reconcileEvery)
	defer ticker.Stop()

	for {
		addresses, err := binder.Addresses(ctx)
		if err != nil {
			logf("gateway: reading this node's addresses: %v", err)
		} else {
			opened, err := binder.ListenAll(addresses, Port)
			if err != nil {
				logf("gateway: binding: %v", err)
			}

			for address, ln := range opened {
				logf("gateway: serving [%s]:%d", address, Port)

				go func(address netip.Addr, ln net.Listener) {
					if err := Serve(ctx, tls.NewListener(ln, tlsConfig), handler); err != nil {
						logf("gateway: [%s]:%d stopped: %v", address, Port, err)
					}
				}(address, ln)
			}
		}

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
