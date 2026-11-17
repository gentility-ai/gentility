package egress

import (
	"net/netip"
	"testing"
)

func TestRefuseIPv6TransitionRanges(t *testing.T) {
	public := netip.MustParseAddr("2606:4700:4700::1111")
	for _, raw := range []string{"2002::", "2002:0a00:0001::", "2002:ffff:ffff:ffff:ffff:ffff:ffff:ffff", "2001::", "2001:0:ffff:ffff:ffff:ffff:ffff:ffff"} {
		addr := netip.MustParseAddr(raw)
		if err := CheckResolved([]netip.Addr{public, addr}); err == nil {
			t.Errorf("accepted %s", raw)
		}
		if err := CheckTarget(raw, []string{raw}); err == nil {
			t.Errorf("allowlist bypass for %s", raw)
		}
	}
	for _, raw := range []string{"2001:1::1", "2003::1", public.String()} {
		if err := CheckResolved([]netip.Addr{netip.MustParseAddr(raw)}); err != nil {
			t.Errorf("refused control %s: %v", raw, err)
		}
	}
}
