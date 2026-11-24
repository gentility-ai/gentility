package tsnode

// Interface mode: the engine with a real network interface.
//
// tsnet cannot do this. It takes a custom TUN but hands the engine neither a
// router nor a DNS configurator (tsnet.Server.start builds its wgengine.Config
// without them, so the engine falls back to router.NewFake and a no-op DNS
// manager), and nothing outside the process can then reach the overlay. So
// interface mode assembles the engine itself, from the importable packages
// cmd/tailscaled assembles it from.
//
// The glue upstream is package main, so it is copied in shape, not imported,
// and tracking it is ours. It follows, at tailscale.com =>
// github.com/gentility-ai/genscale v1.101.0-pre.6net.8:
//
//   - cmd/tailscaled/tailscaled.go run: tsd.NewSystem, then netmon.New, then
//     dns.CleanUp (~481), which always runs before the engine starts.
//     tailscaled's router.CleanUp (~482) is left out. Upstream's Linux hook
//     deletes the ts-* chains, which are an installed Tailscale's (we run
//     NetfilterOff and add none). genscale empties the hook (GENSCALE.md);
//     not calling it as well is belt and braces, should the pin ever go
//     back. Policy routing is the running router's. Its Close deletes rules
//     5110-5170, and the kernel drops table 66's routes with the interface.
//     Its Up deletes those rules before adding them, so a hard kill's
//     leftovers go at the next start; after a hard kill and then leave,
//     nothing starts again and they stay (harmless: table 66 is empty; a debt
//     in the gendesk-6 design). Up does not flush throw routes in table 66,
//     but the router adds those only for LocalRoutes, which no 6net config
//     sets.
//   - cmd/tailscaled/tailscaled.go getLocalBackend: the tsdial.Dialer, the
//     engine (below), store.New, Tun.Start, ipnlocal.NewLocalBackend,
//     SetVarRoot.
//   - cmd/tailscaled/tailscaled.go tryEngine, for a named TUN: tstun.New,
//     router.New, dns.NewOSConfigurator(devName), wgengine.NewUserspaceEngine
//     with Tun, Router and DNS, sys.Set, NetstackRouter false.
//   - cmd/tailscaled/netstack.go newNetstack and getLocalBackend (~645-650,
//     ~690): netstack.Create even with a TUN, ProcessLocalIPs and
//     ProcessSubnets false, sys.Set, and ns.Start(lb) once the backend exists.
//   - tsnet/tsnet.go Server.start: the state file at Dir/tailscaled.state, the
//     login flags (LoginDefault or LoginEphemeral, with
//     LocalBackendStartKeyOSNeutral), lb.Start with the prefs and the auth key,
//     and StartLoginInteractive when the backend comes up in NeedsLogin.
//   - tsnet/tsnet.go Server.Up: wait on the IPN bus, NotifyInitialState, for
//     Running, failing on an ErrMessage.
//   - tsnet/tsnet.go Server.close: the shutdown order.
//
// Coexistence with an installed Tailscale is by genscale's names: 6net's own
// ULA, on Linux route table 66 and rule priorities 5110-5170 (Tailscale's are
// 52 and 5210-5270), and resolver files of our own (genscale's GENSCALE.md).
// What is still shared is recorded in the gendesk-6 design's debts.
//
// Netstack is built, as tailscaled builds it in TUN mode, for one job: only
// netstack's handleLocalPackets answers the MagicDNS service address
// (100.100.100.100 and fd06:6e7::53), and without it nothing resolves
// overlay names. With ProcessLocalIPs and ProcessSubnets false it takes no
// other traffic: the overlay addresses are on the TUN, and the connector's own
// listens and dials stay kernel sockets.
//
// What is left out on purpose: logtail (a zero log ID,
// as tailscaled runs without a log policy), the LocalAPI server and socket
// (nothing outside this process talks to this backend), and the debug,
// proxy, web client and BIRD hooks.

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"

	"tailscale.com/client/local"
	"tailscale.com/client/tailscale/apitype"
	"tailscale.com/control/controlclient"
	"tailscale.com/health"
	"tailscale.com/ipn"
	"tailscale.com/ipn/ipnlocal"
	"tailscale.com/ipn/ipnstate"
	"tailscale.com/ipn/store"
	"tailscale.com/net/dns"
	"tailscale.com/net/netcheck"
	"tailscale.com/net/netmon"
	"tailscale.com/net/tsdial"
	"tailscale.com/net/tstun"
	"tailscale.com/tailcfg"
	"tailscale.com/tsd"
	"tailscale.com/types/logger"
	"tailscale.com/types/logid"
	"tailscale.com/types/netmap"
	"tailscale.com/types/preftype"
	"tailscale.com/util/eventbus"
	"tailscale.com/wgengine"
	"tailscale.com/wgengine/netstack"
	"tailscale.com/wgengine/router"

	// The OS router registers itself through router.HookNewUserspaceRouter;
	// without this import router.New answers "unsupported OS".
	_ "tailscale.com/wgengine/router/osrouter"
)

