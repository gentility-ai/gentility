// Package serve is a server's whole life on the overlay: come up, hold the
// config channel open, beat presence, and run whatever relay the organization
// has approved — for as long as somebody lets it.
//
// It is `join`'s loop grown a memory. A case node is handed its material by the
// Builder on every start and forgets everything when it goes; a server is
// enrolled once, by a person, and has to come back from a reboot on its own.
// That difference is the whole of this package: there are two ways in, and
// which one a start takes is decided by what is on disk.
//
//   - **Reconnect.** node.json is there, so this node has an identity. It
//     rejoins the head on the node key tsnet persisted beside it — with no
//     pre-auth key, which was single-use and expired ten minutes after it was
//     minted — and it does NOT exchange: that already happened, and the
//     material it happened with was purged an hour later.
//   - **First start.** node.json is absent and enrollment.json is there. The
//     enrollment is spent on an identity and then cleared, because what is left
//     is an hour-lived bearer of two secrets and node.json now holds everything
//     a rejoin needs.
//
// Anything else is a refusal, and each one names what a person has to type.
// A refusal is not a retry: `serve` runs under a supervisor that reads the exit
// code, and exit 2, 3, 4 and 5 all mean "do not start me again" (internal/
// exitcodes). Restarting into a refusal is a loop nobody watches.
package serve

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"time"

	"github.com/gentility-ai/gentility/connector/internal/enroll"
	"github.com/gentility-ai/gentility/connector/internal/exitcodes"
	"github.com/gentility-ai/gentility/connector/internal/forward"
	"github.com/gentility-ai/gentility/connector/internal/state"
	"github.com/gentility-ai/gentility/connector/internal/tsnode"
)

// DefaultEgressPort is the port a server offers its egress relay on unless
// somebody says otherwise. It is a declaration and not a permission: what the
// relay binds is whatever port the APPROVED service in the config document
// names, which is this one until a person changes it.
const DefaultEgressPort = 8443

// Options are what the command line says, already defaulted. The defaults
// belong to the command, where an operator can see and override them, not here
// — see cmd/connector.
type Options struct {
	// StateDir is where this node's identity lives, and where tsnet keeps the
	// node key that brings it back.
	StateDir string
	// Hostname is the name to join under on a FIRST start. A reconnect ignores
	// it and uses the name the head already knows this node by.
	Hostname string
	// EgressPort is the port this node advertises its egress relay on. It is a
	// declaration and not a permission: what the relay actually binds is the
	// port of the APPROVED service in the config document.
	EgressPort int
	// Interface puts the overlay on a real network interface rather than a
	// netstack inside this process, so the machine's own programs can reach
	// it. It needs root, which is why only `install --interface` sets it.
	Interface bool
	// Forward is `--forward`: the one declared service this node carries to a
	// local socket (internal/forward). The zero Spec forwards nothing. A node
	// that forwards offers nothing on its heartbeat and runs no relay.
	Forward forward.Spec
}

// Session is a node that is up on the overlay: the tsnet node and the addresses
// the head just attributed to it.
type Session struct {
	Node      *tsnode.Node
	Addresses []netip.Addr
	// Close puts the node back down. Nil is nothing to put down, which is what
	// a test that never joined anything has.
	Close func()
}

func (s Session) close() {
	if s.Close != nil {
		s.Close()
	}
}

// Attendance is everything the long-lived half runs on: a node that is up on
// the overlay, the identity it holds, and where both are written down.
type Attendance struct {
	Env      enroll.Env
	Store    *state.Store
	Identity state.Node
	Session  Session
}

// Deps is the network, behind func fields.
//
// What Run decides — which door this start takes, whether the enrollment is
// spent, when it is forgotten, which refusals never reach the head at all — is
// worth testing, and none of it needs an overlay. What is behind these three is
// the part that does; Default fills them with the real thing.
type Deps struct {
	// Join brings this node up on the overlay. enroll.Join, outside the tests.
	Join func(ctx context.Context, env enroll.Env) (Session, error)

	// Exchange trades the enrollment's nonce for this node's identity and
	// writes it down. Only the first-start door reaches it.
	Exchange func(ctx context.Context, env enroll.Env, store *state.Store, session Session) (state.Node, error)

	// Attend is the long-lived half: presence, the config channel, and the
	// relay the documents reconcile. It returns when the context ends or a
	// refusal is fatal.
	Attend func(ctx context.Context, opts Options, a Attendance) error

	// Log is where serve says what it is doing. Nil is silence.
	Log func(format string, args ...any)
}

