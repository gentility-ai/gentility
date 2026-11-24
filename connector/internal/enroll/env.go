// Package enroll is how a node gets onto the org overlay and earns the two
// secrets it lives by.
//
// Two steps, and they are separate on purpose. **Join** presents a single-use
// pre-auth key to the head and comes up on the overlay with an address the
// head attributed. **Exchange** then reaches Gentility over that overlay and
// trades the nonce it was started with for its node credential and, for a case
// node, its runtime token.
//
// The order is the security story. The nonce alone proves
// nothing — what makes "a different node presented this nonce" a fact rather
// than a claim is that the exchange arrives from an address the gateway
// attributed and Gentility resolved. So the exchange carries **no bearer**: it
// has nothing to present but the nonce and the address it came from, and the
// address is the half it cannot forge.
package enroll

import (
	"fmt"
	"net/netip"
	"os"

	"github.com/gentility-ai/gentility/connector/internal/exitcodes"
	"github.com/gentility-ai/gentility/connector/internal/state"
	"github.com/gentility-ai/gentility/connector/internal/tsnode"
)

// Env is the join environment, as the Builder injects it into a sandbox.
//
// Every field but Ephemeral and GatewayInsecure is required: a node missing any
// one of them cannot join, and saying which one is missing is the difference
// between a recipe someone can fix and a process that exited 2.
type Env struct {
	// Origin is the MCP login origin which minted this enrollment.
	Origin string `json:"origin,omitempty"`
	// HeadURL is the head's control URL (tsnet ControlURL).
	HeadURL string
	// PreauthKey is single-use and expires in ten minutes.
	PreauthKey string
	// EnrollmentID and Nonce are the exchange's two halves. The nonce is
	// returned once at the mint and stored only as a sha256, so a node that
	// loses it cannot get it back.
	EnrollmentID string
	Nonce        string
	// GatewayAddr is where Gentility answers on the overlay: the gateway's
	// overlay address, or the listener itself over loopback during development.
	GatewayAddr netip.AddrPort
	// Ephemeral is true for a case node and false for a machine or a user:
	// the head forgets an ephemeral node when it goes.
	Ephemeral bool
	// StateDir is where the node's identity is written. Required, because a
	// node that guessed at it would come back from a restart as a stranger.
	StateDir string
	// Hostname is the tailnet hostname to join as. Gentility owns the node's
	// DNS name; this is what the head records for it.
	Hostname string
	// GatewayInsecure is the dev recipe's explicit opt-in to a plaintext hop
	// to the gateway. A pinned gateway CA replaces it. See Gateway.
	GatewayInsecure bool
	// GatewayCA is the internal CA's PEM, read from the file
	// OVERLAY_GATEWAY_CA names, for the one moment nothing else can cover:
	// the FIRST exchange. The CA a node pins arrives in its config document
	// (gateway.ca_pem), and the config document arrives through the gateway's
	// TLS door — so a fresh node with an empty state has nothing to verify
	// that first connection with, and the gateway serves nothing in
	// plaintext. The operator hands the CA over the same channel as the
	// enrollment trio; it is a public certificate, not a secret. Once the
	// first document is applied, the recorded ca_pem takes precedence and
	// this variable is only read, never required, again.
	GatewayCA string
	// Interface puts the overlay on a real network interface (tsnode.Options).
	// No variable sets it: it is `serve --interface`'s, and every other node
	// joins in userspace.
	Interface bool
}

