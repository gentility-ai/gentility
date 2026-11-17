package enroll

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gentility-ai/gentility/connector/internal/exitcodes"
	"github.com/gentility-ai/gentility/connector/internal/state"
	"github.com/gentility-ai/gentility/connector/internal/tsnode"
)

func environment() map[string]string {
	return map[string]string{
		"OVERLAY_HEAD_URL":      "https://head.example",
		"OVERLAY_PREAUTH_KEY":   "tskey-auth-abc",
		"OVERLAY_ENROLLMENT_ID": "0198f0a2-0000-7000-8000-00000000000e",
		"OVERLAY_NONCE":         "nonce-shhh",
		"OVERLAY_GATEWAY_ADDR":  "[fd06:6e7:0:1::1]:443",
		"OVERLAY_STATE_DIR":     "/var/lib/gentility",
		"OVERLAY_HOSTNAME":      "case-17",
	}
}

func lookupIn(vars map[string]string) func(string) (string, bool) {
	return func(name string) (string, bool) {
		value, ok := vars[name]
		return value, ok
	}
}

func TestEnvRequiresEveryVariable(t *testing.T) {
	for name := range environment() {
		vars := environment()
		delete(vars, name)

		_, err := ReadEnv(lookupIn(vars))
		if err == nil {
			t.Fatalf("%s missing was accepted", name)
		}
		if !strings.Contains(err.Error(), name) {
			t.Fatalf("the refusal for a missing %s does not name it: %v", name, err)
		}
		if code, fatal := exitcodes.Of(err); !fatal || code != exitcodes.Usage {
			t.Fatalf("%s missing exits (%d, %v), want (%d, true)", name, code, fatal, exitcodes.Usage)
		}
	}
}

// An empty variable is a variable that was never set: an exported-but-blank
// OVERLAY_NONCE is a recipe that went wrong, not a node with no nonce.
func TestEnvTreatsAnEmptyVariableAsMissing(t *testing.T) {
	vars := environment()
	vars["OVERLAY_NONCE"] = ""

	if _, err := ReadEnv(lookupIn(vars)); err == nil {
		t.Fatal("an empty OVERLAY_NONCE was accepted")
	}
}

func TestEnvEphemeralDefaultsFalse(t *testing.T) {
	env, err := ReadEnv(lookupIn(environment()))
	if err != nil {
		t.Fatalf("ReadEnv: %v", err)
	}
	if env.Ephemeral {
		t.Fatal("Ephemeral defaulted to true; only a case node is ephemeral, and it says so")
	}

	vars := environment()
	vars["OVERLAY_EPHEMERAL"] = "1"
	env, err = ReadEnv(lookupIn(vars))
	if err != nil {
		t.Fatalf("ReadEnv: %v", err)
	}
	if !env.Ephemeral {
		t.Fatal("OVERLAY_EPHEMERAL=1 did not make the node ephemeral")
	}
}

// The CA for the first exchange arrives as a file the recipe wrote, because
// the config document that normally carries it is on the far side of the TLS
// door it verifies. Optional; set-but-unreadable is a broken recipe.
func TestEnvReadsTheGatewayCAFromAFile(t *testing.T) {
	env, err := ReadEnv(lookupIn(environment()))
	if err != nil {
		t.Fatalf("ReadEnv: %v", err)
	}
	if env.GatewayCA != "" {
		t.Fatalf("GatewayCA = %q with OVERLAY_GATEWAY_CA unset, want empty", env.GatewayCA)
	}

	path := filepath.Join(t.TempDir(), "overlay-ca.pem")
	if err := os.WriteFile(path, []byte("-----BEGIN CERTIFICATE-----"), 0o600); err != nil {
		t.Fatal(err)
	}
	vars := environment()
	vars["OVERLAY_GATEWAY_CA"] = path

	env, err = ReadEnv(lookupIn(vars))
	if err != nil {
		t.Fatalf("ReadEnv: %v", err)
	}
	if env.GatewayCA != "-----BEGIN CERTIFICATE-----" {
		t.Fatalf("GatewayCA = %q, want the file's contents", env.GatewayCA)
	}

	vars["OVERLAY_GATEWAY_CA"] = filepath.Join(t.TempDir(), "nowhere.pem")
	_, err = ReadEnv(lookupIn(vars))
	if err == nil {
		t.Fatal("an unreadable OVERLAY_GATEWAY_CA was accepted")
	}
	if !strings.Contains(err.Error(), "OVERLAY_GATEWAY_CA") {
		t.Fatalf("the refusal does not name the variable: %v", err)
	}
	if code, fatal := exitcodes.Of(err); !fatal || code != exitcodes.Usage {
		t.Fatalf("an unreadable CA exits (%d, %v), want (%d, true)", code, fatal, exitcodes.Usage)
	}
}

