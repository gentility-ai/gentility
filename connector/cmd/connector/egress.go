package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"os/signal"
	"strconv"
	"syscall"

	"github.com/gentility-ai/gentility/connector/internal/config"
	"github.com/gentility-ai/gentility/connector/internal/egress"
	"github.com/gentility-ai/gentility/connector/internal/enroll"
	"github.com/gentility-ai/gentility/connector/internal/exitcodes"
	"github.com/gentility-ai/gentility/connector/internal/report"
	"github.com/gentility-ai/gentility/connector/internal/state"
	"github.com/gentility-ai/gentility/connector/internal/traffic"
)

// runEgress runs the `egress` service on an already-joined node: it comes up
// on the overlay exactly as `join` does, then binds a CONNECT relay on the
// node's own overlay address for every approved `egress` service its config
// document names.
//
// It calls the same egress.StartServices as `serve`. The relay is authoritative
// and caches nothing: every CONNECT it accepts is a synchronous admit call back to
// Gentility over the overlay, with this node's own credential.
func runEgress(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet(invoked+" egress", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	verbose := flags.Bool("verbose", false, "print the overlay library's own logging")

	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return help(stdout, invoked+" egress [--verbose]", flags)
		}
		diagnosticf(stderr, "%s egress: %v\n", invoked, err)
		return exitcodes.Usage
	}
	if flags.NArg() > 0 {
		diagnosticf(stderr, "%s egress: unexpected argument %q\n", invoked, flags.Arg(0))
		return exitcodes.Usage
	}

	say, complain := narrate("egress", stdout, stderr)

	env, err := enroll.Read()
	if err != nil {
		return complain(err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	libraryLog := libraryLogger(*verbose, stderr)

	node, addresses, err := enroll.Join(ctx, env, libraryLog)
	if err != nil {
		return complain(err)
	}
	defer node.Close()
	say("joined as %s with %d address(es)", env.Hostname, len(addresses))

	store := &state.Store{Dir: env.StateDir}

	stored, base, client, err := identity(ctx, env, store, node, addresses)
	if err != nil {
		return complain(err)
	}
	say("exchanged: node %s", stored.NodeID)

	// The admit client dials Gentility over the same overlay-dialling http.Client
	// the config pull uses, so its source is pinned to this node the same way,
	// and carries this node's own credential — the relay's authority.
	admit := egress.NewAdmitClient(client, base, stored.NodeCredential)

	configs := config.New(base, stored, store, client)
	configs.Log = say

	presence := &report.Reporter{
		BaseURL:    base,
		Credential: stored.NodeCredential,
		HTTP:       client,
		Status:     heartbeat(node, say),
		Ready:      configs.Ready,
		Log:        func(format string, args ...any) { say(format, args...) },
	}
	go presence.Run(ctx)

	counted := &traffic.Reporter{
		BaseURL:    base,
		Credential: stored.NodeCredential,
		HTTP:       client,
		Read:       node.PeerCounters,
		Ready:      configs.Ready,
		Log:        func(format string, args ...any) { say(format, args...) },
	}
	go counted.Run(ctx)

	factory := egress.OverlayListeners{Node: node}

	var relays []*egress.Relay
	started := false

	err = configs.Run(ctx, func(document *config.Document) error {
		if err := document.Adopt(node, store); err != nil {
			return err
		}

		// Bind on the first revision that names an approved egress service, then
		// never again. A relay connector joins and advertises its service before a
		// person approves it with `serve`: the approval lands in a later
		// config revision, so binding on the literal first document would serve
		// nothing forever. StartServices binds only `approved` egress services and
		// returns an empty slice until one appears, so an empty result means "not
		// approved yet, keep looping" and a non-empty one latches `started`.
		//
		// Once bound we stay bound: the relay caches no allowlist, so a later
		// revision changes what Gentility answers, not what this process serves,
		// and the addresses and approved services a relay is bound to change
		// rarely enough that a rebind belongs to a restart, not to every 304.
		if !started {
			relays, err = egress.StartServices(ctx, egress.NewConfig(document, node, admit), factory)
			if err != nil {
				return err
			}
			if len(relays) > 0 {
				started = true
				say("egress: serving %d relay(s)", len(relays))
			}
		}

		say("config rev %d applied", document.Revision)
		return nil
	})

	// Drain the close reports of any tunnel still open at shutdown: StartServices
	// force-closed them when ctx was cancelled, and WaitIdle waits for the byte
	// count to reach Gentility rather than losing it to exit.
	for _, r := range relays {
		r.WaitIdle()
	}

	if errors.Is(err, context.Canceled) {
		return exitcodes.OK
	}
	return complain(err)
}

// runProxy runs the case-side preflight proxy: a loopback CONNECT proxy the
// Builder points HTTPS_PROXY at. It reads config.egress, applies
// preflight_allow as a fast legible refusal for the agent (UX only), and
// forwards CONNECT over the overlay to the relay with a source pinned to this
// node's own address in the org — so the relay's WhoIs attributes it and the
// destination org's filter permits it.
//
// If the config document carries no egress block the org has selected no relay
// yet: there is no egress path to serve, so this waits — a later revision may
// carry one (a reach grant is derived at runtime start, and the proxy's first
// config pull can win that race), and binding once it appears mirrors runEgress.
func runProxy(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet(invoked+" proxy", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	verbose := flags.Bool("verbose", false, "print the overlay library's own logging")

	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return help(stdout, invoked+" proxy [--verbose]", flags)
		}
		diagnosticf(stderr, "%s proxy: %v\n", invoked, err)
		return exitcodes.Usage
	}
	if flags.NArg() > 0 {
		diagnosticf(stderr, "%s proxy: unexpected argument %q\n", invoked, flags.Arg(0))
		return exitcodes.Usage
	}

	say, complain := narrate("proxy", stdout, stderr)

	env, err := enroll.Read()
	if err != nil {
		return complain(err)
	}

	// Where the preflight binds, resolved BEFORE the join: a malformed
	// OVERLAY_PROXY_PORT is a recipe that went wrong and will be just as wrong
	// on the next start, so it costs a usage refusal here rather than a spent
	// join and a listener that never binds.
	listenAddr, err := proxyListenAddr(os.LookupEnv)
	if err != nil {
		return complain(err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	libraryLog := libraryLogger(*verbose, stderr)

	node, addresses, err := enroll.Join(ctx, env, libraryLog)
	if err != nil {
		return complain(err)
	}
	defer node.Close()
	say("joined as %s with %d address(es)", env.Hostname, len(addresses))

	store := &state.Store{Dir: env.StateDir}

	stored, base, client, err := identity(ctx, env, store, node, addresses)
	if err != nil {
		return complain(err)
	}
	say("exchanged: node %s", stored.NodeID)

	configs := config.New(base, stored, store, client)
	configs.Log = say

	presence := &report.Reporter{
		BaseURL:    base,
		Credential: stored.NodeCredential,
		HTTP:       client,
		Status:     heartbeat(node, say),
		Ready:      configs.Ready,
		Log:        func(format string, args ...any) { say(format, args...) },
	}
	go presence.Run(ctx)

	counted := &traffic.Reporter{
		BaseURL:    base,
		Credential: stored.NodeCredential,
		HTTP:       client,
		Read:       node.PeerCounters,
		Ready:      configs.Ready,
		Log:        func(format string, args ...any) { say(format, args...) },
	}
	go counted.Run(ctx)

	started := false

	err = configs.Run(ctx, func(document *config.Document) error {
		if _, err := document.AdoptGrants(node); err != nil {
			return err
		}
		// Logged every revision, before the gates below, so a proxy that is
		// legitimately waiting for its egress block still shows a heartbeat rather
		// than looking hung (runEgress logs the same way).
		say("config rev %d applied", document.Revision)

		if started {
			return nil
		}
		// No egress block yet: the org has selected no relay, or its reach grant
		// has not rendered into this node's document. Keep looping — bind once the
		// egress block appears, the same way runEgress waits for approval.
		if document.Egress == nil {
			return nil
		}

		via, err := netip.ParseAddrPort(net.JoinHostPort(document.Egress.Via, strconv.Itoa(document.Egress.Port)))
		if err != nil {
			return fmt.Errorf("egress.via %q:%d is not an overlay address and port: %w",
				document.Egress.Via, document.Egress.Port, err)
		}

		proxy := &egress.Preflight{
			Allow: document.Egress.PreflightAllow,
			DialRelay: func(ctx context.Context) (net.Conn, error) {
				// DialInOrg picks this node's own address in the relay's org and
				// pins the source to it, so the relay's WhoIs attributes the
				// connection and the org's packet filter permits it.
				return node.DialInOrg(ctx, via)
			},
		}

		// Loopback only (proxyListenAddr guarantees 127.0.0.1): the preflight is
		// inside the sandbox and reachable by the case's own tools, never by
		// another host. The port is ephemeral unless OVERLAY_PROXY_PORT pins it.
		ln, err := net.Listen("tcp", listenAddr)
		if err != nil {
			return fmt.Errorf("binding the preflight proxy on loopback: %w", err)
		}
		say("HTTPS_PROXY=http://%s", ln.Addr())

		go func() {
			<-ctx.Done()
			_ = ln.Close()
		}()
		go func() {
			if err := proxy.Serve(ln); err != nil && ctx.Err() == nil {
				say("proxy: stopped: %v", err)
			}
		}()

		started = true
		return nil
	})

	if errors.Is(err, context.Canceled) {
		return exitcodes.OK
	}
	return complain(err)
}

// proxyListenAddr is the loopback address the preflight proxy binds. By
// default the port is ephemeral (:0) — development and live checks
// let the OS choose and read it back off the connector's `HTTPS_PROXY=` log
// line. When OVERLAY_PROXY_PORT is set the Builder has already pointed the
// sandbox's HTTPS_PROXY at a port it chose, so the proxy must bind THAT port
// rather than one nobody outside can predict.
//
// It is proxy-specific, so it is read here through lookup (os.LookupEnv
// outside the tests) rather than parsed into the shared enroll.Env that join,
// gateway and egress also read. The host is always loopback: the preflight is
// inside the sandbox and must never be reachable by another host.
func proxyListenAddr(lookup func(string) (string, bool)) (string, error) {
	raw, ok := lookup(enroll.VarProxyPort)
	if !ok || raw == "" {
		return "127.0.0.1:0", nil
	}

	port, err := strconv.Atoi(raw)
	if err != nil || port < 1 || port > 65535 {
		return "", exitcodes.Wrap(exitcodes.Usage, fmt.Errorf(
			"%s is %q, which is not a TCP port in 1..65535", enroll.VarProxyPort, raw,
		))
	}

	return net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), nil
}
