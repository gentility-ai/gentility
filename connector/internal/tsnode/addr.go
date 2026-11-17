// Package tsnode is the ONLY package in the connector that imports tsnet.
// Everything the rest of the binary needs from the overlay —
// listening, dialling, peer attribution — comes through here, so a library
// bump has exactly one blast radius.
//
// It exists because tsnet is blind to a node holding more than one address,
// in two ways:
//
//   - tsnet.Server.TailscaleIPs() returns ONE v6 address (the last in the
//     netmap), and a wildcard listener serves only that one. A peer dialling
//     any other granted address is refused in 1 ms.
//   - tsnet.Server.Dial has no source-address parameter and lets the netstack
//     pick, which is always that same primary. A dial into any other org is
//     then dropped by that org's packet filter — an 8-second timeout, no error.
//
// ListenAll answers the first; DialBound answers the second.
package tsnode

import (
	"errors"
	"fmt"
	"net/netip"

	"tailscale.com/net/tsaddr"
)

// OrgPrefixBits is exported because it is the size of the slice an org is
// given, and cmd/clientcontract publishes it in the file the server's allocator
// checks itself against.
const OrgPrefixBits = 64

// ErrNoGrant is what SourceFor says when this node holds no address in the
// destination's organization. The dial cannot be made from anywhere, and a
// caller telling a person so (phonenode.OpenDoor's no_grant) needs to tell
// it apart from a dial that was made and failed.
var ErrNoGrant = errors.New("it holds no address in that organization")

// ConfigGrant is one grant exactly as the config document carries it:
// strings, straight off the wire, nothing checked yet.
type ConfigGrant struct {
	OrgID   string `json:"org_id,omitempty"`
	Org     string `json:"org"`
	Address string `json:"address"`
	Label   string `json:"label"`
}

// Grant is one organization's address on this node — the node's half of a
// `node_grants` row. A node holds one per org it belongs to.
//
// Grants are made by ParseGrants, so a Grant in hand is already a well-formed
// overlay address in a /64 no other grant claims.
type Grant struct {
	OrgID   string
	Org     string
	Address netip.Addr
	Label   string
}

// ParseGrants turns a config document's grants into Grants, or refuses the
// document.
//
// Validating at ingest is what lets SourceFor stay a pure lookup: everything
// that can be wrong with a grant becomes an error the node reports when it
// takes the config in, at the one moment it can still say which document was
// bad.
func ParseGrants(in []ConfigGrant) ([]Grant, error) {
	grants := make([]Grant, 0, len(in))
	claimed := make(map[netip.Prefix]string, len(in))

	for _, cg := range in {
		if cg.Org == "" {
			return nil, fmt.Errorf("tsnode: grant for address %q names no org", cg.Address)
		}

		address, err := netip.ParseAddr(cg.Address)
		if err != nil {
			return nil, fmt.Errorf("tsnode: grant for org %q: address %q: %w", cg.Org, cg.Address, err)
		}

		prefix, err := OrgPrefix(address)
		if err != nil {
			return nil, fmt.Errorf("tsnode: grant for org %q: %w", cg.Org, err)
		}

		if other, taken := claimed[prefix]; taken {
			return nil, fmt.Errorf(
				"tsnode: orgs %q and %q both claim %s; the /64 is what a source is chosen by, "+
					"so one of the two could never be dialled",
				other, cg.Org, prefix,
			)
		}
		claimed[prefix] = cg.Org

		grants = append(grants, Grant{Org: cg.Org, OrgID: cg.OrgID, Address: address, Label: cg.Label})
	}

	return grants, nil
}

// ula is 6net's range, fd06:6e7::/48. The library hard-codes it in
// net/tsaddr, and it is ours because we own that library: our fork, genscale,
// moved it out of Tailscale's ULA so 6net never shares a route
// with an installed Tailscale (genscale's GENSCALE.md). It is read from the
// library, not restated, so the range lives in one place.
var ula = tsaddr.TailscaleULARange()

// OverlayRange is 6net's range, for a package that must name it but may not
// import the library (boundary_test).
func OverlayRange() netip.Prefix { return ula }

// OrgPrefix returns the /64 an overlay address belongs to — the org's prefix.
//
// It refuses anything outside 6net's range, including every IPv4 address:
// the overlay is v6-only, and a v4 address here
// means a caller handed us a CGNAT address the head also assigns.
func OrgPrefix(a netip.Addr) (netip.Prefix, error) {
	if !a.Is6() || a.Is4In6() {
		return netip.Prefix{}, fmt.Errorf("tsnode: %s is not an IPv6 overlay address", a)
	}
	if a.Zone() != "" {
		return netip.Prefix{}, fmt.Errorf("tsnode: %s is scoped to interface zone %q; an overlay address never is", a, a.Zone())
	}
	if !ula.Contains(a) {
		return netip.Prefix{}, fmt.Errorf("tsnode: %s is outside %s", a, ula)
	}

	prefix, err := a.Prefix(OrgPrefixBits)
	if err != nil {
		return netip.Prefix{}, fmt.Errorf("tsnode: masking %s to /%d: %w", a, OrgPrefixBits, err)
	}

	return prefix, nil
}

// SourceFor returns the grant this node must dial dst from: the one whose
// address shares dst's /64.
//
// The /64 is the authority: Grant.Org is a label carried for diagnostics and
// is never matched on.
//
// There is no fallback to a primary address on purpose. A dial from the wrong
// org's source is not slower or less tidy — it is silently dropped by the
// destination's filter, and a caller that gets an error here can say so.
func SourceFor(grants []Grant, dst netip.Addr) (Grant, error) {
	want, err := OrgPrefix(dst)
	if err != nil {
		return Grant{}, err
	}

	for _, grant := range grants {
		// A grant that does not parse is skipped rather than returned:
		// ParseGrants refuses malformed grants at ingest, and failing on one
		// here would make the answer depend on where in the slice it sat.
		got, err := OrgPrefix(grant.Address)
		if err != nil {
			continue
		}
		if got == want {
			return grant, nil
		}
	}

	return Grant{}, fmt.Errorf("tsnode: no grant in %s; this node cannot reach %s: %w", want, dst, ErrNoGrant)
}

// Reconcile diffs the addresses currently bound against the addresses the
// netmap now says this node holds, returning what to bind and what to unbind.
//
// Revoking a grant takes an address away in under a second (spike: 907 ms), and
// noticing is the node's job, not the netmap's — this is what the caller drives
// off an address-set change.
func Reconcile(bound, want []netip.Addr) (bind, unbind []netip.Addr) {
	wanted := make(map[netip.Addr]bool, len(want))
	for _, a := range want {
		wanted[a] = true
	}

	// Both sides may repeat an address — the netmap is not a set — so each is
	// answered for once, in the order it first appears.
	held := make(map[netip.Addr]bool, len(bound))
	for _, a := range bound {
		if held[a] {
			continue
		}
		held[a] = true
		if !wanted[a] {
			unbind = append(unbind, a)
		}
	}

	added := make(map[netip.Addr]bool, len(want))
	for _, a := range want {
		if held[a] || added[a] {
			continue
		}
		added[a] = true
		bind = append(bind, a)
	}

	return bind, unbind
}