func TestEnvRefusesAGatewayAddressThatIsNotAnAddressAndPort(t *testing.T) {
	vars := environment()
	vars["OVERLAY_GATEWAY_ADDR"] = "fd06:6e7:0:1::1"

	_, err := ReadEnv(lookupIn(vars))
	if err == nil {
		t.Fatal("a gateway address with no port was accepted")
	}
	if !strings.Contains(err.Error(), "OVERLAY_GATEWAY_ADDR") {
		t.Fatalf("the refusal does not name the variable: %v", err)
	}
}

// The server's door onto the same Env the case node reads from OVERLAY_*.
func enrollment() state.Enrollment {
	return state.Enrollment{
		EnrollmentID: "0198f0a2-0000-7000-8000-00000000000e",
		PreauthKey:   "tskey-auth-abc",
		Nonce:        "nonce-shhh",
		HeadURL:      "https://head.example",
		ExpiresAt:    "2026-09-06T12:00:00Z",
		GatewayAddr:  "fd06:6e7:0:1::1",
		GatewayPort:  443,
		GatewayCA:    "-----BEGIN CERTIFICATE-----",
	}
}

func TestEnvFromEnrollmentCarriesTheWholeShape(t *testing.T) {
	env, err := EnvFromEnrollment(enrollment(), EnrollmentOptions{
		Hostname: "shipyard", StateDir: "/var/lib/gentility",
	})
	if err != nil {
		t.Fatalf("EnvFromEnrollment: %v", err)
	}

	want := Env{
		HeadURL:      "https://head.example",
		PreauthKey:   "tskey-auth-abc",
		EnrollmentID: "0198f0a2-0000-7000-8000-00000000000e",
		Nonce:        "nonce-shhh",
		GatewayAddr:  netip.MustParseAddrPort("[fd06:6e7:0:1::1]:443"),
		StateDir:     "/var/lib/gentility",
		Hostname:     "shipyard",
		GatewayCA:    "-----BEGIN CERTIFICATE-----",
	}
	if env != want {
		t.Fatalf("Env = %+v, want %+v", env, want)
	}
	// Said again, because these two are the whole difference between a server
	// and a case node and a struct comparison states them only in passing.
	if env.Ephemeral {
		t.Fatal("Ephemeral is true; a server persists the identity it earns")
	}
	if env.GatewayInsecure {
		t.Fatal("GatewayInsecure is true; a real server never speaks plaintext to the gateway")
	}
}

// No gateway placed in the organization is not this function's refusal to make
// (D5): it hands back the zero address and lets `serve` say so.
func TestEnvFromEnrollmentAcceptsAGatewaylessEnrollment(t *testing.T) {
	e := enrollment()
	e.GatewayAddr, e.GatewayPort, e.GatewayCA = "", 0, ""

	env, err := EnvFromEnrollment(e, EnrollmentOptions{Hostname: "shipyard", StateDir: "/var/lib/gentility"})
	if err != nil {
		t.Fatalf("EnvFromEnrollment: %v", err)
	}
	if env.GatewayAddr.IsValid() {
		t.Fatalf("GatewayAddr = %v, want the zero value", env.GatewayAddr)
	}
	if env.GatewayCA != "" {
		t.Fatalf("GatewayCA = %q, want empty", env.GatewayCA)
	}
}

func TestEnvFromEnrollmentNamesTheMaterialItIsMissing(t *testing.T) {
	for name, blank := range map[string]func(*state.Enrollment){
		"head_url":      func(e *state.Enrollment) { e.HeadURL = "" },
		"preauth_key":   func(e *state.Enrollment) { e.PreauthKey = "" },
		"enrollment_id": func(e *state.Enrollment) { e.EnrollmentID = "" },
		"nonce":         func(e *state.Enrollment) { e.Nonce = "" },
	} {
		e := enrollment()
		blank(&e)

		_, err := EnvFromEnrollment(e, EnrollmentOptions{Hostname: "shipyard", StateDir: "/var/lib/gentility"})
		if err == nil {
			t.Fatalf("an enrollment with no %s was accepted", name)
		}
		if !strings.Contains(err.Error(), name) {
			t.Fatalf("the refusal for a missing %s does not name it: %v", name, err)
		}
		if code, fatal := exitcodes.Of(err); !fatal || code != exitcodes.Usage {
			t.Fatalf("a missing %s exits (%d, %v), want (%d, true)", name, code, fatal, exitcodes.Usage)
		}
	}
}

