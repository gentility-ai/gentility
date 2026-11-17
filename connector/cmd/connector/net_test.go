package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gentility-ai/gentility/connector/internal/contracttest"
	"github.com/gentility-ai/gentility/connector/internal/exitcodes"
	"github.com/gentility-ai/gentility/connector/internal/state"
)

const (
	testBearer  = "bearer-of-the-person"
	testPreauth = "tskey-auth-secret"
	testNonce   = "nonce-secret"
)

// joinWorld is a person's machine as `net join` sees it, every seam replaced:
// a person (uid 501) signed in to an enrollment server that counts its
// requests, sudo on the PATH, no unit installed, and a daemon directory the
// fake install writes its status into.
type joinWorld struct {
	requests *atomic.Int32
	labels   *[]string
	daemon   *state.Store
	argvs    [][]string
	// install is what the fake elevated command does, given its argv; nil
	// writes a fresh live status.
	install func(argv []string) error
}

func newJoinWorld(t *testing.T) *joinWorld {
	t.Helper()
	w := &joinWorld{requests: new(atomic.Int32), labels: new([]string), daemon: &state.Store{Dir: t.TempDir()}}

	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		w.requests.Add(1)
		var body struct {
			MachineLabel string `json:"machine_label"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		*w.labels = append(*w.labels, body.MachineLabel)
		if r.URL.Path != "/oauth/enrollment" || r.Header.Get("Authorization") != "Bearer "+testBearer {
			rw.WriteHeader(http.StatusUnauthorized)
			return
		}
		writeDeviceJSON(rw, map[string]any{"enrollment": map[string]any{
			"enrollment_id": "enr-1",
			"preauth_key":   testPreauth,
			"nonce":         testNonce,
			"head_url":      "https://head.example",
			"expires_at":    time.Now().Add(10 * time.Minute).UTC().Format(time.RFC3339),
		}})
	}))
	t.Cleanup(srv.Close)
	signIn(t, srv.URL, testBearer)

	asUID(t, 501)
	swap(t, &sudoUser, func() string { return "" })
	swap(t, &unitInstalled, func() bool { return false })
	swap(t, &hostname, func() string { return "cedar.lan" })
	swap(t, &lookSudo, func() (string, error) { return "/usr/bin/sudo", nil })
	swap(t, &checkPlacement, func(string) error { return nil })
	swap(t, &daemonStatus, w.daemon.LoadStatus)
	swap(t, &waitFor, 2*time.Second)
	swap(t, &pollEvery, 10*time.Millisecond)
	swap(t, &elevate, func(argv []string, _ io.Reader, _, _ io.Writer) error {
		w.argvs = append(w.argvs, argv)
		if w.install != nil {
			return w.install(argv)
		}
		return w.daemon.SaveStatus(liveStatus(time.Now()))
	})
	return w
}

// swap replaces a seam for the length of one test.
func swap[T any](t *testing.T, seam *T, value T) {
	t.Helper()
	previous := *seam
	*seam = value
	t.Cleanup(func() { *seam = previous })
}

func liveStatus(written time.Time) state.Status {
	return state.Status{
		WrittenAt: written,
		Name:      "cedar.morgan.northwind.6net.io",
		Interface: "gentility0",
		Organizations: []state.OrgStatus{
			{Org: "northwind", Address: "fd06:6e7:0:2::5", Label: "cedar.morgan"},
			{Org: "acme", Address: "fd06:6e7:0:9::3", Label: "cedar.morgan"},
		},
	}
}

func (w *joinWorld) join(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := run(append([]string{"net", "join"}, args...), &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

// stagingDirs lists what `net join` left in the person's state directory
// beyond what was there before it ran.
func stagingDirs(t *testing.T) []string {
	t.Helper()
	left, _ := filepath.Glob(filepath.Join(state.DefaultDir(), "net-join-*"))
	return left
}

// The success path: one elevated command holding only this binary, the
// install verb and the staging directory; the material in it private while
// the install runs and gone afterwards; and the name and the address printed,
// never the secrets.
func TestNetJoinInstallsThroughSudoAndPrintsTheName(t *testing.T) {
	w := newJoinWorld(t)
	self, err := thisBinary()
	if err != nil {
		t.Fatal(err)
	}

	var staged string
	w.install = func(argv []string) error {
		staged = argv[6]
		info, err := os.Stat(staged)
		if err != nil {
			t.Fatalf("staging directory during the install: %v", err)
		}
		if mode := info.Mode().Perm(); mode != 0o700 {
			t.Errorf("staging directory mode = %o, want 700", mode)
		}
		file, err := os.Stat(filepath.Join(staged, "enrollment.json"))
		if err != nil {
			t.Fatalf("staged enrollment: %v", err)
		}
		if mode := file.Mode().Perm(); mode != 0o600 {
			t.Errorf("staged enrollment mode = %o, want 600", mode)
		}
		material, err := (&state.Store{Dir: staged}).LoadEnrollment()
		if err != nil || material.PreauthKey != testPreauth || material.Nonce != testNonce || material.EnrollmentID != "enr-1" {
			t.Errorf("staged material = %v (%v), want the served enrollment", material, err)
		}
		return w.daemon.SaveStatus(liveStatus(time.Now()))
	}

	code, stdout, stderr := w.join(t)
	if code != exitcodes.OK {
		t.Fatalf("net join = %d, want 0:\n%s%s", code, stdout, stderr)
	}

	if len(w.argvs) != 1 {
		t.Fatalf("elevated %d commands, want 1: %v", len(w.argvs), w.argvs)
	}
	want := []string{"/usr/bin/sudo", "--", self, "install", "--interface", "--source-state-dir", staged, "--place-binary", "/usr/local/bin/gentility"}
	if strings.Join(w.argvs[0], "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("argv = %q, want %q", w.argvs[0], want)
	}
	for _, arg := range w.argvs[0] {
		if strings.Contains(arg, testBearer) {
			t.Fatalf("the bearer is in the elevated argv: %q", w.argvs[0])
		}
	}
	if !strings.HasPrefix(staged, state.DefaultDir()+string(filepath.Separator)) {
		t.Fatalf("staging %s is not under the person's state directory %s", staged, state.DefaultDir())
	}
	if _, err := os.Stat(staged); !os.IsNotExist(err) {
		t.Fatalf("staging directory left behind: %v", err)
	}
	if got := strings.Join(*w.labels, ","); got != "cedar" {
		t.Fatalf("labels sent = %q, want the hostname's first label", got)
	}

	wantOut := "on 6net as cedar.morgan.northwind.6net.io  fd06:6e7:0:2::5 (northwind)  interface gentility0\n  fd06:6e7:0:9::3 (acme)\n"
	if stdout != wantOut {
		t.Fatalf("stdout = %q, want %q", stdout, wantOut)
	}
	for _, secret := range []string{testPreauth, testNonce, testBearer} {
		if strings.Contains(stdout+stderr, secret) {
			t.Fatalf("output carries a secret:\n%s%s", stdout, stderr)
		}
	}
}

func TestNetJoinSendsTheGivenLabel(t *testing.T) {
	w := newJoinWorld(t)
	if code, stdout, stderr := w.join(t, "--label", "box"); code != exitcodes.OK {
		t.Fatalf("net join = %d:\n%s%s", code, stdout, stderr)
	}
	if got := strings.Join(*w.labels, ","); got != "box" {
		t.Fatalf("labels sent = %q, want box", got)
	}
}

// A root login holds its own bearer, so the same steps run without sudo.
func TestNetJoinAsARootLoginRunsTheInstallWithoutSudo(t *testing.T) {
	w := newJoinWorld(t)
	asUID(t, 0)
	swap(t, &lookSudo, func() (string, error) { return "", errors.New("not consulted") })

	if code, stdout, stderr := w.join(t); code != exitcodes.OK {
		t.Fatalf("net join = %d:\n%s%s", code, stdout, stderr)
	}
	if len(w.argvs) != 1 || w.argvs[0][1] != "install" {
		t.Fatalf("argv = %q, want this binary's install with no sudo", w.argvs)
	}
}

// Every refusal comes before the fetch, because every fetch mints a key.
func TestNetJoinRefusesBeforeFetching(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T, w *joinWorld)
		args  []string
		say   string
	}{
		{"logged out", func(t *testing.T, w *joinWorld) {
			t.Setenv("GENTILITY_STATE_DIR", t.TempDir())
		}, nil, invoked + " login"},
		{"unit installed", func(t *testing.T, w *joinWorld) {
			swap(t, &unitInstalled, func() bool { return true })
		}, nil, invoked + " net leave"},
		{"under sudo", func(t *testing.T, w *joinWorld) {
			asUID(t, 0)
			swap(t, &sudoUser, func() string { return "morgan" })
		}, nil, "`" + invoked + " net join`"},
		{"no sudo", func(t *testing.T, w *joinWorld) {
			swap(t, &lookSudo, func() (string, error) { return "", errors.New("executable file not found in $PATH") })
		}, nil, "no sudo"},
		{"blank label", func(t *testing.T, w *joinWorld) {}, []string{"--label", " "}, "--label"},
		{"no hostname", func(t *testing.T, w *joinWorld) {
			swap(t, &hostname, func() string { return "" })
		}, nil, "--label"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := newJoinWorld(t)
			c.setup(t, w)

			code, stdout, stderr := w.join(t, c.args...)
			if code != exitcodes.Usage {
				t.Fatalf("net join = %d, want %d:\n%s%s", code, exitcodes.Usage, stdout, stderr)
			}
			if !strings.Contains(stderr, c.say) {
				t.Fatalf("stderr = %q, want it to say %q", stderr, c.say)
			}
			if n := w.requests.Load(); n != 0 {
				t.Fatalf("%d requests made, want none", n)
			}
			if len(w.argvs) != 0 {
				t.Fatalf("elevated %q, want nothing", w.argvs)
			}
		})
	}
}

// A refused fetch has nothing to install and stages nothing.
func TestNetJoinAnExpiredSessionSaysToLogIn(t *testing.T) {
	w := newJoinWorld(t)
	signIn(t, os.Getenv("GENTILITY_MCP_URL"), "a-bearer-the-server-does-not-know")

	code, _, stderr := w.join(t)
	if code != 1 || !strings.Contains(stderr, "run `gentility login`") || strings.Count(stderr, "login`") != 1 {
		t.Fatalf("net join = %d, stderr %q; want 1 naming login", code, stderr)
	}
	if len(w.argvs) != 0 || len(stagingDirs(t)) != 0 {
		t.Fatalf("elevated %q, staging %q; want neither", w.argvs, stagingDirs(t))
	}
}

