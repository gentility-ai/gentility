package main

import (
	"context"
	"errors"
	"flag"
	"io"
	"net/http"
	"net/netip"
	"os"
	"os/signal"
	"syscall"

	"github.com/gentility-ai/gentility/connector/internal/config"
	"github.com/gentility-ai/gentility/connector/internal/enroll"
	"github.com/gentility-ai/gentility/connector/internal/exitcodes"
	"github.com/gentility-ai/gentility/connector/internal/report"
	"github.com/gentility-ai/gentility/connector/internal/state"
	"github.com/gentility-ai/gentility/connector/internal/traffic"
	"github.com/gentility-ai/gentility/connector/internal/tsnode"
)

// join runs the node's lifecycle: come up on the overlay, trade the
// nonce for the two secrets, then hold the config channel open and beat
// presence until somebody stops it.
//
// It runs in the FOREGROUND and nothing supervises it here. Whatever started
// it — launchd, systemd, the Builder — reads the exit code, and that is the
// whole of the conversation: exitcodes.Temporary means start me again,
// EnrollRefused and MaterialExpired mean enrol again, CapverTooOld means ship
// a new binary, and Usage means fix the recipe.
//
// There is no retry loop around the exchange, deliberately. A retryable
// failure exits Temporary and the supervisor starts a fresh process, which
// finds the same enrollment and presents the same nonce — the mothership is
// idempotent on it for an hour, so a restart IS the retry, and a loop here
// would be a second answer to the same question.
func join(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet(invoked+" join", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	verbose := flags.Bool("verbose", false, "print the overlay library's own logging")

	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return help(stdout, invoked+" join [--verbose]", flags)
		}
		diagnosticf(stderr, "%s join: %v\n", invoked, err)
		return exitcodes.Usage
	}
	if flags.NArg() > 0 {
		diagnosticf(stderr, "%s join: unexpected argument %q\n", invoked, flags.Arg(0))
		return exitcodes.Usage
	}

	say, complain := narrate("join", stdout, stderr)

	env, err := enroll.Read()
	if err != nil {
		return complain(err)
	}

	// SIGTERM and SIGINT are how a supervisor stops a node. Both end the two
	// loops and let the tsnet server shut down, which is what makes an
	// ephemeral case node disappear from the head rather than linger.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	node, addresses, err := enroll.Join(ctx, env, libraryLogger(*verbose, stderr))
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

	err = configs.Run(ctx, func(document *config.Document) error {
		if err := document.Adopt(node, store); err != nil {
			return err
		}

		say("config rev %d applied", document.Revision)
		return nil
	})

	if errors.Is(err, context.Canceled) {
		return exitcodes.OK
	}
	return complain(err)
}

// identity answers who this node is, where Gentility is, and the client every
// call to it is made with — exchanging the nonce only if it has to.
//
// A restarted connector finds its own state on disk and must NOT exchange
// again: the material it holds is the material the mothership handed over, and
// past the one-hour window a second attempt is a 410 that would kill a node
// that was working perfectly.
//
// Where Gentility answers and how it is dialled is enroll.Mothership's — the
// same hop `serve` reaches it over. What is left here, and the only reason this
// exists at all, is the decision to exchange or not.
func identity(ctx context.Context, env enroll.Env, store *state.Store, node *tsnode.Node, addresses []netip.Addr) (state.Node, string, *http.Client, error) {
	stored, err := store.LoadNode()
	enrolled := err == nil
	if err != nil && !state.IsNotEnrolled(err) {
		return state.Node{}, "", nil, err
	}

	// A node that is not enrolled loads as state.Node{}, which holds no CA, so
	// the first exchange is made over whatever the enrollment carried.
	base, client, err := enroll.Mothership(env, stored, node, addresses)
	if err != nil {
		return state.Node{}, "", nil, err
	}

	if enrolled {
		return stored, base, client, nil
	}

	stored, err = enroll.Exchange(ctx, client, base, env, store)
	if err != nil {
		return state.Node{}, "", nil, err
	}
	return stored, base, client, nil
}

// heartbeat is what a beat says about this node's own path to the overlay.
func heartbeat(
	node *tsnode.Node,
	say func(string, ...any),
) func(context.Context) (report.Status, error) {
	return func(ctx context.Context) (report.Status, error) {
		seen, err := node.Status(ctx)
		if err != nil {
			return report.Status{}, err
		}
		if seen.ObservationsErr != nil {
			say("network observations incomplete: %v", seen.ObservationsErr)
		}

		addresses := make([]string, 0, len(seen.Addresses))
		for _, address := range seen.Addresses {
			addresses = append(addresses, address.String())
		}

		return report.Status{
			NetworkReportVersion: 1,
			PeerPaths:            seen.PeerPaths,
			LocalAddresses:       seen.LocalAddresses,
			HomeDERP:             seen.HomeDERP,
			Endpoint:             seen.Endpoint,
			Relayed:              seen.Relayed,
			Addresses:            addresses,
		}, nil
	}
}