// The one input the enrollment cannot carry and no default can invent: a node
// that guessed at its state directory would come back from a restart a
// stranger, exactly as ReadEnv says.
func TestEnvFromEnrollmentRequiresAStateDir(t *testing.T) {
	_, err := EnvFromEnrollment(enrollment(), EnrollmentOptions{Hostname: "shipyard"})
	if err == nil {
		t.Fatal("an enrollment with no state directory was accepted")
	}
	if !strings.Contains(err.Error(), "state directory") {
		t.Fatalf("the refusal does not name the state directory: %v", err)
	}
}

func TestEnvFromEnrollmentDefaultsTheHostnameToThisMachines(t *testing.T) {
	host, err := os.Hostname()
	if err != nil {
		t.Skipf("this machine has no hostname to default to: %v", err)
	}

	env, err := EnvFromEnrollment(enrollment(), EnrollmentOptions{StateDir: "/var/lib/gentility"})
	if err != nil {
		t.Fatalf("EnvFromEnrollment: %v", err)
	}
	if env.Hostname != host {
		t.Fatalf("Hostname = %q, want this machine's %q", env.Hostname, host)
	}

	env, err = EnvFromEnrollment(enrollment(), EnrollmentOptions{Hostname: "shipyard", StateDir: "/var/lib/gentility"})
	if err != nil {
		t.Fatalf("EnvFromEnrollment: %v", err)
	}
	if env.Hostname != "shipyard" {
		t.Fatalf("Hostname = %q, want the one the operator gave", env.Hostname)
	}
}

func TestEnvFromEnrollmentRefusesAGatewayAddressItCannotParse(t *testing.T) {
	e := enrollment()
	e.GatewayAddr = "gateway.example"

	_, err := EnvFromEnrollment(e, EnrollmentOptions{Hostname: "shipyard", StateDir: "/var/lib/gentility"})
	if err == nil {
		t.Fatal("a gateway address that is not an address was accepted")
	}
	if !strings.Contains(err.Error(), "gateway_addr") {
		t.Fatalf("the refusal does not name the field: %v", err)
	}
	if code, fatal := exitcodes.Of(err); !fatal || code != exitcodes.Usage {
		t.Fatalf("an unparseable gateway_addr exits (%d, %v), want (%d, true)", code, fatal, exitcodes.Usage)
	}
}

// The third door: what a server that has already enrolled comes back up with.
func enrolled() state.Node {
	return state.Node{
		NodeID:         "0198f0a2-0000-7000-8000-000000000001",
		NodeCredential: "cred-shhh",
		LastRevision:   7,
		GatewayAddr:    "fd06:6e7:0:1::1",
		GatewayPort:    443,
		CAPEM:          "-----BEGIN CERTIFICATE-----",
		HeadURL:        "https://head.example",
		Hostname:       "shipyard",
	}
}

func TestEnvFromNodeIsAReconnectWithNoPreauthKey(t *testing.T) {
	env, err := EnvFromNode(enrolled(), EnrollmentOptions{StateDir: "/var/lib/gentility"})
	if err != nil {
		t.Fatalf("EnvFromNode: %v", err)
	}

	want := Env{
		HeadURL:     "https://head.example",
		Hostname:    "shipyard",
		StateDir:    "/var/lib/gentility",
		GatewayAddr: netip.MustParseAddrPort("[fd06:6e7:0:1::1]:443"),
		GatewayCA:   "-----BEGIN CERTIFICATE-----",
	}
	if env != want {
		t.Fatalf("Env = %+v, want %+v", env, want)
	}
	// Said again, because it is the whole point: the key this node joined on
	// was single-use and expired ten minutes later. It rejoins on the node key
	// tsnet persisted, which tsnet re-presents with no auth key at all.
	if env.PreauthKey != "" {
		t.Fatal("PreauthKey is set; a reconnect has no key left to present")
	}
	if env.EnrollmentID != "" || env.Nonce != "" {
		t.Fatal("the exchange material is set; it was spent, and purged an hour later")
	}
	if env.Ephemeral || env.GatewayInsecure {
		t.Fatal("a server is neither ephemeral nor willing to speak plaintext to its gateway")
	}
}

