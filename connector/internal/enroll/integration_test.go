//go:build overlay_integration

// A real join, against a real head and a real Gentility: the three things
// the enrollment flow must support end to end.
//
// It is behind a build tag because it needs both of those running and takes
// about a minute; `go test ./...` never sees it. To run it, from a shell with
// this worktree's direnv active:
//
//	mix phx.server                       # in another terminal, if it is not up
//	cd node && go test -tags overlay_integration ./internal/enroll/ -run TestLive -v
//
// Everything else it needs, bin/overlay-dev does: it starts this worktree's
// head, checks the app is answering and that the ingress will believe a
// loopback peer, stands up a dev gateway in the organization if there is none,
// and mints one enrollment per call. Its refusals say what to fix.
//
// Each test drives the connector's own packages rather than the binary,
// because what is being proved is the CONTRACT — which status means what,
// which secrets come back, what the ledger says afterwards — and a subprocess
// would only let this assert on log lines.
package enroll

import (
	"bufio"
	"context"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gentility-ai/gentility/connector/internal/config"
	"github.com/gentility-ai/gentility/connector/internal/exitcodes"
	"github.com/gentility-ai/gentility/connector/internal/report"
	"github.com/gentility-ai/gentility/connector/internal/state"
	"github.com/gentility-ai/gentility/connector/internal/tsnode"
)

// documentWait is how long the renderer is given. The document is written by
// an Oban job the head's registration queued, so it is usually there before
// the exchange returns; this is the margin, not the expectation.
const documentWait = 90 * time.Second

// TestLiveJoin is the whole arc: onto the overlay with a pre-auth key, the
// nonce traded for the two secrets, the config document pulled, one heartbeat
// beaten — and the enrollment `exchanged` at the end of it.
func TestLiveJoin(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	env := mint(t)
	node, client, base := live(t, ctx, env)
	store := &state.Store{Dir: env.StateDir}

	stored, err := Exchange(ctx, client, base, env, store)
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	if stored.NodeID == "" || stored.NodeCredential == "" {
		t.Fatalf("the exchange handed over %v", stored)
	}
	// This enrollment names a case runtime, so it receives a runtime token.
	if stored.RuntimeToken == "" {
		t.Fatal("a case runtime's exchange carried no runtime token")
	}
	// The exchange never moves the cursor, even when a document came back with
	// the material: one path applies a document and it is the pull loop's.
	if stored.LastRevision != state.NoRevision {
		t.Fatalf("LastRevision = %d after an exchange, want NoRevision", stored.LastRevision)
	}

	configs := config.New(base, stored, store, client)
	document := pull(t, ctx, configs)

	if document.NodeID != stored.NodeID {
		t.Fatalf("the document is for node %s, this node is %s", document.NodeID, stored.NodeID)
	}
	if document.RendererVersion != config.RendererVersion {
		t.Fatalf("renderer_version = %d", document.RendererVersion)
	}
	if len(document.Grants) == 0 {
		t.Fatal("the document names no address for a node that just joined")
	}
	if document.Gateway.Address == "" {
		t.Fatal("the document names no gateway")
	}

	grants, err := tsnode.ParseGrants(document.Grants)
	if err != nil {
		t.Fatalf("the document's grants are not addresses this node can dial from: %v", err)
	}
	node.SetGrants(grants)

	beat := &report.Reporter{
		BaseURL:    base,
		Credential: stored.NodeCredential,
		HTTP:       client,
		Status:     status(t, node),
	}
	if err := beat.Beat(ctx); err != nil {
		t.Fatalf("presence: %v", err)
	}

	if got := enrollmentState(t, env.EnrollmentID); got != "exchanged" {
		t.Fatalf("the enrollment is %s, want exchanged", got)
	}
}

// The mothership is idempotent on (enrollment_id, node_id, nonce) for an hour,
// which is what makes "no retry loop around the exchange" safe: a supervisor
// restarting a connector presents the same nonce and must get the same node
// back, not a second identity or a refusal.
func TestLiveExchangeRetryReturnsTheSameMaterial(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	env := mint(t)
	_, client, base := live(t, ctx, env)

	first, err := Exchange(ctx, client, base, env, &state.Store{Dir: env.StateDir})
	if err != nil {
		t.Fatalf("the first exchange: %v", err)
	}

	// A second state directory, because a connector that had kept the first
	// answer would never ask again — this is the restart that lost it.
	again := env
	again.StateDir = filepath.Join(t.TempDir(), "gentility")

	second, err := Exchange(ctx, client, base, again, &state.Store{Dir: again.StateDir})
	if err != nil {
		t.Fatalf("the second exchange: %v", err)
	}

	if second.NodeID != first.NodeID {
		t.Fatalf("two exchanges, two nodes: %s then %s", first.NodeID, second.NodeID)
	}
	if second.NodeCredential != first.NodeCredential {
		t.Fatal("the second exchange minted a second credential")
	}
	if second.RuntimeToken != first.RuntimeToken {
		t.Fatal("the second exchange minted a second runtime token")
	}
}