// Variable names, in one place: they are a contract with the Builder and with
// the server's development tooling, and a name spelled twice is a name that
// can be spelled differently.
const (
	VarHeadURL         = "OVERLAY_HEAD_URL"
	VarPreauthKey      = "OVERLAY_PREAUTH_KEY"
	VarEnrollmentID    = "OVERLAY_ENROLLMENT_ID"
	VarNonce           = "OVERLAY_NONCE"
	VarGatewayAddr     = "OVERLAY_GATEWAY_ADDR"
	VarEphemeral       = "OVERLAY_EPHEMERAL"
	VarStateDir        = "OVERLAY_STATE_DIR"
	VarHostname        = "OVERLAY_HOSTNAME"
	VarGatewayInsecure = "OVERLAY_GATEWAY_INSECURE"
	VarGatewayCA       = "OVERLAY_GATEWAY_CA"
	// VarExpiresAt is install-specific and read only by `connector install`
	// (see internal/supervise), not part of the join Env every node parses — a
	// node presents its material and the head decides whether it is still
	// good. But it is a name in the same namespace, and this is the one home
	// for OVERLAY_* names, so it cannot be spelled two ways. It carries the
	// mint's own `expires_at` (RFC 3339) through to the staged enrollment;
	// unset, the install dates the material ten minutes out, which is the
	// window the mint gives it.
	VarExpiresAt = "OVERLAY_EXPIRES_AT"
	// VarProxyPort is proxy-specific and read only by `connector proxy` (see
	// cmd/connector/egress.go), not part of the shared join Env every node
	// parses — but its name lives here, the one home for OVERLAY_* names, so it
	// cannot be spelled two ways. Unset means the preflight binds an ephemeral
	// loopback port; set pins it, so the Builder can point HTTPS_PROXY at a port
	// it chose rather than one it has to read back.
	VarProxyPort = "OVERLAY_PROXY_PORT"
)

// Read reads the join environment from the process.
func Read() (Env, error) { return ReadEnv(os.LookupEnv) }

// ReadOrRejoin reads the join environment from the process, asking for the
// one-shot three only when the state directory holds no node.
func ReadOrRejoin() (Env, error) { return ReadEnvOrRejoin(os.LookupEnv) }

// ReadEnv reads the join environment through lookup, which is os.LookupEnv
// everywhere but the tests — a test that had to write the process environment
// would be a test every other test in the package raced with.
//
// An empty variable is a missing one: an exported-but-blank OVERLAY_NONCE is a
// recipe that went wrong, not a node with no nonce.
func ReadEnv(lookup func(string) (string, bool)) (Env, error) {
	return readHop(lookup, ReadJoin)
}

// ReadEnvOrRejoin is ReadEnv over ReadJoinOrRejoin: the gateway hop as
// ReadEnv reads it, and the pre-auth key, the enrollment id and the nonce only
// when there is no node.json to rejoin from.
//
// It is what a node that must come back from its own state reads — `proxy`
// and `egress`, which a supervisor restarts. With a node.json the three stay
// EMPTY even when they are set, and that emptiness is what arms tsnode.Up's
// rejoin refusal: a node the head has forgotten exits EnrollRefused instead of
// waiting for a login nobody will give. It also means a spent key need not be
// kept on disk for the next start.
func ReadEnvOrRejoin(lookup func(string) (string, bool)) (Env, error) {
	return readHop(lookup, ReadJoinOrRejoin)
}

// readHop reads the node through join and then the hop to the gateway, which
// every node but the gateway itself has in front of it.
func readHop(lookup func(string) (string, bool), join func(func(string) (string, bool)) (Env, error)) (Env, error) {
	env, err := join(lookup)
	if err != nil {
		return Env{}, err
	}

	raw, err := required(lookup, VarGatewayAddr)
	if err != nil {
		return Env{}, err
	}
	if env.GatewayAddr, err = netip.ParseAddrPort(raw); err != nil {
		return Env{}, usage(fmt.Errorf(
			"%s is %q, which is not an address and port — it wants the form [fd06:6e7:…]:443: %w",
			VarGatewayAddr, raw, err,
		))
	}

	// An opt-in, and false unless the recipe says otherwise: a hop that is
	// insecure by default is a deployment nobody chose.
	env.GatewayInsecure = flag(lookup, VarGatewayInsecure)

	// Optional, and a PATH rather than the PEM itself: a certificate is
	// multi-line and environments flatten newlines. Set-but-unreadable is a
	// recipe that went wrong, not a node with no CA — the operator wrote the
	// variable on purpose.
	if path, ok := lookup(VarGatewayCA); ok && path != "" {
		pem, err := os.ReadFile(path)
		if err != nil {
			return Env{}, usage(fmt.Errorf(
				"%s is %q, which this connector could not read: %w", VarGatewayCA, path, err,
			))
		}
		env.GatewayCA = string(pem)
	}

	return env, nil
}