// A node.json written before the connector recorded these cannot be rejoined
// from, and saying so is the difference between "enrol again" and a node that
// silently comes up as a stranger.
func TestEnvFromNodeRefusesAnIdentityThatPredatesTheHeadAndHostname(t *testing.T) {
	for name, blank := range map[string]func(*state.Node){
		"head_url": func(n *state.Node) { n.HeadURL = "" },
		"hostname": func(n *state.Node) { n.Hostname = "" },
	} {
		n := enrolled()
		blank(&n)

		_, err := EnvFromNode(n, EnrollmentOptions{StateDir: "/var/lib/gentility"})
		if err == nil {
			t.Fatalf("an identity with no %s was accepted", name)
		}
		if !strings.Contains(err.Error(), name) {
			t.Fatalf("the refusal for a missing %s does not name it: %v", name, err)
		}
		if !strings.Contains(err.Error(), "enrol") {
			t.Fatalf("the refusal for a missing %s does not say to enrol again: %v", name, err)
		}
		if code, fatal := exitcodes.Of(err); !fatal || code != exitcodes.Usage {
			t.Fatalf("a missing %s exits (%d, %v), want (%d, true)", name, code, fatal, exitcodes.Usage)
		}
	}
}

func TestEnvFromNodeRequiresAStateDir(t *testing.T) {
	_, err := EnvFromNode(enrolled(), EnrollmentOptions{})
	if err == nil {
		t.Fatal("a reconnect with no state directory was accepted")
	}
	if !strings.Contains(err.Error(), "state directory") {
		t.Fatalf("the refusal does not name the state directory: %v", err)
	}
}

// Same as EnvFromEnrollment (D5): no gateway placed is `serve`'s to say, not
// this function's to refuse.
func TestEnvFromNodeAcceptsAGatewaylessNode(t *testing.T) {
	n := enrolled()
	n.GatewayAddr, n.GatewayPort, n.CAPEM = "", 0, ""

	env, err := EnvFromNode(n, EnrollmentOptions{StateDir: "/var/lib/gentility"})
	if err != nil {
		t.Fatalf("EnvFromNode: %v", err)
	}
	if env.GatewayAddr.IsValid() {
		t.Fatalf("GatewayAddr = %v, want the zero value", env.GatewayAddr)
	}
}

// The hostname is the node's own, never this machine's: a server that came
// back under a different name would come back as a different node.
func TestEnvFromNodeKeepsTheNodesOwnHostname(t *testing.T) {
	env, err := EnvFromNode(enrolled(), EnrollmentOptions{
		Hostname: "somewhere-else", StateDir: "/var/lib/gentility",
	})
	if err != nil {
		t.Fatalf("EnvFromNode: %v", err)
	}
	if env.Hostname != "shipyard" {
		t.Fatalf("Hostname = %q, want the one the node joined under", env.Hostname)
	}
}

func TestEnvFromNodeRefusesAGatewayAddressItCannotParse(t *testing.T) {
	n := enrolled()
	n.GatewayAddr = "gateway.example"

	_, err := EnvFromNode(n, EnrollmentOptions{StateDir: "/var/lib/gentility"})
	if err == nil {
		t.Fatal("a gateway address that is not an address was accepted")
	}
	if !strings.Contains(err.Error(), "gateway_addr") {
		t.Fatalf("the refusal does not name the field: %v", err)
	}
}

// A reconnect Env carries no pre-auth key, and Join has to hand that empty
// string straight to tsnode.Up rather than guarding on it: whether an empty key
// is sound depends on the state directory, which is tsnode's to look at.
//
// With no persisted state the refusal that comes back is tsnode's own — which
// is the proof that Join has no guard of its own in front of it.
func TestJoinLeavesAnEmptyPreauthKeyToTsnode(t *testing.T) {
	env := Env{
		HeadURL:  "https://head.example",
		Hostname: "shipyard",
		StateDir: t.TempDir(),
	}

	_, _, err := Join(context.Background(), env, nil)
	if err == nil {
		t.Fatal("a first join with no pre-auth key was accepted")
	}
	if !strings.HasPrefix(err.Error(), "tsnode: ") {
		t.Fatalf("the refusal is Join's own, not tsnode's: %v", err)
	}
	if !strings.Contains(err.Error(), "auth key") {
		t.Fatalf("the refusal does not name the auth key: %v", err)
	}
}

// The three ways a base URL is decided, and the one that is refused. TLS is
// never skipped: an unverified https is a plaintext hop wearing a padlock.
func TestGatewayBaseURL(t *testing.T) {
	developmentBuild(t)
	loopback, tlsConfig, err := Gateway(netip.MustParseAddrPort("127.0.0.1:4006"), "", false)
	if err != nil {
		t.Fatalf("loopback: %v", err)
	}
	if loopback != "http://127.0.0.1:4006" || tlsConfig != nil {
		t.Fatalf("loopback = %q, tls %v", loopback, tlsConfig)
	}

	insecure, _, err := Gateway(netip.MustParseAddrPort("[fd06:6e7:0:1::1]:443"), "", true)
	if err != nil {
		t.Fatalf("insecure: %v", err)
	}
	if insecure != "http://[fd06:6e7:0:1::1]:443" {
		t.Fatalf("insecure = %q", insecure)
	}

	if _, _, err := Gateway(netip.MustParseAddrPort("[fd06:6e7:0:1::1]:443"), "", false); err == nil {
		t.Fatal("an overlay gateway with no pinned CA and no explicit opt-in was accepted")
	}

	if _, _, err := Gateway(netip.MustParseAddrPort("[fd06:6e7:0:1::1]:443"), "not a certificate", false); err == nil {
		t.Fatal("a ca_pem that is not a certificate was accepted")
	}
}