// ErrNeedsRoot is interface mode started by anyone but root. Creating a TUN,
// adding addresses and routes and writing the system's resolver configuration
// are all root's, and a node that found that out halfway would leave half of
// them behind.
var ErrNeedsRoot = errors.New("interface mode needs root")

// CleanUp restores what an interface-mode daemon that stopped without closing
// may have left behind, exactly as the engine's own start does: the system
// resolver configuration, and no firewall or policy routing (see the file
// comment). It is what `cleanup --interface` runs after the daemon stops,
// from systemd's ExecStopPost and from leave. It needs root, as the
// configuration it restores is root's.
func CleanUp(logf logger.Logf) error {
	if os.Geteuid() != 0 {
		return fmt.Errorf("tsnode: %w: the system resolver is root's", ErrNeedsRoot)
	}
	if logf == nil {
		logf = logger.Discard
	}
	sys := tsd.NewSystem()
	bus := sys.Bus.Get()
	defer bus.Close()
	netMon, err := netmon.New(bus, logf)
	if err != nil {
		return fmt.Errorf("tsnode: netmon: %w", err)
	}
	defer netMon.Close()
	cleanUp(logf, netMon, bus, sys.HealthTracker.Get())
	return nil
}

// cleanUp is upstream's resolver cleanup for this mode's interface name,
// shared by the engine's start and CleanUp so the two cannot drift. Not
// router.CleanUp: see the file comment.
func cleanUp(logf logger.Logf, netMon *netmon.Monitor, bus *eventbus.Bus, ht *health.Tracker) {
	dnsCleanUp(logf, netMon, bus, ht, tunName(runtime.GOOS))
}

// dnsCleanUp is dns.CleanUp, held in a variable so a test can run cleanUp
// without touching this machine's resolver.
var dnsCleanUp = dns.CleanUp

// tunName is the TUN interface mode asks for. On macOS a utun is named by the
// kernel, and "utun" asks for the next free one. On Linux the name is ours, and
// it is not tailscale0, so an installed tailscaled's device does not collide
// with this one.
func tunName(goos string) string {
	if goos == "darwin" {
		return "utun"
	}
	return "gentility0"
}

// interfacePrefs are the prefs an interface-mode node starts with: tsnet's
// identity prefs, and three that only matter once there is a real interface.
//
// NetfilterMode off: no iptables or nftables rules of ours. The engine's own
// filter, rendered from the World, is the filter (default deny is unchanged).
// RouteAll off and no exit node: only the overlay is routed, never a peer's
// subnet or the default route. CorpDNS on: overlay names resolve for every
// program on the machine.
func interfacePrefs(opts Options) *ipn.Prefs {
	prefs := ipn.NewPrefs()
	prefs.ControlURL = opts.ControlURL
	prefs.Hostname = opts.Hostname
	prefs.WantRunning = true
	prefs.NetfilterMode = preftype.NetfilterOff
	prefs.RouteAll = false
	prefs.ExitNodeID = ""
	prefs.CorpDNS = true
	return prefs
}

