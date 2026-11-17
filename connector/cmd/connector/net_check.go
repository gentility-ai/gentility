package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/gentility-ai/gentility/connector/internal/exitcodes"
	"github.com/gentility-ai/gentility/connector/internal/state"
	"github.com/gentility-ai/gentility/connector/internal/status"
)

type networkCheck struct {
	PeerPaths       []state.PeerPath `json:"peer_paths"`
	PathsIncomplete bool             `json:"paths_incomplete,omitempty"`
	ReadAt          time.Time        `json:"read_at"`
	WrittenAt       time.Time        `json:"written_at"`
	Probe           *state.UDPProbe  `json:"probe"`
	Stale           bool             `json:"stale"`
	Endpoints       *state.NetCheck  `json:"endpoints,omitempty"`
}

// netCheck reads observations made on the daemon's actual sockets. It does not
// start a second overlay identity or mistake a CLI socket for the daemon's.
func netCheck(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet(invoked+" net check", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	dir := flags.String("state-dir", "", "daemon state directory (default: installed daemon's, else this user's)")
	asJSON := flags.Bool("json", false, "print observations as JSON")
	endpoints := flags.Bool("endpoints", false, "include private peer endpoints (requires root or sudo)")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return help(stdout, invoked+" net check [--endpoints] [--json] [--state-dir <dir>]", flags)
		}
		diagnosticf(stderr, "%s net check: %v\n", invoked, err)
		return exitcodes.Usage
	}
	if flags.NArg() != 0 {
		diagnosticf(stderr, "%s net check: unexpected argument %q\n", invoked, flags.Arg(0))
		return exitcodes.Usage
	}
	if *endpoints && geteuid() != 0 {
		diagnosticf(stderr, "%s net check: peer endpoints are private; run with sudo and --endpoints\n", invoked)
		return 1
	}
	store := &state.Store{Dir: statusDir(unitInstalled(), *dir)}
	public, err := store.LoadStatus()
	if err != nil {
		diagnosticf(stderr, "%s net check: cannot read the daemon's observations: %v\n", invoked, err)
		return 1
	}
	check := networkCheck{WrittenAt: public.WrittenAt, Probe: public.UDPProbe, PeerPaths: public.PeerPaths, PathsIncomplete: public.PathsIncomplete}
	now := time.Now()
	check.ReadAt = now
	check.Stale = staleCheck(public.WrittenAt, now)
	if *endpoints {
		private, err := store.LoadNetCheck()
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				diagnosticf(stderr, "%s net check: endpoint observations unavailable; update the Connector daemon\n", invoked)
			} else {
				diagnosticf(stderr, "%s net check: cannot read private endpoint observations: %v\n", invoked, err)
			}
			return 1
		}
		check.Endpoints = &private
		check.Stale = check.Stale || staleCheck(private.WrittenAt, now)
	}
	if *asJSON {
		if err := json.NewEncoder(stdout).Encode(check); err != nil {
			diagnosticf(stderr, "%s net check: writing observations: %v\n", invoked, err)
			return 1
		}
	} else {
		renderNetCheck(stdout, check)
	}
	if check.Probe == nil || check.Stale || invalidCheckTime(check.Probe.ObservedAt, now) {
		return 1
	}
	return exitcodes.OK
}

func staleCheck(observed, now time.Time) bool {
	return invalidCheckTime(observed, now) || now.Sub(observed) >= status.StaleAfter
}

func invalidCheckTime(observed, now time.Time) bool {
	return observed.IsZero() || observed.After(now)
}