// A /usr/local/bin the person owns (Homebrew on an Intel Mac) is a placement
// root's install would refuse after the key was spent, so it is refused
// before the fetch.
func TestNetJoinRefusesAnUntrustedPlacementBeforeFetching(t *testing.T) {
	w := newJoinWorld(t)
	var asked string
	swap(t, &checkPlacement, func(path string) error {
		asked = path
		return errors.New("supervise: refusing to place the binary under /usr/local/bin: it is owned by uid 501, not root")
	})

	code, _, stderr := w.join(t)
	if code != exitcodes.Usage || !strings.Contains(stderr, "/usr/local/bin: it is owned by uid 501") {
		t.Fatalf("net join = %d, stderr %q; want a usage refusal naming the directory", code, stderr)
	}
	if asked != placedBinary {
		t.Fatalf("checked %q, want %q", asked, placedBinary)
	}
	if n := w.requests.Load(); n != 0 || len(w.argvs) != 0 {
		t.Fatalf("%d requests, %d elevations; want none", n, len(w.argvs))
	}
}

// A wrong sudo password or a refused install exits non-zero, having said why
// on the terminal already: the line points there and says nothing joined.
func TestNetJoinAFailedInstallExitsOneAndRemovesTheStaging(t *testing.T) {
	w := newJoinWorld(t)
	w.install = func([]string) error { return exec.Command("false").Run() }

	code, _, stderr := w.join(t)
	want := "sudo or the install failed (see above); nothing was joined"
	if code != 1 || !strings.Contains(stderr, want) {
		t.Fatalf("net join = %d, stderr %q; want 1 and %q", code, stderr, want)
	}
	if left := stagingDirs(t); len(left) != 0 {
		t.Fatalf("staging left behind: %q", left)
	}
}