func TestExchangePostsEnrollmentAndNonceWithNoBearer(t *testing.T) {
	var (
		method  string
		path    string
		headers http.Header
		body    map[string]any
	)
	env, store, base := setup(t, func(w http.ResponseWriter, r *http.Request) {
		method, path, headers = r.Method, r.URL.Path, r.Header.Clone()
		_ = json.NewDecoder(r.Body).Decode(&body)
		writeJSON(t, w, 200, handedOver(nil))
	})

	if _, err := Exchange(context.Background(), nil, base, env, store); err != nil {
		t.Fatalf("Exchange: %v", err)
	}

	if method != http.MethodPost || path != "/overlay/enroll/exchange" {
		t.Fatalf("%s %s, want POST /overlay/enroll/exchange", method, path)
	}
	if body["enrollment_id"] != env.EnrollmentID || body["nonce"] != env.Nonce {
		t.Fatalf("body = %v", body)
	}
	if headers.Get("X-Node-Capver") != "1" {
		t.Fatalf("X-Node-Capver = %q, want 1", headers.Get("X-Node-Capver"))
	}
	// The nonce IS the authentication. A bearer here would be a
	// credential the node does not have yet.
	if got := headers.Get("Authorization"); got != "" {
		t.Fatalf("Authorization = %q, want none — the node holds no credential yet", got)
	}
	if got := headers.Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type = %q", got)
	}
}

func TestExchangeStoresBothSecretsAndNoConfigCursor(t *testing.T) {
	env, store, base := setup(t, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, 200, handedOver(nil))
	})
	env.GatewayCA = "-----BEGIN CERTIFICATE-----"

	node, err := Exchange(context.Background(), nil, base, env, store)
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	if node.NodeCredential != "cred-shhh" || node.RuntimeToken != "rt-shhh" {
		t.Fatalf("node = %+v", node)
	}
	if node.LastRevision != state.NoRevision {
		t.Fatalf("LastRevision = %d, want NoRevision", node.LastRevision)
	}

	stored, err := store.LoadNode()
	if err != nil {
		t.Fatalf("LoadNode: %v", err)
	}
	if stored != node {
		t.Fatalf("stored %+v, returned %+v", stored, node)
	}
	// The gateway and the CA that verifies it, written down together and at
	// once. The config document refreshes them later; it must not be the first
	// to write them, or a crash between the exchange and the first document
	// leaves a node that can never reach the gateway that would repair it.
	if stored.GatewayAddr != "fd06:6e7:0:1::1" || stored.GatewayPort != 443 {
		t.Fatalf("the gateway the exchange went to was not written down: %+v", stored)
	}
	if stored.CAPEM != "-----BEGIN CERTIFICATE-----" {
		t.Fatalf("CAPEM = %q, want the CA the join was handed", stored.CAPEM)
	}
	// The pre-auth key is single-use and ten minutes old; these two are what
	// brings the node back after a reboot, and the exchange is the only moment
	// that holds them.
	if stored.HeadURL != "https://head.example" {
		t.Fatalf("HeadURL = %q, want the head this node joined", stored.HeadURL)
	}
	if stored.Hostname != "case-17" {
		t.Fatalf("Hostname = %q, want the name this node joined under", stored.Hostname)
	}
}

// The window this closes: the exchange has succeeded, `serve` has cleared the
// enrollment, and the first config document has not arrived. A restart now
// takes the reconnect door — and it must find a gateway to pin, or the node can
// never fetch the document that would have handed it one.
func TestEnvFromNodeCanPinAGatewayTheExchangeJustWroteDown(t *testing.T) {
	env, store, base := setup(t, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, 200, handedOver(nil))
	})
	env.GatewayCA = "-----BEGIN CERTIFICATE-----"

	if _, err := Exchange(context.Background(), nil, base, env, store); err != nil {
		t.Fatalf("Exchange: %v", err)
	}

	// No RecordGateway anywhere: this is the node as the exchange alone left it.
	stored, err := store.LoadNode()
	if err != nil {
		t.Fatalf("LoadNode: %v", err)
	}

	reconnect, err := EnvFromNode(stored, EnrollmentOptions{StateDir: env.StateDir})
	if err != nil {
		t.Fatalf("EnvFromNode: %v", err)
	}
	if reconnect.GatewayCA == "" {
		t.Fatal("the reconnect has no CA to pin, so it can never reach the gateway again")
	}
	if reconnect.GatewayAddr != env.GatewayAddr {
		t.Fatalf("GatewayAddr = %v, want %v", reconnect.GatewayAddr, env.GatewayAddr)
	}
}

