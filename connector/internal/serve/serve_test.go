package serve

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/gentility-ai/gentility/connector/internal/config"
	"github.com/gentility-ai/gentility/connector/internal/enroll"
	"github.com/gentility-ai/gentility/connector/internal/exitcodes"
	"github.com/gentility-ai/gentility/connector/internal/report"
	"github.com/gentility-ai/gentility/connector/internal/state"
)

// A server that has already enrolled comes back on the identity it wrote down,
// and MUST NOT exchange again: the pre-auth key it joined on was single-use and
// the enrollment material was purged an hour later, so a second exchange is a
// 410 that would kill a node which was working perfectly. What brings it back
// is the node key tsnet persisted beside node.json.
func TestRunComesBackOnTheStoredIdentityWithoutExchanging(t *testing.T) {
	dir := t.TempDir()
	store := &state.Store{Dir: dir}
	if err := store.SaveNode(state.Node{
		NodeID: "node-1", NodeCredential: "cred", LastRevision: state.NoRevision,
		HeadURL: "https://head.example", Hostname: "relay-1",
	}); err != nil {
		t.Fatal(err)
	}

	spy := &spy{}
	err := Run(context.Background(), Options{StateDir: dir, Hostname: "ignored"}, spy.deps())

	if err != nil {
		t.Fatalf("Run = %v, want nil", err)
	}
	if spy.exchanges != 0 {
		t.Fatalf("exchanged %d time(s) on a reconnect, want 0", spy.exchanges)
	}
	if spy.joined.HeadURL != "https://head.example" || spy.joined.Hostname != "relay-1" {
		t.Fatalf("joined as %+v, want the stored identity's head and hostname", spy.joined)
	}
	// The name the head knows this node by, not the one the flag defaults to: a
	// server that came back under a different hostname would come back as a
	// different node.
	if spy.joined.PreauthKey != "" {
		t.Fatalf("a reconnect presented a pre-auth key: %+v", spy.joined)
	}
	if spy.attended.Identity.NodeID != "node-1" {
		t.Fatalf("attended as %v, want node-1", spy.attended.Identity.NodeID)
	}
}

// The first start is the other half: no identity yet, so the enrollment
// `connector login --server` wrote down is spent on one — and cleared the
// moment it has been, because node.json then holds everything a rejoin needs
// and what is left is an hour-lived bearer of two secrets.
func TestRunSpendsTheEnrollmentOnceAndThenForgetsIt(t *testing.T) {
	dir := t.TempDir()
	store := &state.Store{Dir: dir}
	if err := store.SaveEnrollment(enrollment()); err != nil {
		t.Fatal(err)
	}

	spy := &spy{}
	err := Run(context.Background(), Options{StateDir: dir, Hostname: "relay-1"}, spy.deps())

	if err != nil {
		t.Fatalf("Run = %v, want nil", err)
	}
	if spy.exchanges != 1 {
		t.Fatalf("exchanged %d time(s) on a first start, want 1", spy.exchanges)
	}
	if spy.joined.PreauthKey != "preauth-shhh" || spy.joined.Nonce != "nonce-shhh" {
		t.Fatalf("joined with %+v, want the enrollment's material", spy.joined)
	}
	if _, err := store.LoadEnrollment(); !state.IsNoEnrollment(err) {
		t.Fatalf("LoadEnrollment after the exchange = %v, want it cleared", err)
	}
	if _, err := store.LoadNode(); err != nil {
		t.Fatalf("LoadNode after the exchange = %v, want the identity", err)
	}
}

// A machine nobody has enrolled has not failed at anything; it has been asked
// to do the second step first. It is a refusal and not a retry: the supervisor
// must not start it again, because it will be just as unenrolled next time.
func TestRunRefusesAMachineThatWasNeverEnrolled(t *testing.T) {
	spy := &spy{}
	err := Run(context.Background(), Options{StateDir: t.TempDir()}, spy.deps())

	assertFatal(t, err, exitcodes.Usage, "not enrolled")
	if spy.joins != 0 {
		t.Fatalf("joined %d time(s) with no material, want 0", spy.joins)
	}
}

// The pre-auth key lives ten minutes. A `login --server` that was
// never followed by an `install` leaves one behind that cannot buy anything,
// and the head's answer to it is an auth failure nobody can read — so the fuse
// is checked here, where the connector can say what actually happened and what
// to type.
func TestRunRefusesAnExpiredEnrollmentBeforeItJoins(t *testing.T) {
	dir := t.TempDir()
	store := &state.Store{Dir: dir}

	expired := enrollment()
	expired.ExpiresAt = "2020-01-01T00:00:00Z"
	if err := store.SaveEnrollment(expired); err != nil {
		t.Fatal(err)
	}

	spy := &spy{}
	err := Run(context.Background(), Options{StateDir: dir}, spy.deps())

	assertFatal(t, err, exitcodes.MaterialExpired, "2020-01-01T00:00:00Z")
	if spy.joins != 0 {
		t.Fatalf("joined %d time(s) on expired material, want 0", spy.joins)
	}
}

// Without a gateway there is nowhere to exchange: the node would come up on the
// overlay, find no address to reach Gentility at, and time out. The mothership
// now refuses to mint an enrollment for an organization with no placed gateway,
// so this is the belt to that braces — and it names the thing an admin has to
// do rather than the connection that failed.
func TestRunRefusesAnEnrollmentWithNoGatewayToExchangeAt(t *testing.T) {
	dir := t.TempDir()
	store := &state.Store{Dir: dir}

	gatewayless := enrollment()
	gatewayless.GatewayAddr, gatewayless.GatewayPort, gatewayless.GatewayCA = "", 0, ""
	if err := store.SaveEnrollment(gatewayless); err != nil {
		t.Fatal(err)
	}

	spy := &spy{}
	err := Run(context.Background(), Options{StateDir: dir}, spy.deps())

	assertFatal(t, err, exitcodes.Usage, "no gateway is placed")
	if spy.joins != 0 {
		t.Fatalf("joined %d time(s) with no gateway, want 0", spy.joins)
	}
}