// upInterface brings a node up with the overlay on a real network interface.
//
// It holds Up's contract — the same refusals, the same identity file, the same
// reading of a rejoin the head refuses — and returns a Node with the same
// methods. Underneath, the overlay addresses are on a kernel interface, so the
// Node's listens and dials are ordinary kernel sockets bound to them.
//
// Every error after the first thing is opened closes what was opened, in
// reverse order.
func upInterface(ctx context.Context, opts Options) (_ *Node, reterr error) {
	// Before anything else, and certainly before the TUN.
	if os.Geteuid() != 0 {
		return nil, fmt.Errorf("tsnode: %w: a TUN, its routes and the system resolver are root's", ErrNeedsRoot)
	}
	if err := checkJoin(opts); err != nil {
		return nil, err
	}
	// tsnet makes its Dir 0700 before anything else (Server.start); so do we,
	// so the state file lands where it would have.
	if err := os.MkdirAll(opts.Dir, 0o700); err != nil {
		return nil, fmt.Errorf("tsnode: state directory: %w", err)
	}

	logf := logger.Logf(logger.Discard)
	if opts.Logf != nil {
		logf = opts.Logf
	}

	// undo is what has been opened, last first. Each step that hands
	// ownership on (the TUN, router and DNS to the engine; the engine to the
	// backend) replaces its entries with the new owner's.
	var undo []func()
	defer func() {
		if reterr != nil {
			for i := len(undo) - 1; i >= 0; i-- {
				undo[i]()
			}
		}
	}()

	// run: the System and its bus first, so everything after has a bus.
	sys := tsd.NewSystem()
	bus := sys.Bus.Get()
	undo = append(undo, bus.Close)

	netMon, err := netmon.New(bus, logf)
	if err != nil {
		return nil, fmt.Errorf("tsnode: netmon: %w", err)
	}
	sys.Set(netMon)
	undo = append(undo, func() { netMon.Close() })

	// run (~478-481): "Always clean up, even if we're going to run the
	// server." A previous run killed without closing — SIGKILL, OOM, a power
	// cut — left its resolver configuration behind; on a host using the
	// direct resolv.conf manager that is /etc/resolv.conf pointing at the
	// MagicDNS resolver, and a rejoin refused before the engine sets DNS
	// again would leave the machine with no DNS at all. Restore first.
	cleanUp(logf, netMon, bus, sys.HealthTracker.Get())

	// getLocalBackend: the dialer, mutated by the engine before use.
	dialer := &tsdial.Dialer{Logf: logf}
	dialer.SetBus(bus)
	sys.Set(dialer)
	undo = append(undo, func() { dialer.Close() })

	// tryEngine, for a named TUN.
	engineAt := len(undo)
	dev, devName, err := tstun.New(logf, tunName(runtime.GOOS))
	if err != nil {
		tstun.Diagnose(logf, tunName(runtime.GOOS), err)
		return nil, fmt.Errorf("tsnode: creating the TUN: %w", err)
	}
	undo = append(undo, func() { dev.Close() })

	rtr, err := router.New(logf, dev, netMon, sys.HealthTracker.Get(), bus)
	if err != nil {
		return nil, fmt.Errorf("tsnode: the OS router: %w", err)
	}
	undo = append(undo, func() { rtr.Close() })

	osDNS, err := dns.NewOSConfigurator(logf, sys.HealthTracker.Get(), bus, sys.PolicyClientOrDefault(), sys.ControlKnobs(), devName)
	if err != nil {
		return nil, fmt.Errorf("tsnode: the OS DNS configurator: %w", err)
	}
	undo = append(undo, func() { osDNS.Close() })

	sys.HealthTracker.Get().SetMetricsRegistry(sys.UserMetricsRegistry())
	sys.Set(rtr)
	engine, err := wgengine.NewUserspaceEngine(logf, wgengine.Config{
		Tun:           dev,
		Router:        rtr,
		DNS:           osDNS,
		NetMon:        netMon,
		HealthTracker: sys.HealthTracker.Get(),
		ExtraRootCAs:  sys.ExtraRootCAs,
		Metrics:       sys.UserMetricsRegistry(),
		Dialer:        dialer,
		SetSubsystem:  sys.Set,
		ControlKnobs:  sys.ControlKnobs(),
		EventBus:      bus,
		// An outbound SYN to a 6net machine this node holds no grant for is
		// dropped by wireguard-go; this is how it is seen (refusal.go).
		OnNoPeer: onNoPeer,
	})
	if err != nil {
		// A failed engine has closed its TUN wrapper, and with it dev; the undo
		// closes dev again, which wireguard-go's closeOnce makes harmless.
		return nil, fmt.Errorf("tsnode: the engine: %w", err)
	}
	sys.Set(engine)
	// The kernel routes; the netstack below routes nothing.
	sys.NetstackRouter.Set(false)
	// The engine owns the TUN, the router and the DNS configurator now, and
	// its Close shuts them down: DNS down, router closed, TUN closed.
	undo = append(undo[:engineAt], engine.Close)

	// newNetstack: the service address's answerer, and nothing else. The
	// local IPs are the kernel's, and there are no subnets.
	ns, err := netstack.Create(logf, sys.Tun.Get(), engine, sys.MagicSock.Get(), dialer, sys.DNSManager.Get(), sys.ProxyMapper())
	if err != nil {
		return nil, fmt.Errorf("tsnode: netstack: %w", err)
	}
	ns.ProcessLocalIPs = false
	ns.ProcessSubnets = false
	sys.Set(ns)
	// Last in, so first undone: tsnet Server.close closes the netstack
	// before the backend.
	undo = append(undo, func() { ns.Close() })

	// getLocalBackend: the store, at the path tsnet uses, so a node that
	// switches mode comes back as itself.
	stateStore, err := store.New(logf, filepath.Join(opts.Dir, stateFile))
	if err != nil {
		return nil, fmt.Errorf("tsnode: state store: %w", err)
	}
	sys.Set(stateStore)
	sys.Tun.Get().Start()

	// tsnet's login flags.
	loginFlags := controlclient.LoginDefault
	if opts.Ephemeral {
		loginFlags = controlclient.LoginEphemeral
	}
	// NewLocalBackend evaluates logpolicy.LogsDir for its sockstat logger, and
	// that, unless TS_LOGS_DIR says otherwise, creates the platform's
	// tailscale directory (/var/lib/tailscale) as root — a directory of a
	// product this machine may not have. TS_LOGS_DIR is the variable LogsDir
	// honours first; pointing it at Dir keeps everything inside our state.
	if os.Getenv("TS_LOGS_DIR") == "" {
		if err := os.Setenv("TS_LOGS_DIR", opts.Dir); err != nil {
			return nil, fmt.Errorf("tsnode: TS_LOGS_DIR: %w", err)
		}
	}
	lb, err := ipnlocal.NewLocalBackend(logf, logid.PublicID{}, sys, loginFlags|controlclient.LocalBackendStartKeyOSNeutral)
	if err != nil {
		return nil, fmt.Errorf("tsnode: the local backend: %w", err)
	}
	// Shutdown closes the engine, and with it everything the engine owns.
	undo[engineAt] = lb.Shutdown
	lb.SetVarRoot(opts.Dir)
	if err := ns.Start(lb); err != nil {
		return nil, fmt.Errorf("tsnode: starting the netstack: %w", err)
	}

	// tsnet Server.start: start with the prefs and the key, and log in when
	// the backend comes up asking to. With a key that login presents it; on a
	// rejoin the head has forgotten, it produces the URL the wait refuses.
	if err := lb.Start(ipn.Options{UpdatePrefs: interfacePrefs(opts), AuthKey: opts.AuthKey}); err != nil {
		return nil, fmt.Errorf("tsnode: starting the backend: %w", err)
	}
	if netDebugWanted(os.LookupEnv) {
		// The engine set the magicsock on sys above; netstack already used it.
		lb.MagicConn().SetDebugLoggingEnabled(true)
	}
	if lb.State() == ipn.NeedsLogin {
		if err := lb.StartLoginInteractive(ctx); err != nil {
			return nil, fmt.Errorf("tsnode: logging in: %w", err)
		}
	}

	if err := awaitRunning(ctx, lb, opts.AuthKey == ""); err != nil {
		if errors.Is(err, ErrRejoinRefused) {
			return nil, fmt.Errorf("tsnode: rejoining %s from %q: %w", opts.ControlURL, opts.Dir, err)
		}
		return nil, fmt.Errorf("tsnode: joining %s: %w", opts.ControlURL, err)
	}

	return interfaceNode(lb, devName, func() {
		// tsnet Server.close's order, without the LocalAPI.
		ns.Close()
		lb.Shutdown()
		netMon.Close()
		dialer.Close()
		bus.Close()
	}), nil
}

