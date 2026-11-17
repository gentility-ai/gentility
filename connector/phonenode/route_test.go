package phonenode

import (
	"errors"
	"net"
	"net/netip"
	"strings"
	"testing"
)

func TestAddressKindsCountsWhatTheInterfaceCarries(t *testing.T) {
	addresses := []netip.Addr{
		netip.MustParseAddr("192.0.2.10"),
		netip.MustParseAddr("fe80::1"),
		netip.MustParseAddr("fd00:1234::1"),
		netip.MustParseAddr("2001:db8:1::42"),
		netip.MustParseAddr("2001:db8:1::43"),
		netip.MustParseAddr("fd06:6e7:0:2::a"),
	}
	v4, global6, ula6 := addressKinds(addresses)
	if v4 != 1 || global6 != 2 || ula6 != 1 {
		t.Fatalf("got v4=%d global6=%d ula6=%d; link-local and 6net's own address count as nothing", v4, global6, ula6)
	}
}

func TestDescribeInterfaceSaysCountsNeverAddresses(t *testing.T) {
	byName := func(string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("192.0.2.10"), netip.MustParseAddr("fd8c::1")}, nil
	}
	got := describeInterface("en0", byName)
	if got != "1 IPv4, 0 global IPv6, 1 ULA" {
		t.Fatalf("got %q", got)
	}
	if strings.Contains(got, "192.168") || strings.Contains(got, "fd8c") {
		t.Fatalf("an address leaked: %q", got)
	}
}

// A non-contiguous mask renders as ip/hexmask, which netip.ParsePrefix would
// refuse with the address in its error; the listing still counts, and says
// no address.
func TestAnOddListingStillCountsAndSaysNoAddress(t *testing.T) {
	listing := []net.Addr{
		&net.IPNet{IP: net.ParseIP("192.0.2.10").To4(), Mask: net.IPv4Mask(255, 0, 255, 0)},
		&net.IPNet{IP: net.ParseIP("fd8c::1"), Mask: net.CIDRMask(64, 128)},
		&net.IPAddr{IP: net.ParseIP("2001:db8::42")},
	}
	byName := func(string) ([]netip.Addr, error) { return addressesOf(listing), nil }
	got := describeInterface("en0", byName)
	if got != "1 IPv4, 0 global IPv6, 1 ULA" {
		t.Fatalf("got %q", got)
	}
	for _, leak := range []string{"192.168", "fd8c", "2001", "ff00"} {
		if strings.Contains(got, leak) {
			t.Fatalf("an address leaked: %q", got)
		}
	}
}

func TestDescribeInterfaceNamesAnUnreadableOne(t *testing.T) {
	byName := func(string) ([]netip.Addr, error) { return nil, errors.New("no such interface") }
	if got := describeInterface("en9", byName); got != "cannot be read: no such interface" {
		t.Fatalf("got %q", got)
	}
}

func TestDefaultRouteRefusesNoName(t *testing.T) {
	if got := DefaultRoute("", nil); !strings.Contains(got, `"malformed"`) {
		t.Fatalf("got %s", got)
	}
}

// Lines reach a Logger on phonenode's own goroutine (logQueue), so the test
// waits for the line the way the package's other tests do. The hint it sets
// is process-wide, and the fork ignores "", so it cannot be reset afterwards.
func TestDefaultRouteAnswersOkAndLogsTheInterface(t *testing.T) {
	log := &lineLogger{}
	if got := DefaultRoute("lo0", log); !strings.Contains(got, `"ok":true`) {
		t.Fatalf("got %s", got)
	}
	eventually(t, "the default-route line", func() bool {
		log.mu.Lock()
		defer log.mu.Unlock()
		for _, line := range log.lines {
			if strings.Contains(line, "phonenode: default route on lo0:") {
				return true
			}
		}
		return false
	})
}