// ReadJoin reads everything a node needs to come up on the overlay EXCEPT
// where Gentility answers.
//
// The gateway is the one node with no gateway in front of it. It reaches the
// app over the deploy's docker network by a container alias, not over the
// overlay by an address, and it names itself in the header it signs for
// everybody else — so OVERLAY_GATEWAY_ADDR and OVERLAY_GATEWAY_INSECURE are
// about a hop it does not have. They are ReadEnv's, which is every other
// node's door; the six variables here are what it means to be a node at all.
func ReadJoin(lookup func(string) (string, bool)) (Env, error) {
	env, err := readNode(lookup)
	if err != nil {
		return Env{}, err
	}
	if err := readEnrollment(lookup, &env); err != nil {
		return Env{}, err
	}
	return env, nil
}

// ReadJoinOrRejoin reads the same variables, but asks the state directory first
// whether the one-shot three are needed at all.
//
// It is the fourth door onto Env and the one written in environment variables:
// serve.door decides between a rejoin and a first join by loading the stored
// node, and this decides the same thing the same way. When there is a node, the
// pre-auth key, the enrollment id and the nonce stay EMPTY — the same
// deliberate emptiness EnvFromNode leaves, for the same two reasons. tsnet
// re-presents the node key it persisted in the state directory and ignores an
// auth key once it finds one (tsnode.Up), and the exchange already happened on
// material that lived an hour and was purged after it.
//
// Which is why requiring them on every start is a way to lose a node that was
// working. A container re-created without the spent secrets still in its shell
// would refuse to boot with its own identity sitting in the volume beside it —
// and for the gateway, a node that does not come up is every node behind it off
// the overlay.
//
// An unreadable node.json is neither door: it is returned as it comes, because
// a state directory this process cannot read is not a recipe to fix.
func ReadJoinOrRejoin(lookup func(string) (string, bool)) (Env, error) {
	env, err := readNode(lookup)
	if err != nil {
		return Env{}, err
	}

	switch _, err := (&state.Store{Dir: env.StateDir}).LoadNode(); {
	case err == nil:
		return env, nil
	case !state.IsNotEnrolled(err):
		return Env{}, err
	}

	if err := readEnrollment(lookup, &env); err != nil {
		return Env{}, err
	}
	return env, nil
}

// readNode reads what it takes to be a node at all: the head to present to,
// the directory the identity lives in, the name to come back under, and what
// kind of node this is. Every start needs all four, the first and the
// thousandth.
func readNode(lookup func(string) (string, bool)) (Env, error) {
	var env Env
	var err error

	if env.HeadURL, err = required(lookup, VarHeadURL); err != nil {
		return Env{}, err
	}
	if env.StateDir, err = required(lookup, VarStateDir); err != nil {
		return Env{}, err
	}
	if env.Hostname, err = required(lookup, VarHostname); err != nil {
		return Env{}, err
	}

	// An opt-in, false unless the recipe says otherwise: a node that is
	// ephemeral by default is a machine the head forgets.
	env.Ephemeral = flag(lookup, VarEphemeral)

	return env, nil
}

// readEnrollment reads the three variables a FIRST join is handed and nothing
// after it can use again: the pre-auth key that expires in ten minutes, and the
// enrollment id and nonce the exchange spends.
func readEnrollment(lookup func(string) (string, bool), env *Env) error {
	var err error

	if env.PreauthKey, err = required(lookup, VarPreauthKey); err != nil {
		return err
	}
	if env.EnrollmentID, err = required(lookup, VarEnrollmentID); err != nil {
		return err
	}
	if env.Nonce, err = required(lookup, VarNonce); err != nil {
		return err
	}
	return nil
}

// EnrollmentOptions are the two inputs an enrollment cannot carry, because
// they are about this machine rather than about the grant: where the node
// keeps its identity, and what the head should call it.
type EnrollmentOptions struct {
	// Hostname is the tailnet hostname to join as. Empty means this machine's
	// own, which is what an operator who did not say means.
	Hostname string
	// StateDir is where the node's identity is written. Required, for the same
	// reason ReadEnv requires OVERLAY_STATE_DIR: a node that guessed at it
	// would come back from a restart as a stranger. `serve` fills it from
	// --state-dir, which defaults to state.DefaultDir() — the default belongs
	// to the command, where the operator can see and override it, not here.
	StateDir string
}

