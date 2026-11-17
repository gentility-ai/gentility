package phonenode

import (
	"errors"
	"fmt"
	"net"
	"net/netip"

	"github.com/gentility-ai/gentility/connector/internal/tsnode"
)

// DefaultRoute tells the library which interface the phone's default route is
// on. The library pins its sockets to that interface (IP_BOUND_IF) and,
// without the hint, guesses it from the routing table; Tailscale's own app
// sends the hint from NWPathMonitor, and so does ours, at start and on every
// path change. The answer is {"ok":true}, or malformed for an empty name.
//
// It also logs what the interface carries, as counts by kind and never as
// addresses: that one line answers whether this phone's Wi-Fi has a ULA.
func DefaultRoute(ifName string, log Logger) string {
	if ifName == "" {
		return failure(codeMalformed, errors.New("default route: no interface name"))
	}
	tsnode.HintDefaultRoute(ifName)
	logger(log)("phonenode: default route on %s: %s", ifName, describeInterface(ifName, interfaceAddresses))
	return answer(map[string]any{"ok": true})
}

// interfaceAddresses is the OS's answer for one interface's addresses. Its
// error names the interface at most, never an address.
func interfaceAddresses(name string) ([]netip.Addr, error) {
	iface, err := net.InterfaceByName(name)
	if err != nil {
		return nil, err
	}
	addrs, err := iface.Addrs()
	if err != nil {
		return nil, err
	}
	return addressesOf(addrs), nil
}

// addressesOf keeps each listed address that converts, and skips the rest:
// one odd entry, a non-contiguous mask say, must not blank the counts, and
// nothing here may fail with an address in its words.
func addressesOf(addrs []net.Addr) []netip.Addr {
	addresses := make([]netip.Addr, 0, len(addrs))
	for _, addr := range addrs {
		ipnet, ok := addr.(*net.IPNet)
		if !ok {
			continue
		}
		if address, ok := netip.AddrFromSlice(ipnet.IP); ok {
			addresses = append(addresses, address)
		}
	}
	return addresses
}

// describeInterface is the log line's tail: counts, or why there are none.
func describeInterface(name string, byName func(string) ([]netip.Addr, error)) string {
	addresses, err := byName(name)
	if err != nil {
		return fmt.Sprintf("cannot be read: %v", err)
	}
	v4, global6, ula6 := addressKinds(addresses)
	return fmt.Sprintf("%d IPv4, %d global IPv6, %d ULA", v4, global6, ula6)
}

// addressKinds counts an interface's addresses the way the library sorts
// them: IPv4, global IPv6, and ULA (fc00::/7) outside 6net's own range.
// Link-local and 6net's overlay addresses count as nothing. The overlay
// range is read from the library through tsnode, so it lives in one place.
func addressKinds(addresses []netip.Addr) (v4, global6, ula6 int) {
	overlay := tsnode.OverlayRange()
	for _, address := range addresses {
		switch {
		case address.Is4() || address.Is4In6():
			v4++
		case address.IsLinkLocalUnicast() || overlay.Contains(address):
		case address.IsPrivate():
			ula6++
		case address.IsGlobalUnicast():
			global6++
		}
	}
	return v4, global6, ula6
}