// sudo that never started said nothing above, so the line carries the error.
func TestNetJoinASudoThatNeverStartedSaysWhy(t *testing.T) {
	w := newJoinWorld(t)
	w.install = func([]string) error { return errors.New("fork/exec /usr/bin/sudo: permission denied") }

	code, _, stderr := w.join(t)
	if code != 1 || !strings.Contains(stderr, "running sudo: fork/exec") || !strings.Contains(stderr, "nothing was joined") {
		t.Fatalf("net join = %d, stderr %q; want 1 naming the exec failure", code, stderr)
	}
	if left := stagingDirs(t); len(left) != 0 {
		t.Fatalf("staging left behind: %q", left)
	}
}

func TestSecondsSaysAWaitAsAPersonReadsIt(t *testing.T) {
	if got := seconds(60 * time.Second); got != "60s" {
		t.Fatalf("seconds(60s) = %q, want %q", got, "60s")
	}
}

// Help is an answer, not a mistake: it goes to stdout and exits 0, and `net
// status --help` describes `net status`.
func TestNetHelpPrintsTheUsageToStdout(t *testing.T) {
	for _, ask := range []string{"--help", "-h", "help"} {
		var stdout, stderr bytes.Buffer
		if code := run([]string{"net", ask}, &stdout, &stderr); code != 0 || stderr.Len() != 0 {
			t.Fatalf("net %s = %d, stderr %q; want 0 and nothing", ask, code, stderr.String())
		}
		if !strings.HasPrefix(stdout.String(), "usage: "+invoked+" net <command>") {
			t.Fatalf("net %s printed:\n%s", ask, stdout.String())
		}
	}

	var stdout, stderr bytes.Buffer
	if code := run([]string{"net", "status", "--help"}, &stdout, &stderr); code != 0 {
		t.Fatalf("net status --help = %d: %s", code, stderr.String())
	}
	if !strings.HasPrefix(stdout.String(), "usage: "+invoked+" net status ") {
		t.Fatalf("net status --help printed:\n%s", stdout.String())
	}
}