// Run is `connector serve`: one node, held on the overlay until ctx ends.
//
// It returns nil on a clean stop — a supervisor's SIGTERM, cancelling ctx — and
// otherwise the error the caller turns into an exit code with exitcodes.Of. A
// too-old capver comes back from Attend as a fatal error and is returned as
// one: there is no fail-static mode, because the config channel is what carries
// revocations and allowlists, and a node that kept serving while its lifeline
// was being refused would be a node nobody could revoke.
func Run(ctx context.Context, opts Options, deps Deps) error {
	logf := deps.Log
	if logf == nil {
		logf = func(string, ...any) {}
	}

	store := &state.Store{Dir: opts.StateDir}

	env, identity, spend, err := door(store, opts)
	if err != nil {
		return err
	}
	env.Interface = opts.Interface

	session, err := deps.Join(ctx, env)
	if err != nil {
		return err
	}
	defer session.close()
	logf("joined as %s with %d address(es)", env.Hostname, len(session.Addresses))

	if spend {
		if identity, err = deps.Exchange(ctx, env, store, session); err != nil {
			return err
		}
		logf("exchanged: node %s", identity.NodeID)

		// The moment there is an identity, and not before: node.json now holds
		// everything a rejoin needs, and what is left in enrollment.json is an
		// hour-lived bearer of two secrets that can buy nothing more. A clear
		// that failed is worth saying and not worth stopping for — the material
		// is spent either way, and a node that refused to serve over it would be
		// one a stale file could take down.
		if err := store.ClearEnrollment(); err != nil {
			logf("could not forget the spent enrollment: %v", err)
		}
	}

	err = deps.Attend(ctx, opts, Attendance{
		Env: env, Store: store, Identity: identity, Session: session,
	})
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

// door decides how this start gets onto the overlay: on the identity it already
// holds, or on the enrollment a person earned for it. It answers the join
// environment, the identity (zero when there is none yet), and whether the
// enrollment still has to be spent.
//
// Everything it refuses, it refuses BEFORE the join — an expired pre-auth key
// presented to the head comes back as an auth failure nobody can read, and a
// missing gateway comes back as a thirty-second timeout with no reason at all.
func door(store *state.Store, opts Options) (enroll.Env, state.Node, bool, error) {
	stored, err := store.LoadNode()
	switch {
	case err == nil:
		// The reconnect. EnvFromNode leaves the pre-auth key, the enrollment id
		// and the nonce empty on purpose: tsnet re-presents the node key it
		// persisted in the state directory, and this node has nothing left to
		// exchange.
		env, err := enroll.EnvFromNode(stored, enroll.EnrollmentOptions{StateDir: opts.StateDir})
		return env, stored, false, err

	case !state.IsNotEnrolled(err):
		return enroll.Env{}, state.Node{}, false, err
	}

	enrollment, err := store.LoadEnrollment()
	switch {
	case state.IsNoEnrollment(err):
		return enroll.Env{}, state.Node{}, false, exitcodes.Wrap(exitcodes.Usage, errors.New(
			"not enrolled — run `connector login --server` first, and choose the organization "+
				"this machine is a server for",
		))
	case err != nil:
		return enroll.Env{}, state.Node{}, false, err
	}

	if err := unexpired(enrollment); err != nil {
		return enroll.Env{}, state.Node{}, false, err
	}
	if err := gatewayPlaced(enrollment); err != nil {
		return enroll.Env{}, state.Node{}, false, err
	}

	env, err := enroll.EnvFromEnrollment(enrollment, enroll.EnrollmentOptions{
		Hostname: opts.Hostname, StateDir: opts.StateDir,
	})
	return env, state.Node{LastRevision: state.NoRevision}, true, err
}

// unexpired checks the pre-auth key's ten-minute fuse.
//
// An expires_at this connector cannot read is refused rather than assumed
// live: the mothership writes an ISO 8601 instant on every enrollment, so a
// value that is not one is material this binary does not understand, and
// joining on it would spend a nonce to reach the same refusal without the
// sentence that says what to do.
func unexpired(e state.Enrollment) error {
	expires, err := time.Parse(time.RFC3339, e.ExpiresAt)
	if err != nil {
		return exitcodes.Wrap(exitcodes.MaterialExpired, fmt.Errorf(
			"the stored enrollment expires at %q, which is not a time this connector can read — "+
				"run `connector login --server` again", e.ExpiresAt,
		))
	}

	if time.Now().After(expires) {
		return exitcodes.Wrap(exitcodes.MaterialExpired, fmt.Errorf(
			"this enrollment expired at %s — run `connector login --server` again",
			expires.Format(time.RFC3339),
		))
	}
	return nil
}

// gatewayPlaced checks that there is somewhere to exchange.
//
// The exchange reaches Gentility over the organization's gateway and pins its
// CA, so all three of address, port and CA are needed and any one of them alone
// is nothing. The mothership now refuses to mint an enrollment for an
// organization that has no placed gateway, so this is defensive — but it is the
// difference between a sentence naming what an admin has to do and a node that
// joins, finds nowhere to go, and times out without a reason.
func gatewayPlaced(e state.Enrollment) error {
	if e.GatewayAddr != "" && e.GatewayPort != 0 && e.GatewayCA != "" {
		return nil
	}
	return exitcodes.Wrap(exitcodes.Usage, errors.New(
		"no gateway is placed in this organization — an admin must place one, then run "+
			"`connector login --server` again",
	))
}
