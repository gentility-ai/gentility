// Package egress implements the `egress` service — a CONNECT relay whose every
// admission is a synchronous call to Gentility — and the unauthoritative local
// preflight proxy that a case node points HTTPS_PROXY at.
//
// Only public internet destinations are eligible for relay traffic.
package egress

import (
	"fmt"
	"net/netip"
)

// refused holds every prefix a relay must never forward to, whatever name
// resolved to it. The relay refuses link-local, loopback, ULA, CGNAT and
// cloud-metadata ranges regardless of name. RFC1918 and multicast are
// here because a grant to the relay is not a grant to the LAN:
// a home relay that forwarded to 192.168.1.1
// would hand the sandbox the operator's router.
//
// fd00::/8 covers the overlay's own fd06:6e7::/48, deliberately: the
// relay forwards to the internet, and the overlay is reached by declared
// services and reach grants, never by asking the relay nicely.
var refused = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"), // CGNAT (and Tailscale v4)
	netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("169.254.0.0/16"), // link-local, incl. 169.254.169.254
	netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.168.0.0/16"),
	netip.MustParsePrefix("224.0.0.0/4"), // multicast
	netip.MustParsePrefix("240.0.0.0/4"), // reserved
	netip.MustParsePrefix("::/128"),
	// ::/96 is the deprecated IPv4-compatible form (obsolete RFC 4291): ::a9fe:a9fe
	// embeds 169.254.169.254 the same way NAT64 does. Not routed by modern stacks,
	// but an SSRF refusal list should be complete, and ::/96 blocks nothing live —
	// the only real addresses inside it are :: and ::1, already refused above.
	netip.MustParsePrefix("::/96"),
	netip.MustParsePrefix("::1/128"),
	netip.MustParsePrefix("fc00::/7"), // ULA, incl. fd00::/8 and the overlay
	// NAT64 embeds an arbitrary IPv4 address in the low 32 bits, so a v6 literal
	// in one of these prefixes is a v4 destination in disguise: 64:ff9b::a9fe:a9fe
	// embeds 169.254.169.254 (cloud metadata) and 64:ff9b::0a00:0001 embeds 10.0.0.1
	// (RFC1918). .Unmap() only rewrites IPv4-MAPPED (::ffff:0:0/96), not these, so
	// without an explicit refusal a NAT64 path (AWS IPv6-only+DNS64/NAT64 subnets,
	// arbitrary residential networks) would let the kernel translate the disguise
	// back to the embedded v4 and hand the sandbox instance metadata. Blocking the
	// whole well-known prefix also blocks NAT64-to-public: correct for an SSRF relay,
	// since an IPv6-only+NAT64 egress node is an operational special case handled
	// explicitly, not by leaving the metadata door open. A custom-NSP NAT64 prefix
	// is out of scope: reaching it requires an actively hostile operator network.
	netip.MustParsePrefix("64:ff9b::/96"),   // NAT64 well-known (RFC 6052)
	netip.MustParsePrefix("64:ff9b:1::/48"), // NAT64 local-use (RFC 8215)
	netip.MustParsePrefix("2002::/16"),      // 6to4 embeds an IPv4 destination
	netip.MustParsePrefix("2001::/32"),      // Teredo tunnels through IPv4
	netip.MustParsePrefix("fe80::/10"),      // link-local
	netip.MustParsePrefix("ff00::/8"),       // multicast
}

// CheckResolved refuses the whole set if ANY resolved address is out of bounds.
// Not "use the good ones": a name that resolves to both a public address and
// 127.0.0.1 is a name doing something it should have to explain, and picking
// the survivor is how a DNS-rebinding target gets forwarded on the retry.
func CheckResolved(addrs []netip.Addr) error {
	if len(addrs) == 0 {
		return fmt.Errorf("egress: destination resolved to no addresses")
	}
	for _, addr := range addrs {
		// Unmap folds IPv4-MAPPED back to v4; WithZone("") drops any scope id,
		// since netip.Prefix.Contains returns false for a zoned addr — a resolver
		// that ever handed us fe80::1%eth0 would otherwise bypass every v6 prefix.
		a := addr.Unmap().WithZone("")
		if !a.IsValid() {
			return fmt.Errorf("egress: invalid resolved address")
		}
		for _, p := range refused {
			if p.Contains(a) {
				return fmt.Errorf("egress: destination %s is in refused range %s", a, p)
			}
		}
	}
	return nil
}

// CheckTarget refuses an IP-literal CONNECT target unless the org allowlisted
// that exact literal. A hostname passes here and is checked again,
// after resolution, by CheckResolved.
func CheckTarget(host string, literalAllow []string) error {
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return nil // not a literal; it is a hostname
	}
	for _, allowed := range literalAllow {
		if a, err := netip.ParseAddr(allowed); err == nil && a == addr {
			return CheckResolved([]netip.Addr{addr})
		}
	}
	return fmt.Errorf("egress: IP-literal target %s is not allowlisted as a literal", host)
}