// A status.json from before this join is an earlier daemon's, and is not this
// one coming up.
func TestNetJoinDoesNotReadAnEarlierSnapshotAsSuccess(t *testing.T) {
	w := newJoinWorld(t)
	swap(t, &waitFor, 100*time.Millisecond)
	w.install = func([]string) error {
		return w.daemon.SaveStatus(liveStatus(time.Now().Add(-time.Second)))
	}

	code, stdout, stderr := w.join(t)
	if code != 1 {
		t.Fatalf("net join = %d, want 1:\n%s%s", code, stdout, stderr)
	}
	for _, want := range []string{invoked + " net status", "journalctl -u gentility-connector"} {
		if !strings.Contains(stderr, want) {
			t.Fatalf("stderr = %q, want it to name %q", stderr, want)
		}
	}
	if stdout != "" {
		t.Fatalf("stdout = %q, want no success line", stdout)
	}
	if left := stagingDirs(t); len(left) != 0 {
		t.Fatalf("staging left behind: %q", left)
	}
}

func TestNetLeaveAsksSudoToRunLeave(t *testing.T) {
	asUID(t, 501)
	swap(t, &lookSudo, func() (string, error) { return "/usr/bin/sudo", nil })
	var argvs [][]string
	swap(t, &elevate, func(argv []string, _ io.Reader, _, _ io.Writer) error {
		argvs = append(argvs, argv)
		return nil
	})

	var stdout, stderr bytes.Buffer
	if code := run([]string{"net", "leave"}, &stdout, &stderr); code != exitcodes.OK {
		t.Fatalf("net leave = %d:\n%s%s", code, stdout.String(), stderr.String())
	}

	binary := placedBinary
	if _, err := os.Stat(placedBinary); err != nil {
		binary, _ = thisBinary()
	}
	want := []string{"/usr/bin/sudo", "--", binary, "leave"}
	if len(argvs) != 1 || strings.Join(argvs[0], " ") != strings.Join(want, " ") {
		t.Fatalf("argvs = %q, want one %q", argvs, want)
	}
}

