//go:build darwin

package tsnode

import "tailscale.com/net/netmon"

// HintDefaultRoute tells the library which interface carries the default
// route. On Apple platforms the library cannot trust its own reading of the
// routing table, so the fork's netmon learns the default route from the app,
// as Tailscale's own app tells it from NWPathMonitor. The ios build satisfies
// the darwin constraint.
func HintDefaultRoute(ifName string) { netmon.UpdateLastKnownDefaultRouteInterface(ifName) }
