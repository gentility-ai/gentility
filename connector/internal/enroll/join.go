package enroll

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"path/filepath"

	"github.com/gentility-ai/gentility/connector/internal/exitcodes"
	"github.com/gentility-ai/gentility/connector/internal/state"
	"github.com/gentility-ai/gentility/connector/internal/tsnode"
)

// Join brings this node up on the overlay with its single-use pre-auth key,
// and leaves it able to dial.
//
// Three things happen and the order is not free:
//
//  1. tsnode.Up presents the key and blocks until the netmap arrives, which is
//     the moment the head has decided what addresses this node holds;
//  2. those addresses are adopted as grants, because the exchange has to be
//     dialled from one of them and the config document that would name them
//     properly is what the exchange earns;
//  3. nothing else. The node serves nothing until it holds a config document,
//     so there is no listener to bind here.
//
// tsnet keeps its own state in state.TSNetDir beside the connector's own files,
// never among them: one is the library's to write and the other is ours, and a
// library that tidied up its directory would take the node's credential with
// it.
//
// The returned Node owns its tsnet server; Close shuts the whole node down.
func Join(ctx context.Context, env Env, logf func(string, ...any)) (*tsnode.Node, []netip.Addr, error) {
	node, err := tsnode.Up(ctx, tsnode.Options{
		ControlURL: env.HeadURL,
		AuthKey:    env.PreauthKey,
		Hostname:   env.Hostname,
		Dir:        filepath.Join(env.StateDir, state.TSNetDir),
		Ephemeral:  env.Ephemeral,
		Interface:  env.Interface,
		Logf:       logf,
	})
	if err != nil {
		return nil, nil, upRefusal(err)
	}

	addresses, err := node.AdoptAddresses(ctx)
	if err != nil {
		node.Close()
		return nil, nil, err
	}

	return node, addresses, nil
}

// upRefusal names the exit code for the two refusals Up can make that a
// restart would only repeat.
//
// Interface mode started without root is Usage: the command is wrong, not the
// moment, and the units list Usage among the codes a supervisor must not
// restart into. Left as Temporary it would be retried every two seconds.
//
// A head that no longer holds this node's identity is EnrollRefused, which is
// the code a revoked node's exchange already gets, because it is the same fact
// and the same remedy: the mothership does not recognise this node, a restart
// will not change that, and only a person minting a new enrollment can. The unit files list that code among the ones
// systemd must not restart into (internal/supervise), so a stale state
// directory stops the service instead of knocking on the head every two
// seconds forever.
//
// Everything else Up can fail with stays retryable and comes back as Temporary,
// because everything else — a head that is down, a network that is not up yet —
// is fixed by starting again.
func upRefusal(err error) error {
	if errors.Is(err, tsnode.ErrNeedsRoot) {
		return exitcodes.Wrap(exitcodes.Usage, err)
	}
	if errors.Is(err, tsnode.ErrRejoinRefused) {
		return exitcodes.Wrap(exitcodes.EnrollRefused, err)
	}
	return err
}

// Dialer is the dial every call to Gentility is made through: the destination
// decides the organization, and the source is pinned to this node's own
// address in it.
//
// Primary-source dialling is what this prevents. A node holding grants in two
// organizations has one primary address, and a connection to the second
// organization's gateway leaving from the first organization's address is
// dropped by that gateway's packet filter with no error and an eight-second
// timeout.
func Dialer(node *tsnode.Node) func(context.Context, string, string) (net.Conn, error) {
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		if network != "tcp" && network != "tcp4" && network != "tcp6" {
			return nil, fmt.Errorf("enroll: the overlay carries tcp, not %q", network)
		}

		dst, err := netip.ParseAddrPort(address)
		if err != nil {
			return nil, fmt.Errorf("enroll: %q is not an overlay address and port: %w", address, err)
		}

		return node.DialInOrg(ctx, dst)
	}
}