func TestNetStatusIsStatus(t *testing.T) {
	t.Setenv("GENTILITY_STATE_DIR", t.TempDir())
	dir := t.TempDir()

	var plain, viaNet, stderr bytes.Buffer
	if code := run([]string{"status", "--state-dir", dir}, &plain, &stderr); code != 0 {
		t.Fatalf("status = %d: %s", code, stderr.String())
	}
	if code := run([]string{"net", "status", "--state-dir", dir}, &viaNet, &stderr); code != 0 {
		t.Fatalf("net status = %d: %s", code, stderr.String())
	}
	if plain.String() != viaNet.String() {
		t.Fatalf("net status printed\n%s\nstatus printed\n%s", viaNet.String(), plain.String())
	}
}

func TestNetWithoutAKnownVerbIsAUsageError(t *testing.T) {
	for _, args := range [][]string{{"net"}, {"net", "bogus"}} {
		var stdout, stderr bytes.Buffer
		if code := run(args, &stdout, &stderr); code != exitcodes.Usage {
			t.Fatalf("run(%q) = %d, want %d", args, code, exitcodes.Usage)
		}
		for _, verb := range []string{"join", "leave", "status", "serve", "unserve"} {
			if !strings.Contains(stderr.String(), verb) {
				t.Fatalf("run(%q) usage does not list %s:\n%s", args, verb, stderr.String())
			}
		}
	}
}

// A person's login ends by saying what joining is, now that it is not part of
// the login.
func TestRunLoginPointsAPersonAtNetJoin(t *testing.T) {
	t.Setenv("GENTILITY_STATE_DIR", t.TempDir())
	srv := deviceFlowServer(t, nil)

	var stdout, stderr bytes.Buffer
	if code := run([]string{"login", "--url", srv.URL}, &stdout, &stderr); code != 0 {
		t.Fatalf("login = %d:\n%s%s", code, stdout.String(), stderr.String())
	}
	if !strings.HasSuffix(stdout.String(), invoked+" net join\n") {
		t.Fatalf("stdout = %q, want it to end naming net join", stdout.String())
	}
}

// A Ctrl-C while waiting ends the wait, says the daemon is installed, and
// still removes the staging.
func TestNetJoinInterruptedWhileWaitingRemovesTheStaging(t *testing.T) {
	w := newJoinWorld(t)
	swap(t, &waitFor, 10*time.Second)
	w.install = func([]string) error {
		self, err := os.FindProcess(os.Getpid())
		if err != nil {
			return err
		}
		return self.Signal(os.Interrupt)
	}

	start := time.Now()
	code, _, stderr := w.join(t)
	if code != 1 || !strings.Contains(stderr, "the daemon is installed") || !strings.Contains(stderr, invoked+" net status") {
		t.Fatalf("net join = %d, stderr %q; want 1 saying the daemon is installed", code, stderr)
	}
	if waited := time.Since(start); waited > 5*time.Second {
		t.Fatalf("waited %s after the interrupt", waited)
	}
	if left := stagingDirs(t); len(left) != 0 {
		t.Fatalf("staging left behind: %q", left)
	}
}

// serveWorld is a person's joined machine as `net serve` sees it: signed in to
// a fake MCP host, run as themselves, and a daemon whose status.json lists
// the organizations given.
func serveWorld(t *testing.T, orgs []state.OrgStatus, reply func(*testing.T, toolCall) string) *[]toolCall {
	t.Helper()
	srv, seen := mcpServer(t, testBearer, reply)
	signIn(t, srv.URL, testBearer)

	asUID(t, 501)
	swap(t, &sudoUser, func() string { return "" })
	daemon := &state.Store{Dir: t.TempDir()}
	if orgs != nil {
		written := liveStatus(time.Now())
		written.Organizations = orgs
		if err := daemon.SaveStatus(written); err != nil {
			t.Fatalf("SaveStatus: %v", err)
		}
	}
	swap(t, &daemonStatus, daemon.LoadStatus)
	return seen
}

var (
	northwindCedar = state.OrgStatus{Org: "northwind", Address: "fd06:6e7:0:2::5", Label: "cedar.morgan", Name: "cedar.morgan.northwind.6net.io"}
	acmeCedar      = state.OrgStatus{Org: "acme", Address: "fd06:6e7:0:9::3", Label: "cedar.morgan", Name: "cedar.morgan.acme.6net.io"}
)