// A binary below the mothership's floor is one only a new binary fixes, so it
// exits saying exactly that and the supervisor is expected not to start it
// again. There is no fail-static mode: the config loop is what carries
// revocations, and a node that kept serving while its lifeline was refused
// would be a node nobody could revoke.
func TestRunExitsFatalWhenTheMothershipRefusesThisCapver(t *testing.T) {
	dir := t.TempDir()
	store := &state.Store{Dir: dir}
	if err := store.SaveNode(state.Node{
		NodeID: "node-1", NodeCredential: "cred", LastRevision: state.NoRevision,
		HeadURL: "https://head.example", Hostname: "relay-1",
	}); err != nil {
		t.Fatal(err)
	}

	spy := &spy{attend: exitcodes.Wrap(
		exitcodes.CapverTooOld, &config.CapverTooOldError{Declared: 1, Floor: 2},
	)}

	err := Run(context.Background(), Options{StateDir: dir}, spy.deps())

	assertFatal(t, err, exitcodes.CapverTooOld, "upgrade it")
}

// --interface is a property of this start and not of the identity: both doors
// carry it to the join, and a start without it joins in userspace.
func TestRunCarriesTheInterfaceModeToTheJoin(t *testing.T) {
	for _, mode := range []bool{true, false} {
		dir := t.TempDir()
		store := &state.Store{Dir: dir}
		if err := store.SaveEnrollment(enrollment()); err != nil {
			t.Fatal(err)
		}

		first := &spy{}
		opts := Options{StateDir: dir, Hostname: "relay-1", Interface: mode}
		if err := Run(context.Background(), opts, first.deps()); err != nil {
			t.Fatalf("Run (first start, interface %v) = %v, want nil", mode, err)
		}
		if first.joined.Interface != mode {
			t.Fatalf("first start joined with Interface %v, want %v", first.joined.Interface, mode)
		}

		// The same directory again is the reconnect door.
		again := &spy{}
		if err := Run(context.Background(), opts, again.deps()); err != nil {
			t.Fatalf("Run (reconnect, interface %v) = %v, want nil", mode, err)
		}
		if again.exchanges != 0 || again.joined.Interface != mode {
			t.Fatalf("reconnect joined with Interface %v (%d exchanges), want %v and none",
				again.joined.Interface, again.exchanges, mode)
		}
	}
}

// A person's machine runs no egress relay, and the mothership refuses `egress`
// from a user node, all-or-nothing: offering it got every heartbeat answered
// 422. So in interface mode the heartbeat carries no services key at all, and a
// server goes on offering its relay on the port it was given.
func TestOffersNothingFromAPersonsMachineAndTheRelayFromAServer(t *testing.T) {
	person, err := json.Marshal(report.Status{Services: offers(Options{Interface: true, EgressPort: DefaultEgressPort})})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(person), `"services"`) {
		t.Fatalf("interface-mode heartbeat = %s, want no services key", person)
	}

	server := offers(Options{EgressPort: 9443})
	if len(server) != 1 || server[0] != (report.Service{Kind: "egress", Port: 9443}) {
		t.Fatalf("userspace offers = %+v, want egress on 9443", server)
	}
}

// spy is the network, remembered rather than performed: what Run decides is
// which door it takes and what it spends, and neither needs an overlay.
type spy struct {
	joins     int
	joined    enroll.Env
	exchanges int
	attended  Attendance
	attend    error
}

func (s *spy) deps() Deps {
	return Deps{
		Join: func(_ context.Context, env enroll.Env) (Session, error) {
			s.joins++
			s.joined = env
			return Session{}, nil
		},
		Exchange: func(_ context.Context, env enroll.Env, store *state.Store, _ Session) (state.Node, error) {
			s.exchanges++
			node := state.Node{
				NodeID: "node-1", NodeCredential: "cred", LastRevision: state.NoRevision,
				HeadURL: env.HeadURL, Hostname: env.Hostname,
			}
			return node, store.SaveNode(node)
		},
		Attend: func(_ context.Context, _ Options, a Attendance) error {
			s.attended = a
			return s.attend
		},
	}
}

func enrollment() state.Enrollment {
	return state.Enrollment{
		EnrollmentID: "enr-1",
		PreauthKey:   "preauth-shhh",
		Nonce:        "nonce-shhh",
		HeadURL:      "https://head.example",
		ExpiresAt:    time.Now().Add(10 * time.Minute).UTC().Format(time.RFC3339),
		GatewayAddr:  "fd06:6e7:0:1::1",
		GatewayPort:  443,
		GatewayCA:    "-----BEGIN CERTIFICATE-----\nnot a real one\n-----END CERTIFICATE-----\n",
	}
}

func assertFatal(t *testing.T, err error, code int, says string) {
	t.Helper()

	if err == nil {
		t.Fatalf("Run = nil, want a refusal exiting %d", code)
	}
	got, fatal := exitcodes.Of(err)
	if !fatal || got != code {
		t.Fatalf("Run = %v (exit %d, fatal %v), want exit %d", err, got, fatal, code)
	}
	if !strings.Contains(err.Error(), says) {
		t.Fatalf("Run = %q, want it to say %q", err, says)
	}
}