// A join with no gateway placed in front of it writes no gateway down. Half a
// record — an address with no CA, or a placeholder where an address should be —
// is what a later reconnect would try to dial.
func TestExchangeWithNoGatewayWritesNoneDown(t *testing.T) {
	env, store, base := setup(t, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, 200, handedOver(nil))
	})
	env.GatewayAddr = netip.AddrPort{}

	node, err := Exchange(context.Background(), nil, base, env, store)
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	if node.GatewayAddr != "" || node.GatewayPort != 0 || node.CAPEM != "" {
		t.Fatalf("a gatewayless exchange wrote a gateway down: %+v", node)
	}
}

// The loopback dev recipe: a gateway, and no CA to verify it with, because the
// listener is on this machine and there is no network to protect. It keeps
// writing exactly what it always wrote.
func TestExchangeOverLoopbackWritesTheAddressAndNoCA(t *testing.T) {
	env, store, base := setup(t, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, 200, handedOver(nil))
	})
	env.GatewayAddr = netip.MustParseAddrPort("127.0.0.1:4006")

	node, err := Exchange(context.Background(), nil, base, env, store)
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	if node.GatewayAddr != "127.0.0.1" || node.GatewayPort != 4006 {
		t.Fatalf("node = %+v, want the loopback gateway written down", node)
	}
	if node.CAPEM != "" {
		t.Fatalf("CAPEM = %q, want none — the dev recipe pins nothing", node.CAPEM)
	}
}

// The mothership answers `config: null` for a node the renderer has not
// reached (the enrollment endpoint), and a document when it has. Even
// then the cursor stays unset: one path applies a document and it is the
// config client's, whose first pull is held open and answers at once.
func TestExchangeDoesNotApplyAConfigItWasHanded(t *testing.T) {
	env, store, base := setup(t, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, 200, handedOver(map[string]any{
			"renderer_version": 1, "revision": 0, "capver_floor": 1, "node_id": "n",
		}))
	})

	node, err := Exchange(context.Background(), nil, base, env, store)
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	if node.LastRevision != state.NoRevision {
		t.Fatalf("LastRevision = %d, want NoRevision", node.LastRevision)
	}
}

// A machine or a user gets a node credential and nothing else.
func TestExchangeAcceptsAnAnswerWithNoRuntimeToken(t *testing.T) {
	env, store, base := setup(t, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, 200, map[string]any{
			"node_id":         "0198f0a2-0000-7000-8000-000000000001",
			"node_credential": "cred-shhh",
			"runtime_token":   nil,
			"config":          nil,
		})
	})

	node, err := Exchange(context.Background(), nil, base, env, store)
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	if node.RuntimeToken != "" {
		t.Fatalf("RuntimeToken = %q, want none", node.RuntimeToken)
	}
}

// A 2xx that names no credential is worse than a refusal: the node would
// record an identity it cannot present and never enrol again.
func TestExchangeRefusesAnAnswerWithNoCredential(t *testing.T) {
	env, store, base := setup(t, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, 200, map[string]any{"node_id": "n", "config": nil})
	})

	if _, err := Exchange(context.Background(), nil, base, env, store); err == nil {
		t.Fatal("an answer with no node_credential was accepted")
	}
	if _, err := store.LoadNode(); !state.IsNotEnrolled(err) {
		t.Fatalf("state was written anyway: %v", err)
	}
}

func TestExchangeRetryAfterA5xxGetsTheSameMaterial(t *testing.T) {
	var calls int
	env, store, base := setup(t, func(w http.ResponseWriter, _ *http.Request) {
		calls++
		if calls == 1 {
			writeJSON(t, w, 500, map[string]string{"error": "internal_error"})
			return
		}
		writeJSON(t, w, 200, handedOver(nil))
	})

	_, err := Exchange(context.Background(), nil, base, env, store)
	if err == nil {
		t.Fatal("a 500 was read as success")
	}
	if _, fatal := exitcodes.Of(err); fatal {
		t.Fatalf("a 500 must be retryable: %v", err)
	}

	// The retry is idempotent on (enrollment_id, node_id, nonce) for an hour,
	// so the same request gets the same two secrets back.
	node, err := Exchange(context.Background(), nil, base, env, store)
	if err != nil {
		t.Fatalf("the retry: %v", err)
	}
	if node.NodeCredential != "cred-shhh" {
		t.Fatalf("node = %+v", node)
	}
}

