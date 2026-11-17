package tsnode

import (
	"errors"
	"net/netip"
	"strings"
	"testing"
)

func addr(t *testing.T, s string) netip.Addr {
	t.Helper()
	a, err := netip.ParseAddr(s)
	if err != nil {
		t.Fatalf("ParseAddr(%q): %v", s, err)
	}
	return a
}

func TestOrgPrefixMasksToTheOrg64(t *testing.T) {
	got, err := OrgPrefix(addr(t, "fd06:6e7:0:2::a"))
	if err != nil {
		t.Fatalf("OrgPrefix: %v", err)
	}
	if got.String() != "fd06:6e7:0:2::/64" {
		t.Fatalf("OrgPrefix = %q, want fd06:6e7:0:2::/64", got.String())
	}
}

func TestOrgPrefixRefusesAddressesOutsideTheOverlayRange(t *testing.T) {
	if _, err := OrgPrefix(addr(t, "2001:db8::1")); err == nil {
		t.Fatal("OrgPrefix accepted an address outside fd06:6e7::/48")
	}
	if _, err := OrgPrefix(addr(t, "100.64.0.1")); err == nil {
		t.Fatal("OrgPrefix accepted an IPv4 address; the overlay is v6-only")
	}
}

func TestSourceForPicksTheGrantInTheDestinationsOrg(t *testing.T) {
	grants := []Grant{
		{Org: "acme", Address: addr(t, "fd06:6e7:0:1::1")},
		{Org: "beta", Address: addr(t, "fd06:6e7:0:2::1")},
	}

	src, err := SourceFor(grants, addr(t, "fd06:6e7:0:1::2"))
	if err != nil {
		t.Fatalf("SourceFor: %v", err)
	}
	if src.Address.String() != "fd06:6e7:0:1::1" || src.Org != "acme" {
		t.Fatalf("SourceFor = %+v, want the acme grant", src)
	}
}

func TestSourceForRefusesADestinationInNoGrantedOrg(t *testing.T) {
	grants := []Grant{{Org: "acme", Address: addr(t, "fd06:6e7:0:1::1")}}

	// This is the failure the spike measured as an 8-second timeout: without
	// this refusal the node dials from its primary address and the peer's
	// packet filter silently drops it.
	_, err := SourceFor(grants, addr(t, "fd06:6e7:0:2::2"))
	if err == nil {
		t.Fatal("SourceFor invented a source for an org this node holds no grant in")
	}
	if !errors.Is(err, ErrNoGrant) {
		t.Fatalf("err = %v, want ErrNoGrant so a caller can say which failure it was", err)
	}
}

func TestSourceForRefusesNoGrants(t *testing.T) {
	if _, err := SourceFor(nil, addr(t, "fd06:6e7:0:1::2")); err == nil {
		t.Fatal("SourceFor answered with no grants at all")
	}
}

func TestReconcileDiffsBoundAgainstWanted(t *testing.T) {
	bound := []netip.Addr{addr(t, "fd06:6e7:0:1::1"), addr(t, "fd06:6e7:0:2::1")}
	want := []netip.Addr{addr(t, "fd06:6e7:0:1::1"), addr(t, "fd06:6e7:0:3::1")}

	bind, unbind := Reconcile(bound, want)

	if len(bind) != 1 || bind[0].String() != "fd06:6e7:0:3::1" {
		t.Fatalf("bind = %v, want [fd06:6e7:0:3::1]", bind)
	}
	if len(unbind) != 1 || unbind[0].String() != "fd06:6e7:0:2::1" {
		t.Fatalf("unbind = %v, want [fd06:6e7:0:2::1]", unbind)
	}
}

func TestReconcileIsStableWhenNothingChanged(t *testing.T) {
	same := []netip.Addr{addr(t, "fd06:6e7:0:1::1")}

	bind, unbind := Reconcile(same, same)

	if len(bind) != 0 || len(unbind) != 0 {
		t.Fatalf("Reconcile(same, same) = %v, %v; want no churn", bind, unbind)
	}
}