// EnvFromEnrollment builds the join environment from an enrollment `connector
// login --server` was handed and wrote down.
//
// This is the SERVER's FIRST way in; the case node's is ReadEnv, which reads
// the same Env out of the OVERLAY_* variables the Builder injects, and the
// server's second start onwards is EnvFromNode. The three doors must stay in
// step: whatever Join and Exchange come to need, each has to fill, and this
// file holds them side by side so that a field added to one is a field visibly
// missing from the others.
//
// The differences from ReadEnv are the differences between the two kinds of
// node, and there are only three:
//
//   - Ephemeral is false. A server persists the identity it earns; the head
//     forgets an ephemeral node the moment it goes.
//   - GatewayInsecure is false, always. The plaintext hop is the dev recipe's
//     explicit opt-in and there is no way to ask for it here, because a real
//     server reaching its organization's gateway has the CA to pin.
//   - A missing gateway is not a refusal. When the organization has no placed
//     gateway the enrollment carries no address, and this returns the zero
//     AddrPort and no error: Join needs no gateway (only HeadURL, PreauthKey,
//     Hostname, StateDir and Ephemeral reach it), so `serve` gets to say "no
//     gateway is placed in this organization" before it joins, rather than
//     having this function refuse material that is otherwise sound (D5).
//
// Nothing else in Env is left unset. There is no proxy port here: OVERLAY_PROXY_PORT
// is `connector proxy`'s alone and never was part of the join Env.
func EnvFromEnrollment(e state.Enrollment, opts EnrollmentOptions) (Env, error) {
	env := Env{
		Origin:       e.Origin,
		HeadURL:      e.HeadURL,
		PreauthKey:   e.PreauthKey,
		EnrollmentID: e.EnrollmentID,
		Nonce:        e.Nonce,
		StateDir:     opts.StateDir,
		Hostname:     opts.Hostname,
		GatewayCA:    e.GatewayCA,
	}

	for _, field := range []struct {
		name  string
		value string
	}{
		{"head_url", env.HeadURL},
		{"preauth_key", env.PreauthKey},
		{"enrollment_id", env.EnrollmentID},
		{"nonce", env.Nonce},
	} {
		if field.value == "" {
			return Env{}, usage(fmt.Errorf(
				"the stored enrollment names no %s, and this node cannot join without it — "+
					"run `connector login --server` again", field.name,
			))
		}
	}

	if env.StateDir == "" {
		return Env{}, usage(fmt.Errorf(
			"this node was given no state directory, and it cannot keep the identity it earns without one",
		))
	}

	if env.Hostname == "" {
		hostname, err := os.Hostname()
		if err != nil {
			return Env{}, fmt.Errorf("enroll: this machine has no hostname to join as: %w", err)
		}
		env.Hostname = hostname
	}

	var err error
	if env.GatewayAddr, err = gatewayAddrPort("the stored enrollment", e.GatewayAddr, e.GatewayPort); err != nil {
		return Env{}, err
	}

	return env, nil
}

// EnvFromNode builds the RECONNECT environment: what a server that already
// enrolled comes back up with after a reboot.
//
// The third door onto Env, and here beside the other two for the reason that
// file comment gives: whatever Join and Exchange come to need, every door has
// to fill, and a field added to one is a field visibly missing from the others.
//
// The difference is the field that is deliberately EMPTY. The pre-auth key was
// single-use and expired ten minutes after it was minted, so a server that has
// been up for a week has nothing left to present — and needs nothing: tsnet
// re-presents the node key it persisted in the state directory, and ignores an
// auth key once it finds one (see tsnode.Up). EnrollmentID and Nonce are empty
// for the same reason and one more: that exchange already happened, and its
// material was purged an hour later. A reconnect joins; it does not exchange.
//
// Everything else comes from the identity on disk rather than from the
// operator. The hostname especially: it is the name the head knows this node
// by, and a server that came back under a different one would come back as a
// different node — so EnrollmentOptions.Hostname is not consulted here, unlike
// in EnvFromEnrollment where there is no node yet to have a name.
func EnvFromNode(n state.Node, opts EnrollmentOptions) (Env, error) {
	for _, field := range []struct {
		name  string
		value string
	}{
		{"head_url", n.HeadURL},
		{"hostname", n.Hostname},
	} {
		if field.value == "" {
			return Env{}, usage(fmt.Errorf(
				"this node's stored identity names no %s, so there is nothing to rejoin the head "+
					"with — it was written before the connector recorded one, and this node has to "+
					"enrol again: run `connector login --server`", field.name,
			))
		}
	}

	if opts.StateDir == "" {
		return Env{}, usage(fmt.Errorf(
			"this node was given no state directory, and the identity it comes back on lives in one",
		))
	}

	env := Env{
		Origin:    n.Origin,
		HeadURL:   n.HeadURL,
		Hostname:  n.Hostname,
		StateDir:  opts.StateDir,
		GatewayCA: n.CAPEM,
	}

	var err error
	if env.GatewayAddr, err = gatewayAddrPort("this node's stored identity", n.GatewayAddr, n.GatewayPort); err != nil {
		return Env{}, err
	}

	return env, nil
}