func TestExchange410IsMaterialExpired(t *testing.T) {
	env, store, base := setup(t, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, 410, map[string]string{"error": "material_purged"})
	})

	_, err := Exchange(context.Background(), nil, base, env, store)
	if code, fatal := exitcodes.Of(err); !fatal || code != exitcodes.MaterialExpired {
		t.Fatalf("exit code = (%d, %v), want (%d, true); err = %v", code, fatal, exitcodes.MaterialExpired, err)
	}
	if !strings.Contains(err.Error(), "material_purged") {
		t.Fatalf("the refusal does not name what the mothership said: %v", err)
	}
}

func TestExchange403IsEnrollRefused(t *testing.T) {
	for _, reason := range []string{"refused", "compromised", "unknown_peer"} {
		env, store, base := setup(t, func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(t, w, 403, map[string]string{"error": reason})
		})

		_, err := Exchange(context.Background(), nil, base, env, store)
		if code, fatal := exitcodes.Of(err); !fatal || code != exitcodes.EnrollRefused {
			t.Fatalf("%s: exit code = (%d, %v), want (%d, true); err = %v",
				reason, code, fatal, exitcodes.EnrollRefused, err)
		}
	}
}

// The exchange presents no bearer, so a 401 can only be the ingress: no
// X-Overlay-Peer, two of them, or a verifier that would not believe the one
// there was. Every one of those is the gateway's, and the nonce is good for an
// hour — so the answer is to come back, not to throw the enrollment away.
func TestExchange401AboutThePeerIsRetryable(t *testing.T) {
	for _, reason := range []string{"no_overlay_peer", "ambiguous_peer", "peer_unverified"} {
		env, store, base := setup(t, func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(t, w, 401, map[string]string{"error": reason})
		})

		_, err := Exchange(context.Background(), nil, base, env, store)
		if err == nil {
			t.Fatalf("%s: want an error", reason)
		}
		if _, fatal := exitcodes.Of(err); fatal {
			t.Fatalf("%s: a 401 on the exchange is the gateway's and must be retryable: %v", reason, err)
		}
	}
}

func TestExchange426IsCapverTooOld(t *testing.T) {
	env, store, base := setup(t, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, 426, map[string]any{"error": "capver_below_floor", "floor": 4})
	})

	_, err := Exchange(context.Background(), nil, base, env, store)
	if code, fatal := exitcodes.Of(err); !fatal || code != exitcodes.CapverTooOld {
		t.Fatalf("exit code = (%d, %v), want (%d, true); err = %v", code, fatal, exitcodes.CapverTooOld, err)
	}
	if !strings.Contains(err.Error(), "4") {
		t.Fatalf("the refusal does not carry the floor: %v", err)
	}
}

// A 422 is this connector's own request being malformed. Retrying re-sends it.
func TestExchange422IsFatal(t *testing.T) {
	env, store, base := setup(t, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, 422, map[string]string{"error": "invalid_request"})
	})

	_, err := Exchange(context.Background(), nil, base, env, store)
	if _, fatal := exitcodes.Of(err); !fatal {
		t.Fatalf("a 422 must not be retried: %v", err)
	}
}

func TestExchangeGoesToTheGatewayAddress(t *testing.T) {
	var host string
	env, store, base := setup(t, func(w http.ResponseWriter, r *http.Request) {
		host = r.Host
		writeJSON(t, w, 200, handedOver(nil))
	})

	if _, err := Exchange(context.Background(), nil, base, env, store); err != nil {
		t.Fatalf("Exchange: %v", err)
	}

	parsed, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	if host != parsed.Host {
		t.Fatalf("the exchange reached %q, want the gateway at %q", host, parsed.Host)
	}
}

func setup(t *testing.T, handler http.HandlerFunc) (Env, *state.Store, string) {
	t.Helper()

	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	vars := environment()
	vars["OVERLAY_STATE_DIR"] = filepath.Join(t.TempDir(), "gentility")
	env, err := ReadEnv(lookupIn(vars))
	if err != nil {
		t.Fatalf("ReadEnv: %v", err)
	}

	return env, &state.Store{Dir: env.StateDir}, server.URL
}

func handedOver(config map[string]any) map[string]any {
	return map[string]any{
		"node_id":         "0198f0a2-0000-7000-8000-000000000001",
		"node_credential": "cred-shhh",
		"runtime_token":   "rt-shhh",
		"config":          config,
	}
}