func TestOrgPrefix(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string // "" means the address must be refused
	}{
		{"masks to the org /64", "fd06:6e7:0:2::a", "fd06:6e7:0:2::/64"},
		{"4-in-6 is still IPv4", "::ffff:100.64.0.1", ""},
		{"one /48 below the ULA", "fd06:6e6:ffff:ffff::1", ""},
		{"one /48 above the ULA", "fd06:6e8::1", ""},
		{"Tailscale's range", "fd7a:115c:a1e0:2::1", ""},
		{"zoned address", "fd06:6e7:0:2::a%eth0", ""},
		{"last address in the ULA", "fd06:6e7:0:ffff:ffff:ffff:ffff:ffff", "fd06:6e7:0:ffff::/64"},
		// A subnet tsaddr reserves is a perfectly well-formed /64 here.
		// Refusing to allocate one is the server allocator's job, checked
		// by the reserved-address parity test; this function only masks.
		{"reserved via subnet", "fd06:6e7:0:b1a::1", "fd06:6e7:0:b1a::/64"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := OrgPrefix(addr(t, c.in))
			if c.want == "" {
				if err == nil {
					t.Fatalf("OrgPrefix(%s) = %q, want a refusal", c.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("OrgPrefix(%s): %v", c.in, err)
			}
			if got.String() != c.want {
				t.Fatalf("OrgPrefix(%s) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

func TestOrgPrefixNamesTheZoneItRefuses(t *testing.T) {
	_, err := OrgPrefix(addr(t, "fd06:6e7:0:2::a%eth0"))
	if err == nil {
		t.Fatal("OrgPrefix accepted a zoned address")
	}
	// The address prints WITH its zone in any refusal, so naming "eth0" alone
	// would also pass on the ULA-containment message. The point is that the
	// zone is the stated reason.
	if !strings.Contains(err.Error(), "zone") || !strings.Contains(err.Error(), "eth0") {
		t.Fatalf("error %q does not refuse the address for its zone", err)
	}
}

func TestSourceForRefusesAReservedSubnetThisNodeHoldsNoGrantIn(t *testing.T) {
	grants := []Grant{{Org: "acme", Address: addr(t, "fd06:6e7:0:1::1")}}

	if _, err := SourceFor(grants, addr(t, "fd06:6e7:0:b1a::1")); err == nil {
		t.Fatal("SourceFor answered for the via range, which no org is granted")
	}
}

func TestSourceForDoesNotDependOnGrantOrder(t *testing.T) {
	good := Grant{Org: "acme", Address: addr(t, "fd06:6e7:0:1::1")}
	// Not an overlay address at all: OrgPrefix refuses it. A grant like this
	// must not decide the answer for a destination it has nothing to do with.
	bad := Grant{Org: "stale", Address: addr(t, "2001:db8::1")}
	dst := addr(t, "fd06:6e7:0:1::2")

	for _, grants := range [][]Grant{{bad, good}, {good, bad}} {
		src, err := SourceFor(grants, dst)
		if err != nil {
			t.Fatalf("SourceFor(%v): %v", grants, err)
		}
		if src.Org != "acme" {
			t.Fatalf("SourceFor(%v) = %+v, want the acme grant", grants, src)
		}
	}
}

func TestParseGrantsParsesTheConfigDocumentShape(t *testing.T) {
	got, err := ParseGrants([]ConfigGrant{
		{Org: "acme", Address: "fd06:6e7:0:1::a", Label: "n1"},
		{Org: "beta", Address: "fd06:6e7:0:2::a", Label: "n2"},
	})
	if err != nil {
		t.Fatalf("ParseGrants: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("ParseGrants returned %d grants, want 2", len(got))
	}
	if got[0].Org != "acme" || got[0].Address.String() != "fd06:6e7:0:1::a" || got[0].Label != "n1" {
		t.Fatalf("ParseGrants[0] = %+v, want the acme grant", got[0])
	}
}

func TestParseGrantsRefusesAnUnparseableAddress(t *testing.T) {
	if _, err := ParseGrants([]ConfigGrant{{Org: "acme", Address: "not-an-address"}}); err == nil {
		t.Fatal("ParseGrants accepted an address that does not parse")
	}
}

func TestParseGrantsRefusesAnAddressOutsideTheOverlay(t *testing.T) {
	if _, err := ParseGrants([]ConfigGrant{{Org: "acme", Address: "2001:db8::1"}}); err == nil {
		t.Fatal("ParseGrants accepted an address outside 6net's range")
	}
}

func TestParseGrantsRefusesTwoGrantsInOneOrgPrefix(t *testing.T) {
	// Two orgs sharing a /64 is a malformed document: the /64 is what
	// SourceFor matches on, so one of them could never be reached.
	_, err := ParseGrants([]ConfigGrant{
		{Org: "acme", Address: "fd06:6e7:0:1::a"},
		{Org: "beta", Address: "fd06:6e7:0:1::b"},
	})
	if err == nil {
		t.Fatal("ParseGrants accepted two grants in one /64")
	}
	if !strings.Contains(err.Error(), "acme") || !strings.Contains(err.Error(), "beta") {
		t.Fatalf("error %q does not name both orgs sharing the prefix", err)
	}
}

func TestParseGrantsRefusesAnEmptyOrg(t *testing.T) {
	if _, err := ParseGrants([]ConfigGrant{{Org: "", Address: "fd06:6e7:0:1::a"}}); err == nil {
		t.Fatal("ParseGrants accepted a grant with no org")
	}
}

func TestReconcileIsDuplicateSafe(t *testing.T) {
	a := addr(t, "fd06:6e7:0:1::1")

	bind, unbind := Reconcile(nil, []netip.Addr{a, a})
	if len(bind) != 1 || bind[0] != a || len(unbind) != 0 {
		t.Fatalf("Reconcile(nil, [x x]) = %v, %v; want bind [x] and no unbind", bind, unbind)
	}

	bind, unbind = Reconcile([]netip.Addr{a, a}, nil)
	if len(unbind) != 1 || unbind[0] != a || len(bind) != 0 {
		t.Fatalf("Reconcile([x x], nil) = %v, %v; want unbind [x] and no bind", bind, unbind)
	}
}