// gatewayAddrPort composes where Gentility answers out of the address and port
// stored beside each other.
//
// Both halves or neither: a port with no address is as gateway-less as nothing
// at all, and inventing one from half the material is how a node ends up
// dialling somewhere nobody placed. Never by string concatenation — an overlay
// address is IPv6, and "fd06:6e7:…:443" is not an address and port, it is a typo
// the parser will happily accept as something else.
func gatewayAddrPort(source, address string, port int) (netip.AddrPort, error) {
	if address == "" || port == 0 {
		return netip.AddrPort{}, nil
	}

	addr, err := netip.ParseAddr(address)
	if err != nil {
		return netip.AddrPort{}, usage(fmt.Errorf(
			"%s's gateway_addr is %q, which is not an address — it wants the form fd06:6e7:…: %w",
			source, address, err,
		))
	}
	if port < 0 || port > 65535 {
		return netip.AddrPort{}, usage(fmt.Errorf(
			"%s's gateway_port is %d, which is not a port", source, port,
		))
	}

	return netip.AddrPortFrom(fromLegacyULA(addr), uint16(port)), nil
}

// legacyULA is the range 6net used before 2026-10-10, which it shared with
// Tailscale. A node.json written then names its gateway inside it, and the
// config document that would correct it can only arrive through the
// gateway, so a stored address there is read as the same interface under
// the current range.
//
// Both stored doors get it. That includes an OVERLAY_GATEWAY_ADDR given to
// `install` before the move: install saves it into enrollment.json, and the
// next start reads it back through EnvFromEnrollment, shimmed. Only the
// environment read directly by ReadEnv is taken as given.
//
// Remove it when the "legacy gateway shim" check in the gendesk-6 design's
// debts passes: every node not revoked and enrolled before the cutover has an
// applied revision (`gentility status --json`'s "revision") at or past its
// cutover render, the first node_config_revisions row the server wrote for it
// after the range migration. Applying that document rewrote its node.json
// gateway_addr into fd06:6e7::/48 (state.Store.RecordGateway).
var legacyULA = netip.MustParsePrefix("fd7a:115c:a1e0::/48")

func fromLegacyULA(a netip.Addr) netip.Addr {
	if !legacyULA.Contains(a) {
		return a
	}
	b, base := a.As16(), tsnode.OverlayRange().Addr().As16()
	copy(b[:6], base[:6])
	return netip.AddrFrom16(b)
}

func required(lookup func(string) (string, bool), name string) (string, error) {
	value, _ := lookup(name)
	if value == "" {
		return "", usage(fmt.Errorf("%s is not set, and this node cannot join without it", name))
	}
	return value, nil
}

// flag reads an opt-in. "1" and nothing else: a variable set to "false" or
// "no" meaning true is the classic way an operator turns something on by
// trying to turn it off.
func flag(lookup func(string) (string, bool), name string) bool {
	value, _ := lookup(name)
	return value == "1"
}

// usage marks a refusal the operator has to fix. It will be just as wrong on
// the next start, so a supervisor must not restart into it.
func usage(err error) error { return exitcodes.Wrap(exitcodes.Usage, err) }