func renderNetCheck(out io.Writer, check networkCheck) {
	fmt.Fprintf(out, "Daemon observations written: %s\n", checkTime(check.WrittenAt, check.ReadAt))
	if staleCheck(check.WrittenAt, check.ReadAt) {
		fmt.Fprintln(out, "Daemon observations are stale or have an invalid timestamp. Check that the Connector daemon is running and the machine's clock is correct.")
	}
	if check.Probe == nil {
		fmt.Fprintln(out, "UDP probe unavailable. Update the Connector daemon, or wait for its first probe.")
	} else {
		p := check.Probe
		fmt.Fprintf(out, "Daemon UDP probe: %s\n", checkTime(p.ObservedAt, check.ReadAt))
		if invalidCheckTime(p.ObservedAt, check.ReadAt) {
			fmt.Fprintln(out, "Probe timestamp is invalid. Check the machine's clock and wait for the daemon's next probe.")
		} else if staleCheck(p.ObservedAt, check.ReadAt) {
			fmt.Fprintln(out, "This is the last probe, not a current connectivity check. Probes pause while idle or without peers; the daemon probes again when it has traffic. Send traffic to a peer and run net check again.")
		}
		if p.UDP {
			fmt.Fprintln(out, "UDP: a STUN round trip completed.")
		} else {
			fmt.Fprintln(out, "UDP: no STUN round trip completed. This alone does not establish that UDP is blocked.")
		}
		if p.GlobalV4 != "" {
			fmt.Fprintf(out, "Probe-observed IPv4 endpoint: %s\n", p.GlobalV4)
		}
		if p.GlobalV6 != "" {
			fmt.Fprintf(out, "Probe-observed IPv6 endpoint: %s\n", p.GlobalV6)
		}
	}
	if check.Endpoints == nil {
		switch {
		case check.PeerPaths == nil:
			fmt.Fprintln(out, "Peer paths unavailable. Update the Connector daemon.")
		case len(check.PeerPaths) == 0:
			fmt.Fprintln(out, "No peer paths observed.")
		default:
			fmt.Fprintln(out, "Peer paths (one per overlay address):")
			for _, peer := range check.PeerPaths {
				fmt.Fprintf(out, "  %s  %s\n", peer.Address, checkPeerPath(peer.Path, peer.Relay))
			}
		}
		if check.PathsIncomplete {
			fmt.Fprintln(out, "Peer observations are incomplete.")
		}
		fmt.Fprintln(out, "Peer endpoints: run with sudo and --endpoints to include them.")
		return
	}
	fmt.Fprintf(out, "Peer observations: %s\n", checkTime(check.Endpoints.WrittenAt, check.ReadAt))
	if staleCheck(check.Endpoints.WrittenAt, check.ReadAt) {
		fmt.Fprintln(out, "Peer observations are stale or have an invalid timestamp. Check that the Connector daemon is running and can write its diagnostic snapshot, and that the machine's clock is correct.")
	}
	if check.Endpoints.Incomplete {
		fmt.Fprintln(out, "Peer observations are incomplete.")
	}
	if len(check.Endpoints.Peers) == 0 {
		fmt.Fprintln(out, "No peers observed.")
	} else {
		fmt.Fprintln(out, "Peer paths (one per overlay address):")
	}
	for _, peer := range check.Endpoints.Peers {
		fmt.Fprintf(out, "  %s  %s", peer.Address, checkPeerPath(peer.Path, peer.Relay))
		if peer.Via != "" {
			fmt.Fprintf(out, " via %s", peer.Via)
		}
		fmt.Fprintln(out)
		known := strings.Join(peer.Endpoints, ", ")
		if peer.EndpointsUnavailable {
			known = "unavailable"
		} else if known == "" {
			known = "none known"
		}
		fmt.Fprintf(out, "    Advertised endpoints: %s\n", known)
		if peer.EndpointsTruncated {
			fmt.Fprintln(out, "    Endpoint list truncated.")
		}
	}
}

func checkTime(observed, now time.Time) string {
	if observed.IsZero() {
		return "timestamp unavailable"
	}
	if observed.After(now) {
		return observed.Format(time.RFC3339) + " (invalid future timestamp)"
	}
	return fmt.Sprintf("%s (%s ago)", observed.Format(time.RFC3339), now.Sub(observed).Truncate(time.Second))
}

func checkPeerPath(path, relay string) string {
	switch path {
	case "direct", "idle":
		return path
	case "derp":
		if relay != "" {
			return "relay (" + relay + ")"
		}
		return "relay"
	case "peer_relay":
		return "peer relay"
	default:
		return "unknown"
	}
}
