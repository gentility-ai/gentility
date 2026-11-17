package egress

import (
	"net/netip"
	"testing"
)

func TestCheckResolved(t *testing.T) {
	cases := []struct {
		name    string
		addrs   []string
		wantErr bool
	}{
		{"public v4", []string{"93.184.216.34"}, false},
		{"public v6", []string{"2606:2800:220:1:248:1893:25c8:1946"}, false},
		{"public v6 cloudflare", []string{"2606:4700::1111"}, false},
		{"loopback v4", []string{"127.0.0.1"}, true},
		{"loopback v6", []string{"::1"}, true},
		{"rfc1918 10", []string{"10.0.0.5"}, true},
		{"rfc1918 172.16", []string{"172.16.4.4"}, true},
		{"rfc1918 192.168", []string{"192.168.1.1"}, true},
		{"link-local v4", []string{"169.254.1.1"}, true},
		{"aws metadata", []string{"169.254.169.254"}, true},
		{"gcp metadata v6", []string{"fd00:ec2::254"}, true},
		{"link-local v6", []string{"fe80::1"}, true},
		{"ula", []string{"fd00::1"}, true},
		{"6net's range is still ula", []string{"fd06:6e7:0:1::a"}, true},
		{"cgnat", []string{"100.64.0.1"}, true},
		{"multicast", []string{"224.0.0.1"}, true},
		{"unspecified", []string{"0.0.0.0"}, true},
		{"nat64 metadata", []string{"64:ff9b::a9fe:a9fe"}, true},    // embeds 169.254.169.254
		{"nat64 rfc1918", []string{"64:ff9b::0a00:0001"}, true},     // embeds 10.0.0.1
		{"nat64 local-use", []string{"64:ff9b:1::0a00:0001"}, true}, // RFC 8215, embeds 10.0.0.1
		{"v4-compatible metadata", []string{"::a9fe:a9fe"}, true},   // deprecated ::/96, embeds 169.254.169.254
		{"zoned link-local bypass", []string{"fe80::1%eth0"}, true}, // zone must not skip the v6 prefixes
		{"one bad among good refuses all", []string{"93.184.216.34", "127.0.0.1"}, true},
		{"empty resolution refuses", []string{}, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			addrs := make([]netip.Addr, 0, len(tc.addrs))
			for _, s := range tc.addrs {
				a, err := netip.ParseAddr(s)
				if err != nil {
					t.Fatalf("bad fixture %q: %v", s, err)
				}
				addrs = append(addrs, a)
			}
			err := CheckResolved(addrs)
			if (err != nil) != tc.wantErr {
				t.Fatalf("CheckResolved(%v) error = %v, wantErr %v", tc.addrs, err, tc.wantErr)
			}
		})
	}
}

// A zero/invalid netip.Addr must not fail open: BitLen 0 short-circuits
// Contains to false, so an unfiltered garbage addr would report refused=false.
// It cannot arrive through the string table above (netip.ParseAddr never yields
// an invalid addr), so it gets its own case.
func TestCheckResolvedRefusesInvalidAddr(t *testing.T) {
	if err := CheckResolved([]netip.Addr{{}}); err == nil {
		t.Fatal("a zero/invalid resolved address must be refused, not fail open")
	}
}

func TestCheckTarget(t *testing.T) {
	if err := CheckTarget("example.com", nil); err != nil {
		t.Fatalf("hostname target should be allowed: %v", err)
	}
	if err := CheckTarget("93.184.216.34", nil); err == nil {
		t.Fatal("an IP literal with no literal allowlist must be refused")
	}
	if err := CheckTarget("93.184.216.34", []string{"93.184.216.34"}); err != nil {
		t.Fatalf("an allowlisted IP literal must be allowed: %v", err)
	}
	if err := CheckTarget("127.0.0.1", []string{"127.0.0.1"}); err == nil {
		t.Fatal("an allowlisted literal in a refused range must still be refused")
	}
}

// The obfuscated-literal layering: netip.ParseAddr is
// strict and refuses the odd `inet` spellings that resolve to loopback, so
// CheckTarget lets them through as if they were hostnames. That is the correct,
// fail-safe layering: they are not admitted here, they are handed to the
// resolver, and CheckResolved catches the address they expand to after
// resolution. The classification each side must give for these forms is pinned
// as shared vectors in contract/connector/allowlist_cases.json,
// while this test asserts CheckTarget's half of the layering.
func TestCheckTargetTreatsObfuscatedLiteralsAsHostnames(t *testing.T) {
	obfuscated := []string{
		"0x7f.0.0.1", // hex, would be 127.0.0.1
		"127.1",      // short form, 127.0.0.1
		"010.0.0.1",  // octal, 8.0.0.1
		"2130706433", // decimal, 127.0.0.1
	}
	for _, host := range obfuscated {
		if _, err := netip.ParseAddr(host); err == nil {
			t.Fatalf("fixture %q parsed as an IP literal; the layering assumes netip refuses it", host)
		}
		if err := CheckTarget(host, nil); err != nil {
			t.Fatalf("CheckTarget(%q) refused a non-literal — it must pass to be resolved: %v", host, err)
		}
	}
}