func writeJSON(t *testing.T, w http.ResponseWriter, status int, body any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		t.Errorf("encoding the reply: %v", err)
	}
}

// A head that no longer holds this node's identity is EnrollRefused: the same
// code a revoked node's exchange gets, because it is the same remedy. It has to
// be fatal or the supervisor restarts straight back into a login nobody is
// going to give, every two seconds, forever.
func TestARefusedRejoinIsEnrollRefused(t *testing.T) {
	refused := upRefusal(fmt.Errorf("tsnode: rejoining %s: %w", "https://head.example", tsnode.ErrRejoinRefused))

	code, fatal := exitcodes.Of(refused)
	if !fatal || code != exitcodes.EnrollRefused {
		t.Fatalf("a refused rejoin exits (%d, %v), want (%d, true)", code, fatal, exitcodes.EnrollRefused)
	}
	// The wrap must not swallow what the operator is meant to read.
	if !strings.Contains(refused.Error(), "Re-enrol") {
		t.Errorf("the refusal no longer says what to do: %v", refused)
	}
}

// Interface mode without root is the command's fault, so it is Usage, which
// the units forbid restarting into, and not Temporary, which they retry.
func TestInterfaceModeWithoutRootIsUsage(t *testing.T) {
	refused := upRefusal(fmt.Errorf("tsnode: %w: a TUN, its routes and the system resolver are root's", tsnode.ErrNeedsRoot))

	if code, fatal := exitcodes.Of(refused); !fatal || code != exitcodes.Usage {
		t.Fatalf("interface mode without root exits (%d, %v), want (%d, true)", code, fatal, exitcodes.Usage)
	}
}

// Everything else Up can fail with is a head that is down or a network that is
// not up yet, and a restart is the retry.
func TestAnOrdinaryJoinFailureStaysRetryable(t *testing.T) {
	if _, fatal := exitcodes.Of(upRefusal(errors.New("dial tcp: connection refused"))); fatal {
		t.Error("an ordinary join failure was treated as fatal")
	}
}

// A node.json written before 6net left Tailscale's range names its gateway
// there, and the config document that would correct it arrives through that
// gateway. Both stored doors read it as the same interface under the new
// range; nothing else is translated.
func TestAStoredGatewayInTheLegacyRangeIsReadUnderTheNewOne(t *testing.T) {
	for _, tc := range []struct {
		stored string
		want   string
	}{
		{"fd7a:115c:a1e0:2::1", "[fd06:6e7:0:2::1]:443"},
		{"fd7a:115c:a1e0:ab:1:2:3:4", "[fd06:6e7:0:ab:1:2:3:4]:443"},
		{"fd06:6e7:0:2::1", "[fd06:6e7:0:2::1]:443"},
		{"fd7a:115c:a1e1::1", "[fd7a:115c:a1e1::1]:443"},
		{"::1", "[::1]:443"},
		{"127.0.0.1", "127.0.0.1:443"},
	} {
		n := enrolled()
		n.GatewayAddr = tc.stored
		env, err := EnvFromNode(n, EnrollmentOptions{StateDir: "/var/lib/gentility"})
		if err != nil {
			t.Fatalf("EnvFromNode(%s): %v", tc.stored, err)
		}
		if got := env.GatewayAddr.String(); got != tc.want {
			t.Fatalf("stored identity %s: GatewayAddr = %s, want %s", tc.stored, got, tc.want)
		}

		e := enrollment()
		e.GatewayAddr = tc.stored
		env, err = EnvFromEnrollment(e, EnrollmentOptions{Hostname: "shipyard", StateDir: "/var/lib/gentility"})
		if err != nil {
			t.Fatalf("EnvFromEnrollment(%s): %v", tc.stored, err)
		}
		if got := env.GatewayAddr.String(); got != tc.want {
			t.Fatalf("stored enrollment %s: GatewayAddr = %s, want %s", tc.stored, got, tc.want)
		}
	}
}

// An operator's input is never silently rewritten: the shim is for what the
// connector itself wrote down, not for what a recipe says.
func TestTheEnvironmentsGatewayInTheLegacyRangeIsTakenAsGiven(t *testing.T) {
	vars := environment()
	vars["OVERLAY_GATEWAY_ADDR"] = "[fd7a:115c:a1e0:2::1]:443"
	env, err := ReadEnv(lookupIn(vars))
	if err != nil {
		t.Fatalf("ReadEnv: %v", err)
	}
	if want := netip.MustParseAddrPort("[fd7a:115c:a1e0:2::1]:443"); env.GatewayAddr != want {
		t.Fatalf("GatewayAddr = %v, want %v as given", env.GatewayAddr, want)
	}
}