// awaitRunning waits on lb's IPN bus until the backend is Running, the context
// ends, or runningVerdict calls it off.
//
// NotifyInitialState makes the current state the first notification, so a
// backend that is already Running — or already asking for a login — is seen,
// whatever happened before the watch was attached.
func awaitRunning(ctx context.Context, lb *ipnlocal.LocalBackend, rejoin bool) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var verdict error
	running := false
	lb.WatchNotifications(ctx, ipn.NotifyInitialState, nil, func(n *ipn.Notify) bool {
		done, err := runningVerdict(*n, rejoin)
		if done {
			verdict, running = err, err == nil
		}
		return !done
	})

	switch {
	case verdict != nil:
		return verdict
	case running:
		return nil
	case ctx.Err() != nil:
		return ctx.Err()
	default:
		return errors.New("the backend stopped sending notifications before it was running")
	}
}

// runningVerdict reads one notification of the wait for Running: done says the
// wait is over, and err why it failed.
//
// The rejoin rule is watchForLogin's: a backend asking for a login is a
// refusal only on a rejoin. A first join passes through NeedsLogin while the
// key is presented, and reading that as a refusal would refuse every good
// join.
func runningVerdict(n ipn.Notify, rejoin bool) (done bool, err error) {
	if n.ErrMessage != nil {
		return true, fmt.Errorf("backend: %s", *n.ErrMessage)
	}
	if rejoin && needsLogin(n) {
		return true, ErrRejoinRefused
	}
	if n.State != nil && *n.State == ipn.Running {
		return true, nil
	}
	return false, nil
}

