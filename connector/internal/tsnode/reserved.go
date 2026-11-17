package tsnode

import (
	"fmt"
	"net/netip"
	"slices"

	"tailscale.com/net/tsaddr"
)

// ReservedSubnet is one /64 inside 6net's range that the pinned
// library has already spoken for. Gentility allocates one /64 per organization
// out of the same /48, so these are the indices the allocator must skip.
type ReservedSubnet struct {
	// Index is bits 48..63 of the prefix — the org index Gentility allocates.
	Index int
	// Prefix is the reserved range widened to the /64 it sits in, because a /64
	// is the unit an org is given.
	Prefix netip.Prefix
	// Source names the tsaddr function or constant the range was read from, so
	// a new entry says who reserved it.
	Source string
}

// ULARange is 6net's range as the PINNED library (genscale) defines it.
func ULARange() netip.Prefix {
	return tsaddr.TailscaleULARange()
}

// ReservedSubnets reads, off the pinned tailscale.com, every /64 inside the ULA
// range the library reserves for itself. Nothing here is hand-copied: the
// prefixes come from the library, and cmd/clientcontract publishes them as the
// contract's client/tsaddr_reserved.json for the server's allocator parity test.
// A library upgrade that reserves a new subnet therefore cannot pass unnoticed.
//
// The list of sources is enumerated because Go cannot enumerate a package's
// functions at run time. Adding one here is the only correct response to an
// upgrade that reserves more.
func ReservedSubnets() ([]ReservedSubnet, error) {
	return reservedSubnets(tsaddr.TailscaleULARange(), librarySources())
}

// reservedSource is one range the pinned library carves out, paired with the
// name of the thing it was read from.
type reservedSource struct {
	prefix netip.Prefix
	name   string
}

// librarySources is the whole of what the pinned tailscale.com reserves inside
// the ULA. It is the one place an upgrade adds a line.
func librarySources() []reservedSource {
	return []reservedSource{
		{
			netip.MustParsePrefix(tsaddr.TailscaleServiceIPv6String + "/128"),
			"TailscaleServiceIPv6String " + tsaddr.TailscaleServiceIPv6String,
		},
		{tsaddr.TailscaleViaRange(), "TailscaleViaRange"},
		{tsaddr.Tailscale4To6Range(), "Tailscale4To6Range"},
		{tsaddr.TailscaleEphemeral6Range(), "TailscaleEphemeral6Range"},
	}
}

// reservedSubnets is the whole computation with the library read lifted out, so
// a test can hand it a source the pinned library does not have today.
func reservedSubnets(ula netip.Prefix, sources []reservedSource) ([]ReservedSubnet, error) {
	seen := make(map[int]bool, len(sources))
	out := make([]ReservedSubnet, 0, len(sources))

	for _, s := range sources {
		// A source outside the ULA is not something to skip quietly. Every
		// range named here is one the library carves out of the range the
		// allocator hands out of, so one landing elsewhere means the library
		// has moved it — and the fixture, and the allocator's skip list with
		// it, would be silently wrong.
		if !ula.Contains(s.prefix.Addr()) {
			return nil, fmt.Errorf(
				"tsnode: %s is %s, outside %s — the pinned tailscale.com has moved a "+
					"reserved range out of the ULA, so this skip list can no longer be "+
					"trusted; do not regenerate the fixture from it",
				s.name, s.prefix, ula,
			)
		}
		subnet := to64(s.prefix)
		index := orgIndexOf(subnet)
		if seen[index] {
			continue
		}
		seen[index] = true
		out = append(out, ReservedSubnet{Index: index, Prefix: subnet, Source: s.name})
	}

	slices.SortFunc(out, func(a, b ReservedSubnet) int { return a.Index - b.Index })

	return out, nil
}

// orgIndexOf reads bits 48..63 — the org index — out of a prefix in the ULA.
func orgIndexOf(p netip.Prefix) int {
	b := p.Addr().As16()
	return int(b[6])<<8 | int(b[7])
}

// to64 widens a reserved range to the /64 containing it.
func to64(p netip.Prefix) netip.Prefix {
	masked, err := p.Addr().Prefix(OrgPrefixBits)
	if err != nil {
		// Unreachable: OrgPrefixBits is 64 and every address here is a v6 address.
		panic(err)
	}
	return masked
}
