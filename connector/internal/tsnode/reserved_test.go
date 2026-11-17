package tsnode

import (
	"net/netip"
	"strings"
	"testing"
)

// TestReservedSubnets pins what the PINNED library reserves. These four tuples
// are shared with the server allocator's parity test; a library bump that
// changes any of them must
// fail here first, in the package that reads them.
func TestReservedSubnets(t *testing.T) {
	want := []ReservedSubnet{
		{0, netip.MustParsePrefix("fd06:6e7::/64"), "TailscaleServiceIPv6String fd06:6e7::53"},
		{2842, netip.MustParsePrefix("fd06:6e7:0:b1a::/64"), "TailscaleViaRange"},
		{43794, netip.MustParsePrefix("fd06:6e7:0:ab12::/64"), "Tailscale4To6Range"},
		{61411, netip.MustParsePrefix("fd06:6e7:0:efe3::/64"), "TailscaleEphemeral6Range"},
	}

	got, err := ReservedSubnets()
	if err != nil {
		t.Fatalf("ReservedSubnets: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("ReservedSubnets returned %d subnets, want %d: %+v", len(got), len(want), got)
	}
	for i, w := range want {
		if got[i] != w {
			t.Fatalf("ReservedSubnets[%d] = %+v, want %+v", i, got[i], w)
		}
	}
}

// The allocator walks the list in order and treats one index as one skip, so
// the order and the uniqueness are part of the contract, not incidental.
func TestReservedSubnetsAreSortedAndDeduped(t *testing.T) {
	got, err := ReservedSubnets()
	if err != nil {
		t.Fatalf("ReservedSubnets: %v", err)
	}
	for i := 1; i < len(got); i++ {
		if got[i].Index <= got[i-1].Index {
			t.Fatalf("index %d follows %d: the list is not sorted and deduped: %+v", got[i].Index, got[i-1].Index, got)
		}
	}
}

func TestReservedSubnetsDedupesTwoRangesInOneOrgPrefix(t *testing.T) {
	ula := netip.MustParsePrefix("fd06:6e7::/48")
	got, err := reservedSubnets(ula, []reservedSource{
		{netip.MustParsePrefix("fd06:6e7:0:b1a::/64"), "first"},
		{netip.MustParsePrefix("fd06:6e7:0:b1a:beef::/80"), "second"},
	})
	if err != nil {
		t.Fatalf("reservedSubnets: %v", err)
	}
	if len(got) != 1 || got[0].Source != "first" {
		t.Fatalf("reservedSubnets = %+v, want one entry sourced from the first range", got)
	}
}

// A source outside the ULA cannot be silently dropped. It means the pinned
// library moved a range out from under us, and the fixture the allocator reads
// would then be quietly wrong — so the read fails and the build stops.
func TestReservedSubnetsRefusesASourceOutsideTheULA(t *testing.T) {
	ula := netip.MustParsePrefix("fd06:6e7::/48")
	_, err := reservedSubnets(ula, []reservedSource{
		{netip.MustParsePrefix("fd06:6e8::/64"), "MovedRange"},
	})
	if err == nil {
		t.Fatal("reservedSubnets accepted a range outside the ULA instead of refusing it")
	}
	if !strings.Contains(err.Error(), "MovedRange") {
		t.Fatalf("error %q does not name the source that moved", err)
	}
}