func runNet(args ...string) (int, string, string) {
	var stdout, stderr bytes.Buffer
	code := run(append([]string{"net"}, args...), &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func declared(c toolCall) string {
	org := c.args["organization"].(string)
	name := map[string]string{"northwind": northwindCedar.Name, "acme": acmeCedar.Name}[org]
	approved := org == "northwind"
	state := "declared"
	if approved {
		state = "approved"
	}
	reply, _ := json.Marshal(map[string]any{
		"success":  true,
		"service":  map[string]any{"id": "svc-" + org, "kind": "ssh", "port": c.args["port"], "state": state},
		"name":     name,
		"approved": approved,
	})
	return string(reply)
}

// One call per organization status.json lists, naming this machine by its
// address there; an admin's is approved and a member's waits, and each says so.
func TestNetServeDeclaresSshInEveryOrganization(t *testing.T) {
	seen := serveWorld(t, []state.OrgStatus{northwindCedar, acmeCedar}, func(_ *testing.T, c toolCall) string { return declared(c) })

	code, stdout, stderr := runNet("serve", "ssh")
	if code != exitcodes.OK {
		t.Fatalf("net serve ssh = %d:\n%s%s", code, stdout, stderr)
	}

	want := []map[string]any{
		{"organization": "northwind", "node": "fd06:6e7:0:2::5", "kind": "ssh", "port": float64(22)},
		{"organization": "acme", "node": "fd06:6e7:0:9::3", "kind": "ssh", "port": float64(22)},
	}
	if len(*seen) != len(want) {
		t.Fatalf("the host saw %d calls, want %d: %#v", len(*seen), len(want), *seen)
	}
	for i, c := range *seen {
		if c.name != "net_service_declare" {
			t.Fatalf("call %d: tool %q, want net_service_declare", i, c.name)
		}
		got, _ := json.Marshal(c.args)
		expected, _ := json.Marshal(want[i])
		if string(got) != string(expected) {
			t.Fatalf("call %d: arguments %s, want %s", i, got, expected)
		}
	}

	lines := "ssh on cedar.morgan.northwind.6net.io:22 — approved; your other machines can reach it\n" +
		"ssh on cedar.morgan.acme.6net.io:22 — declared; an acme admin must approve it (net_service_approve svc-acme)\n"
	if stdout != lines {
		t.Fatalf("stdout =\n%s\nwant\n%s", stdout, lines)
	}
	if strings.Contains(stdout+stderr, testBearer) {
		t.Fatal("the bearer appears in the output")
	}
}

func TestNetServeTakesAPortAfterTheKind(t *testing.T) {
	seen := serveWorld(t, []state.OrgStatus{northwindCedar}, func(_ *testing.T, c toolCall) string { return declared(c) })

	code, stdout, stderr := runNet("serve", "ssh", "--port", "2222")
	if code != exitcodes.OK {
		t.Fatalf("net serve ssh --port 2222 = %d:\n%s%s", code, stdout, stderr)
	}
	if len(*seen) != 1 || (*seen)[0].args["port"] != float64(2222) {
		t.Fatalf("calls %#v, want one on port 2222", *seen)
	}
	if !strings.HasPrefix(stdout, "ssh on cedar.morgan.northwind.6net.io:2222 — approved") {
		t.Fatalf("stdout = %q", stdout)
	}
}

// Every refusal that needs no answer from the mothership comes before asking
// it anything.
func TestNetServeRefusesBeforeAnyRequest(t *testing.T) {
	cases := []struct {
		name  string
		orgs  []state.OrgStatus
		setup func(t *testing.T)
		args  []string
		code  int
		say   string
	}{
		{"not joined", nil, nil, []string{"serve", "ssh"}, exitcodes.Usage, invoked + " net join"},
		{"joined nowhere", []state.OrgStatus{}, nil, []string{"serve", "ssh"}, exitcodes.Usage, invoked + " net join"},
		{"another kind", []state.OrgStatus{northwindCedar}, nil, []string{"serve", "db"}, exitcodes.Usage, `"db"`},
		{"no kind", []state.OrgStatus{northwindCedar}, nil, []string{"serve"}, exitcodes.Usage, invoked + " net serve ssh"},
		{"a bad port", []state.OrgStatus{northwindCedar}, nil, []string{"serve", "ssh", "--port", "0"}, exitcodes.Usage, "--port"},
		{"two kinds", []state.OrgStatus{northwindCedar}, nil, []string{"serve", "ssh", "ssh"}, exitcodes.Usage, "unexpected argument"},
		{"under sudo", []state.OrgStatus{northwindCedar}, func(t *testing.T) {
			asUID(t, 0)
			swap(t, &sudoUser, func() string { return "morgan" })
		}, []string{"serve", "ssh"}, exitcodes.Usage, "without sudo"},
		{"unserve under sudo", []state.OrgStatus{northwindCedar}, func(t *testing.T) {
			asUID(t, 0)
			swap(t, &sudoUser, func() string { return "morgan" })
		}, []string{"unserve", "ssh"}, exitcodes.Usage, "without sudo"},
		{"unserve another kind", []state.OrgStatus{northwindCedar}, nil, []string{"unserve", "api"}, exitcodes.Usage, `"api"`},
		{"logged out", []state.OrgStatus{northwindCedar}, func(t *testing.T) {
			t.Setenv("GENTILITY_STATE_DIR", t.TempDir())
		}, []string{"serve", "ssh"}, exitcodes.Failure, invoked + " login"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			seen := serveWorld(t, c.orgs, func(t *testing.T, call toolCall) string {
				t.Errorf("unexpected call %#v", call)
				return `{"success":false,"error":"no"}`
			})
			if c.setup != nil {
				c.setup(t)
			}

			code, stdout, stderr := runNet(c.args...)
			if code != c.code {
				t.Fatalf("net %q = %d, want %d:\n%s%s", c.args, code, c.code, stdout, stderr)
			}
			if !strings.Contains(stderr, c.say) {
				t.Fatalf("stderr = %q, want it to say %q", stderr, c.say)
			}
			if len(*seen) != 0 {
				t.Fatalf("the host saw %#v, want nothing", *seen)
			}
		})
	}
}

// A refusal in one organization is that organization's; the others are still
// asked, and the run fails having said why.
func TestNetServeAToolRefusalExitsOneWithItsReason(t *testing.T) {
	seen := serveWorld(t, []state.OrgStatus{northwindCedar, acmeCedar}, func(_ *testing.T, c toolCall) string {
		if c.args["organization"] == "northwind" {
			return `{"success":false,"error":"That machine is not yours."}`
		}
		return declared(c)
	})

	code, stdout, stderr := runNet("serve", "ssh")
	if code != exitcodes.Failure {
		t.Fatalf("net serve ssh = %d, want 1:\n%s%s", code, stdout, stderr)
	}
	if !strings.Contains(stderr, "northwind") || !strings.Contains(stderr, "That machine is not yours.") {
		t.Fatalf("stderr = %q, want the organization and the tool's reason", stderr)
	}
	if len(*seen) != 2 || !strings.Contains(stdout, "cedar.morgan.acme.6net.io:22 — declared") {
		t.Fatalf("calls %d, stdout %q; want acme asked and answered", len(*seen), stdout)
	}
}

func TestNetUnserveWithdrawsSshInEveryOrganization(t *testing.T) {
	seen := serveWorld(t, []state.OrgStatus{northwindCedar, acmeCedar}, func(_ *testing.T, c toolCall) string {
		return `{"success":true,"service":{"id":"svc","kind":"ssh","port":22,"state":"removed"}}`
	})

	code, stdout, stderr := runNet("unserve", "ssh")
	if code != exitcodes.OK {
		t.Fatalf("net unserve ssh = %d:\n%s%s", code, stdout, stderr)
	}
	want := []map[string]any{
		{"organization": "northwind", "node": "fd06:6e7:0:2::5", "kind": "ssh", "port": float64(22)},
		{"organization": "acme", "node": "fd06:6e7:0:9::3", "kind": "ssh", "port": float64(22)},
	}
	if len(*seen) != len(want) {
		t.Fatalf("the host saw %d calls, want %d", len(*seen), len(want))
	}
	for i, c := range *seen {
		got, _ := json.Marshal(c.args)
		expected, _ := json.Marshal(want[i])
		if c.name != "net_service_remove" || string(got) != string(expected) {
			t.Fatalf("call %d: %s %s, want net_service_remove %s", i, c.name, got, expected)
		}
	}
	lines := "ssh on cedar.morgan.northwind.6net.io:22 withdrawn\nssh on cedar.morgan.acme.6net.io:22 withdrawn\n"
	if stdout != lines {
		t.Fatalf("stdout =\n%s\nwant\n%s", stdout, lines)
	}
}

// Withdrawing what is already withdrawn has the outcome asked for: it says so
// and succeeds, rather than failing a second `net unserve`.
func TestNetUnserveOfNothingServedSucceedsSayingSo(t *testing.T) {
	var messages struct {
		NotServed []string `json:"not_served"`
	}
	if err := json.Unmarshal(contracttest.Read(t, "connector/messages.json"), &messages); err != nil {
		t.Fatal(err)
	}
	if len(messages.NotServed) == 0 {
		t.Fatal("server contract has no not-served messages")
	}
	for _, message := range messages.NotServed {
		t.Run(message, func(t *testing.T) {
			serveWorld(t, []state.OrgStatus{northwindCedar}, func(*testing.T, toolCall) string {
				body, err := json.Marshal(map[string]any{"success": false, "error": message})
				if err != nil {
					t.Fatal(err)
				}
				return string(body)
			})
			code, stdout, stderr := runNet("unserve", "ssh")
			if code != exitcodes.OK {
				t.Fatalf("net unserve ssh = %d, want 0:\n%s%s", code, stdout, stderr)
			}
			if stdout != "ssh was not being served on cedar.morgan.northwind.6net.io:22\n" || stderr != "" {
				t.Fatalf("stdout %q, stderr %q", stdout, stderr)
			}
		})
	}
}

// An organization the daemon lists without an address yet is not asked with
// an empty node; it is named, the others are still served, and the run fails.
func TestNetServeSkipsAnOrganizationWithNoAddressYet(t *testing.T) {
	pending := state.OrgStatus{Org: "acme"}
	for _, verb := range []string{"serve", "unserve"} {
		t.Run(verb, func(t *testing.T) {
			seen := serveWorld(t, []state.OrgStatus{northwindCedar, pending}, func(_ *testing.T, c toolCall) string {
				if verb == "serve" {
					return declared(c)
				}
				return `{"success":true,"service":{"id":"svc","kind":"ssh","port":22,"state":"removed"}}`
			})

			code, stdout, stderr := runNet(verb, "ssh")
			if code != exitcodes.Failure {
				t.Fatalf("net %s ssh = %d, want 1:\n%s%s", verb, code, stdout, stderr)
			}
			if len(*seen) != 1 || (*seen)[0].args["organization"] != "northwind" {
				t.Fatalf("calls %#v, want northwind alone", *seen)
			}
			if !strings.Contains(stdout, "cedar.morgan.northwind.6net.io:22") {
				t.Fatalf("stdout = %q, want northwind answered", stdout)
			}
			if !strings.Contains(stderr, "acme") || !strings.Contains(stderr, "address there yet") {
				t.Fatalf("stderr = %q, want acme named as not yet addressed", stderr)
			}
		})
	}
}

func TestNetUnserveAnotherRefusalExitsOne(t *testing.T) {
	serveWorld(t, []state.OrgStatus{northwindCedar}, func(*testing.T, toolCall) string {
		return `{"success":false,"error":"That machine is not yours."}`
	})

	code, _, stderr := runNet("unserve", "ssh")
	if code != exitcodes.Failure || !strings.Contains(stderr, "That machine is not yours.") {
		t.Fatalf("net unserve ssh = %d, stderr %q; want 1 with the reason", code, stderr)
	}
}

// An expired session is the same answer in every organization, so it is said
// once and the rest are not asked.
func TestNetServeStopsAtAnExpiredSession(t *testing.T) {
	serveWorld(t, []state.OrgStatus{northwindCedar, acmeCedar}, func(*testing.T, toolCall) string { return "" })
	requests := new(atomic.Int32)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("WWW-Authenticate", `Bearer realm="mcp"`)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(srv.Close)
	signIn(t, srv.URL, "stale")

	code, _, stderr := runNet("serve", "ssh")
	if code != exitcodes.Failure || !strings.Contains(stderr, "expired") || strings.Count(stderr, "login`") != 1 {
		t.Fatalf("net serve ssh = %d, stderr %q; want 1 naming login once", code, stderr)
	}
	if n := requests.Load(); n != 1 {
		t.Fatalf("%d requests, want 1", n)
	}
}
