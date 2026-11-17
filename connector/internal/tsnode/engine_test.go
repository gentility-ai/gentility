package tsnode

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"os"
	"runtime"
	"strings"
	"testing"

	"tailscale.com/client/local"
	"tailscale.com/health"
	"tailscale.com/ipn"
	"tailscale.com/net/netmon"
	"tailscale.com/tailcfg"
	"tailscale.com/types/logger"
	"tailscale.com/types/preftype"
	"tailscale.com/util/eventbus"
	"tailscale.com/wgengine/router"
)

func TestTheInterfaceIsNamedForThePlatform(t *testing.T) {
	if got := tunName("darwin"); got != "utun" {
		t.Fatalf("darwin: %q", got)
	}
	if got := tunName("linux"); got != "gentility0" {
		t.Fatalf("linux: %q", got)
	}
}

func TestInterfacePrefsRouteOnlyTheOverlayAndInstallNoFirewall(t *testing.T) {
	p := interfacePrefs(Options{ControlURL: "https://head.example", Hostname: "willow"})
	if p.ControlURL != "https://head.example" || p.Hostname != "willow" || !p.WantRunning {
		t.Fatalf("identity prefs: %+v", p)
	}
	if p.NetfilterMode != preftype.NetfilterOff {
		t.Fatalf("netfilter %v, want off: the World's filter is the filter", p.NetfilterMode)
	}
	if !p.CorpDNS {
		t.Fatal("CorpDNS off: overlay names would not resolve")
	}
	if p.RouteAll || p.ExitNodeID != "" {
		t.Fatal("accepting routes or an exit node would route more than the /48")
	}
}

func TestInterfaceModeRefusesWithoutRoot(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root")
	}
	_, err := Up(context.Background(), Options{ControlURL: "https://head.example", Dir: t.TempDir(), AuthKey: "k", Interface: true})
	if !errors.Is(err, ErrNeedsRoot) {
		t.Fatalf("err = %v, want ErrNeedsRoot", err)
	}
}

// The refusal has to come before anything is built: a directory Up would have
// made is the trace of a node that got further than it should have.
func TestInterfaceModeRefusesWithoutRootBeforeTouchingAnything(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root")
	}
	dir := t.TempDir() + "/tailscale"

	_, err := Up(context.Background(), Options{ControlURL: "https://head.example", Dir: dir, AuthKey: "k", Interface: true})
	if !errors.Is(err, ErrNeedsRoot) {
		t.Fatalf("err = %v, want ErrNeedsRoot", err)
	}
	if !strings.Contains(err.Error(), "interface mode needs root") {
		t.Fatalf("the refusal does not say what it needs: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("the state directory exists after the refusal: %v", err)
	}
}

// The join guards are Up's, and interface mode holds them too; they come after
// the root check, so only root can reach them, and these are their pure halves.
func TestTheJoinGuardsAreSharedByBothModes(t *testing.T) {
	if err := checkJoin(Options{AuthKey: "k"}); err == nil || !strings.Contains(err.Error(), "control URL") {
		t.Fatalf("no control URL: %v", err)
	}
	if err := checkJoin(Options{ControlURL: "https://head.example", Dir: t.TempDir()}); err == nil ||
		!strings.Contains(err.Error(), "auth key") {
		t.Fatalf("a first join with no key: %v", err)
	}
	if err := checkJoin(Options{ControlURL: "https://head.example", AuthKey: "k"}); err != nil {
		t.Fatalf("a first join with a key: %v", err)
	}
}

// What interface mode waits on. The rejoin rule is watchForLogin's — a
// backend asking for a login is a refusal only when there was no key to
// present — and Running is the end of the wait.
func TestTheWaitForRunningReadsTheBusLikeUserspaceDoes(t *testing.T) {
	message := "boom"
	for _, tc := range []struct {
		name    string
		notify  ipn.Notify
		rejoin  bool
		done    bool
		wantErr error
		errText string
	}{
		{name: "running", notify: inState(ipn.Running), done: true},
		{name: "starting", notify: inState(ipn.Starting)},
		{name: "first join passes through NeedsLogin", notify: inState(ipn.NeedsLogin)},
		{name: "first join sees a URL", notify: browseTo("https://head.example/a/1")},
		{name: "rejoin asked to log in", notify: inState(ipn.NeedsLogin), rejoin: true, done: true, wantErr: ErrRejoinRefused},
		{name: "rejoin sent a URL", notify: browseTo("https://head.example/a/1"), rejoin: true, done: true, wantErr: ErrRejoinRefused},
		{name: "backend error", notify: ipn.Notify{ErrMessage: &message}, done: true, errText: "boom"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			done, err := runningVerdict(tc.notify, tc.rejoin)
			if done != tc.done {
				t.Fatalf("done = %v, want %v", done, tc.done)
			}
			switch {
			case tc.wantErr != nil:
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v, want %v", err, tc.wantErr)
				}
			case tc.errText != "":
				if err == nil || !strings.Contains(err.Error(), tc.errText) {
					t.Fatalf("err = %v, want one saying %q", err, tc.errText)
				}
			case err != nil:
				t.Fatalf("err = %v", err)
			}
		})
	}
}