// The security story of the whole design: what makes "a
// different node presented this nonce" a FACT rather than a claim is that the
// exchange arrives from an address the gateway attributed and Gentility
// resolved. A node holding a nonce it stole gets nothing, and the enrollment
// it stole from is compromised — which is a decision a human looks at, not
// something the thief can retry past.
func TestLiveANodeThatPresentsAnotherEnrollmentsNonceIsRefused(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	// The victim joins — its enrollment is bound to its node — and stops
	// there, holding a nonce it has not spent.
	victim := mint(t)
	live(t, ctx, victim)

	if got := enrollmentState(t, victim.EnrollmentID); got != "registered" {
		t.Fatalf("the victim's enrollment is %s after joining, want registered", got)
	}

	// The thief is a node of its own, on its own pre-auth key, presenting the
	// victim's enrollment and nonce.
	thief := mint(t)
	_, client, base := live(t, ctx, thief)

	stolen := thief
	stolen.EnrollmentID = victim.EnrollmentID
	stolen.Nonce = victim.Nonce

	_, err := Exchange(ctx, client, base, stolen, &state.Store{Dir: stolen.StateDir})
	if err == nil {
		t.Fatal("a stolen nonce was exchanged")
	}
	if code, fatal := exitcodes.Of(err); !fatal || code != exitcodes.EnrollRefused {
		t.Fatalf("exit code = (%d, %v), want a fatal EnrollRefused; err = %v", code, fatal, err)
	}

	if got := enrollmentState(t, victim.EnrollmentID); got != "compromised" {
		t.Fatalf("the victim's enrollment is %s, want compromised", got)
	}
}

// live brings a node up on the overlay and hands back the client every call to
// Gentility is made with. Closing it is this test's cleanup.
func live(t *testing.T, ctx context.Context, env Env) (*tsnode.Node, *http.Client, string) {
	t.Helper()

	if !Direct(env.GatewayAddr) {
		t.Fatalf("%s is not the loopback listener this test drives", env.GatewayAddr)
	}

	node, addresses, err := Join(ctx, env, nil)
	if err != nil {
		t.Fatalf("Join: %v", err)
	}
	t.Cleanup(node.Close)

	if len(addresses) == 0 {
		t.Fatal("the head attributed no overlay address")
	}

	peer, err := SelfPeer(addresses)
	if err != nil {
		t.Fatal(err)
	}

	base, tlsConfig, err := Gateway(env.GatewayAddr, "", env.GatewayInsecure)
	if err != nil {
		t.Fatal(err)
	}

	return node, Client(nil, tlsConfig, peer), base
}

// pull asks for the config document until one arrives. `not_rendered` is the
// normal first answer — the render is a job the registration queued — and the
// long poll is what usually makes this one call.
func pull(t *testing.T, ctx context.Context, configs *config.Client) *config.Document {
	t.Helper()

	deadline := time.Now().Add(documentWait)
	for {
		document, err := configs.Pull(ctx)
		if err == nil {
			return document
		}
		if _, fatal := exitcodes.Of(err); fatal {
			t.Fatalf("the config pull was refused: %v", err)
		}
		if time.Now().After(deadline) {
			t.Fatalf("no config document in %s: %v", documentWait, err)
		}
		time.Sleep(time.Second)
	}
}

func status(t *testing.T, node *tsnode.Node) func(context.Context) (report.Status, error) {
	t.Helper()

	return func(ctx context.Context) (report.Status, error) {
		seen, err := node.Status(ctx)
		if err != nil {
			return report.Status{}, err
		}

		addresses := make([]string, 0, len(seen.Addresses))
		for _, address := range seen.Addresses {
			addresses = append(addresses, address.String())
		}

		return report.Status{
			Endpoint:  seen.Endpoint,
			Relayed:   seen.Relayed,
			Addresses: addresses,
		}, nil
	}
}

// mint runs bin/overlay-dev and reads the join environment it wrote, pointed
// at a state directory of this test's own so two nodes in one test are two
// nodes.
func mint(t *testing.T) Env {
	t.Helper()

	root := repoRoot(t)
	command := exec.Command(filepath.Join(root, "bin", "overlay-dev"))
	command.Dir = root

	if out, err := command.CombinedOutput(); err != nil {
		t.Fatalf("bin/overlay-dev: %v\n%s", err, out)
	}

	// Read rather than parsed off stdout: the file is the contract
	// bin/overlay-dev writes and the connector reads, and the two secrets in
	// it are never printed.
	vars := readEnvFile(t, filepath.Join(root, "tmp", "overlay-dev", "join.env"))

	env, err := ReadEnv(func(name string) (string, bool) {
		value, ok := vars[name]
		return value, ok
	})
	if err != nil {
		t.Fatalf("the join environment bin/overlay-dev wrote: %v", err)
	}

	env.StateDir = filepath.Join(t.TempDir(), "gentility")

	return env
}

// enrollmentState asks the database what the enrollment ended up as. The wire
// only ever shows a node one half of an append.
func enrollmentState(t *testing.T, id string) string {
	t.Helper()

	root := repoRoot(t)
	command := exec.Command(filepath.Join(root, "bin", "overlay-dev"), "--state", id)
	command.Dir = root

	out, err := command.Output()
	if err != nil {
		t.Fatalf("bin/overlay-dev --state %s: %v", id, err)
	}

	return strings.TrimSpace(string(out))
}

func readEnvFile(t *testing.T, path string) map[string]string {
	t.Helper()

	file, err := os.Open(path)
	if err != nil {
		t.Fatalf("the join environment: %v", err)
	}
	defer file.Close()

	vars := map[string]string{}
	lines := bufio.NewScanner(file)
	for lines.Scan() {
		name, value, found := strings.Cut(lines.Text(), "=")
		if found {
			vars[name] = value
		}
	}
	if err := lines.Err(); err != nil {
		t.Fatal(err)
	}

	return vars
}

// repoRoot is three directories up from internal/enroll, which is where the
// test binary runs.
func repoRoot(t *testing.T) string {
	t.Helper()

	root, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "bin", "overlay-dev")); err != nil {
		t.Fatalf("bin/overlay-dev is not at %s: %v", root, err)
	}

	return root
}