// interfaceNode is the Node over an assembled engine. It never asserts the
// netstack, which here answers only the MagicDNS service address, and every
// seam is either a kernel socket or the backend answering directly.
//
// The kernel sockets are sound because the router has put the overlay
// addresses on iface before the backend reports Running: the backend
// reconfigures the engine, router included, on entering Starting, and only
// leaves Starting for Running.
func interfaceNode(lb *ipnlocal.LocalBackend, iface string, closeEngine func()) *Node {
	return &Node{
		listen: net.Listen,
		dial:   kernelDial,
		whois: func(_ context.Context, remoteAddr string) (*apitype.WhoIsResponse, error) {
			remote, err := netip.ParseAddrPort(remoteAddr)
			if err != nil {
				return nil, err
			}
			return backendWhoIs(lb.WhoIs("tcp", remote))
		},
		status: func(context.Context) (*ipnstate.Status, error) {
			return lb.Status(), nil
		},
		peerMap: func(context.Context) (*netmap.NetworkMap, error) {
			return lb.NetMapWithPeers(), nil
		},
		close: closeEngine,
		probe: func(ctx context.Context) *netcheck.Report {
			return lb.MagicConn().GetLastNetcheckReport(ctx)
		},
		iface:     iface,
		listeners: map[netip.Addr]net.Listener{},
	}
}

// kernelDial opens a TCP connection to dst from src as an ordinary kernel
// socket. LocalAddr is the whole point: the source is the one DialBound
// checked against the grants, not whichever overlay address the routing table
// would have picked.
func kernelDial(ctx context.Context, src netip.Addr, dst netip.AddrPort) (net.Conn, error) {
	dialer := &net.Dialer{LocalAddr: &net.TCPAddr{IP: src.AsSlice()}}
	return dialer.DialContext(ctx, "tcp", dst.String())
}

// backendWhoIs is the LocalAPI's whois answer built from the backend's, so
// the two modes read alike: the same response, and the same
// local.ErrPeerNotFound for a peer the netmap does not hold.
func backendWhoIs(n tailcfg.NodeView, u tailcfg.UserProfile, ok bool) (*apitype.WhoIsResponse, error) {
	if !ok {
		return nil, local.ErrPeerNotFound
	}
	return &apitype.WhoIsResponse{Node: n.AsStruct(), UserProfile: &u}, nil
}
