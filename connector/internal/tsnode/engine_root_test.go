//go:build interface_root

package tsnode

import (
	"net"
	"net/netip"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"

	"tailscale.com/net/netmon"
	"tailscale.com/net/tstun"
	"tailscale.com/tsd"
	"tailscale.com/wgengine/router"
)

// The part of interface mode this package owns that runs without a head: a
// real TUN, the OS router putting an overlay address and the /48 route on it,
// and both gone once it is closed. On Linux, also that the router's policy
// routing is genscale's, not Tailscale's: table 66 behind rules 5110-5170,
// never table 52, and its rules gone once it is closed.
//
// The full join — engine, backend, login, Running, kernel sockets on the
// overlay — needs a control server, and is proved live (the slice's Gates),
// not here.
//
// Run as root: go test -tags interface_root ./internal/tsnode/
func TestTheRouterPutsTheOverlayOnARealInterfaceAndTakesItOff(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root")
	}

	// A host that already has the interface — a running connector — is not
	// one this test may create and destroy it on.
	if runtime.GOOS != "darwin" {
		if _, err := net.InterfaceByName(tunName(runtime.GOOS)); err == nil {
			t.Skipf("%s already exists on this host", tunName(runtime.GOOS))
		}
	}

	sys := tsd.NewSystem()
	defer sys.Bus.Get().Close()
	netMon, err := netmon.New(sys.Bus.Get(), t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	defer netMon.Close()

	dev, devName, err := tstun.New(t.Logf, tunName(runtime.GOOS))
	if err != nil {
		t.Fatalf("tstun.New: %v", err)
	}
	devClosed := false
	defer func() {
		if !devClosed {
			dev.Close()
		}
	}()

	rtr, err := router.New(t.Logf, dev, netMon, sys.HealthTracker.Get(), sys.Bus.Get())
	if err != nil {
		t.Fatalf("router.New: %v", err)
	}
	if err := rtr.Up(); err != nil {
		t.Fatalf("router up: %v", err)
	}

	address := netip.MustParsePrefix("fd06:6e7:0:1::a/128")
	if err := rtr.Set(&router.Config{
		LocalAddrs: []netip.Prefix{address},
		Routes:     []netip.Prefix{netip.MustParsePrefix("fd06:6e7::/48")},
	}); err != nil {
		t.Fatalf("router.Set: %v", err)
	}

	iface, err := net.InterfaceByName(devName)
	if err != nil {
		t.Fatalf("%s: %v", devName, err)
	}
	addrs, err := iface.Addrs()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, a := range addrs {
		if ipnet, ok := a.(*net.IPNet); ok {
			if ip, ok := netip.AddrFromSlice(ipnet.IP); ok && ip.Unmap() == address.Addr() {
				found = true
			}
		}
	}
	if !found {
		t.Fatalf("%s does not hold %s; it holds %v", devName, address.Addr(), addrs)
	}

	if runtime.GOOS == "linux" {
		rules := ip(t, "-6", "rule", "show")
		t.Logf("ip -6 rule show, after Up:\n%s", rules)
		// A kernel without IPv6 policy routing lists no rules at all, not even
		// its own 0 and 32766, and the router then adds none of ours: that is
		// the host's lack, not the router's, so say so.
		if strings.TrimSpace(rules) == "" {
			t.Fatal("this kernel lists no IPv6 policy rules at all (not even 0 and 32766), so it has no IPv6 policy routing for the router to use")
		}
		if !strings.Contains(rules, "5170:\tfrom all lookup 66") {
			t.Fatalf("no rule at 5170 looks up table 66:\n%s", rules)
		}
		if strings.Contains(rules, "lookup 52") {
			t.Fatalf("a rule looks up Tailscale's table 52:\n%s", rules)
		}
		if routes := ip(t, "-6", "route", "show", "table", "66"); !strings.Contains(routes, "fd06:6e7::/48") {
			t.Fatalf("table 66 holds no route for the /48:\n%s", routes)
		}
	}

	if err := rtr.Close(); err != nil {
		t.Fatalf("router close: %v", err)
	}
	if runtime.GOOS == "linux" {
		if rules := ip(t, "-6", "rule", "show"); strings.Contains(rules, "lookup 66") {
			t.Fatalf("a rule still looks up table 66 after close:\n%s", rules)
		}
	}
	if err := dev.Close(); err != nil {
		t.Fatalf("tun close: %v", err)
	}
	devClosed = true

	if _, err := net.InterfaceByName(devName); err == nil {
		t.Fatalf("%s is still there after the TUN was closed", devName)
	}
}

// The cleanup the engine runs at every start, and `cleanup --interface` after
// every stop, leaves an installed Tailscale's firewall alone: a ts-input chain
// standing in for Tailscale's survives it. Linux only, where the chains are.
func TestTheCleanUpLeavesTailscalesChainsAlone(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root")
	}
	if runtime.GOOS != "linux" {
		t.Skip("the ts-* chains are Linux's")
	}
	if err := exec.Command("iptables", "-n", "-L", "ts-input").Run(); err == nil {
		t.Skip("this host already has a ts-input chain, which is not this test's to touch")
	}

	if out, err := exec.Command("iptables", "-N", "ts-input").CombinedOutput(); err != nil {
		t.Fatalf("iptables -N ts-input: %v\n%s", err, out)
	}
	t.Cleanup(func() { exec.Command("iptables", "-X", "ts-input").Run() })

	if err := CleanUp(t.Logf); err != nil {
		t.Fatalf("CleanUp: %v", err)
	}

	if out, err := exec.Command("iptables", "-n", "-L", "ts-input").CombinedOutput(); err != nil {
		t.Fatalf("the cleanup removed Tailscale's ts-input chain: %v\n%s", err, out)
	}
}

// ip runs the ip command and returns what it printed.
func ip(t *testing.T, args ...string) string {
	t.Helper()
	out, err := exec.Command("ip", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("ip %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}