// An unknown peer has to read the same in both modes: callers tell "not found"
// from "the LocalAPI is down" by errors.Is on local.ErrPeerNotFound.
func TestWhoIsFromTheBackendSpeaksTheLocalClientsErrors(t *testing.T) {
	if _, err := backendWhoIs(tailcfg.NodeView{}, tailcfg.UserProfile{}, false); !errors.Is(err, local.ErrPeerNotFound) {
		t.Fatalf("err = %v, want local.ErrPeerNotFound", err)
	}

	node := (&tailcfg.Node{Name: "willow.", Addresses: []netip.Prefix{netip.MustParsePrefix("fd06:6e7:0:1::2/128")}}).View()
	who, err := backendWhoIs(node, tailcfg.UserProfile{LoginName: "ann@example.com"}, true)
	if err != nil {
		t.Fatal(err)
	}
	if who.Node == nil || who.Node.Name != "willow." || who.UserProfile == nil || who.UserProfile.LoginName != "ann@example.com" {
		t.Fatalf("response %+v", who)
	}
}

// DialBound's grant checks are mode-blind; what it hands the checked pair to
// is the seam, and in interface mode that is a kernel socket.
func TestDialBoundHandsTheCheckedPairToTheDialSeam(t *testing.T) {
	src := addr(t, "fd06:6e7:0:1::1")
	dst := netip.AddrPortFrom(addr(t, "fd06:6e7:0:1::2"), 80)

	var gotSrc netip.Addr
	var gotDst netip.AddrPort
	n := testNode(nil)
	n.dial = func(_ context.Context, s netip.Addr, d netip.AddrPort) (net.Conn, error) {
		gotSrc, gotDst = s, d
		return nil, errors.New("not dialling in a test")
	}
	n.SetGrants([]Grant{{Address: src}})

	if _, err := n.DialBound(context.Background(), src, dst); err == nil {
		t.Fatal("the seam's error was swallowed")
	}
	if gotSrc != src || gotDst != dst {
		t.Fatalf("seam got %s -> %s, want %s -> %s", gotSrc, gotDst, src, dst)
	}
}

// The kernel dial binds the source it is given. Loopback stands in for the
// overlay address: the property is that LocalAddr is the address asked for,
// not one the routing table picked.
func TestTheKernelDialLeavesFromTheSourceItIsGiven(t *testing.T) {
	ln, err := net.Listen("tcp", "[::1]:0")
	if err != nil {
		t.Skipf("no IPv6 loopback here: %v", err)
	}
	defer ln.Close()
	go func() {
		if c, err := ln.Accept(); err == nil {
			c.Close()
		}
	}()

	src := netip.MustParseAddr("::1")
	dst := ln.Addr().(*net.TCPAddr).AddrPort()
	conn, err := kernelDial(context.Background(), src, dst)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	if got := conn.LocalAddr().(*net.TCPAddr).AddrPort().Addr(); got != src {
		t.Fatalf("left from %s, want %s", got, src)
	}
}

// Close runs the close seam once: a second Close finds nothing to shut down,
// and a Node wrapped around somebody else's server (New) has no seam to run.
func TestCloseRunsTheCloseSeamOnce(t *testing.T) {
	closes := 0
	n := testNode(nil)
	n.close = func() { closes++ }

	n.Close()
	n.Close()
	if closes != 1 {
		t.Fatalf("closed %d times, want 1", closes)
	}

	testNode(nil).Close() // no seam: nothing to run, and no panic
}

// The cleanup restores the resolver configuration and nothing else: the
// router's cleanup hook is not called (engine.go's file comment says why).
func TestCleanUpRestoresTheResolverAndLeavesTheFirewallAlone(t *testing.T) {
	var dnsIface string
	previous := dnsCleanUp
	dnsCleanUp = func(_ logger.Logf, _ *netmon.Monitor, _ *eventbus.Bus, _ *health.Tracker, iface string) {
		dnsIface = iface
	}
	t.Cleanup(func() { dnsCleanUp = previous })

	routerCleanUps := 0
	t.Cleanup(router.HookCleanUp.SetForTest(func(logger.Logf, *netmon.Monitor, string) { routerCleanUps++ }))

	cleanUp(t.Logf, nil, nil, nil)

	if dnsIface != tunName(runtime.GOOS) {
		t.Fatalf("the resolver cleanup ran for %q, want %q", dnsIface, tunName(runtime.GOOS))
	}
	if routerCleanUps != 0 {
		t.Fatalf("the router's cleanup ran %d times; upstream's removes only Tailscale's chains", routerCleanUps)
	}
}
