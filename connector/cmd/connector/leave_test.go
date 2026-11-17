package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gentility-ai/gentility/connector/internal/exitcodes"
	"github.com/gentility-ai/gentility/connector/internal/state"
	"github.com/gentility-ai/gentility/connector/internal/supervise"
)

// recorder is the Runner leave is handed in a test: it writes down what it was
// asked to run and runs nothing, so no test here ever reaches launchctl or
// systemctl. fail, when set, is every command's answer.
type recorder struct {
	calls []string
	fail  error
}

func (r *recorder) Run(_ context.Context, name string, args ...string) error {
	r.calls = append(r.calls, strings.Join(append([]string{name}, args...), " "))
	return r.fail
}

// cleanUps swaps cleanUpInterface for a counter, so no test here touches this
// machine's resolver, and answers how many times it ran.
func cleanUps(t *testing.T) *int {
	t.Helper()
	n := new(int)
	previous := cleanUpInterface
	cleanUpInterface = func() error { *n++; return nil }
	t.Cleanup(func() { cleanUpInterface = previous })
	return n
}

// asUID makes the command believe it runs as uid.
func asUID(t *testing.T, uid int) {
	t.Helper()
	previous := geteuid
	geteuid = func() int { return uid }
	t.Cleanup(func() { geteuid = previous })
}

// status reads the directory it was told to; otherwise the daemon's, when one
// is installed, because that is where this machine's identity is; otherwise
// this user's, which is where `login` left it.
func TestStatusDirChoosesTheDaemonsDirectoryWhenOneIsInstalled(t *testing.T) {
	t.Setenv("GENTILITY_STATE_DIR", "/home/someone/.config/gentility")

	cases := []struct {
		installed bool
		explicit  string
		want      string
	}{
		{installed: true, explicit: "", want: supervise.DefaultStateDir},
		{installed: false, explicit: "", want: "/home/someone/.config/gentility"},
		{installed: true, explicit: "/tmp/elsewhere", want: "/tmp/elsewhere"},
		{installed: false, explicit: "/tmp/elsewhere", want: "/tmp/elsewhere"},
	}
	for _, c := range cases {
		if got := statusDir(c.installed, c.explicit); got != c.want {
			t.Errorf("statusDir(%v, %q) = %q, want %q", c.installed, c.explicit, got, c.want)
		}
	}
}

// leaveOptions is a darwin machine under a temporary root, with the daemon's
// state directory inside it.
func leaveOptions(t *testing.T, out *bytes.Buffer) supervise.Options {
	t.Helper()
	root := t.TempDir()
	return supervise.Options{
		GOOS:     "darwin",
		Root:     root,
		Label:    supervise.DefaultLabel,
		StateDir: filepath.Join(root, "var", "lib", "gentility"),
		Out:      out,
	}
}

// installed puts down what an install leaves: the unit, and an identity with
// tailscale's state beside it and an enrollment staged to join on.
func installed(t *testing.T, opts supervise.Options) string {
	t.Helper()
	plist := filepath.Join(opts.Root, "Library", "LaunchDaemons", opts.Label+".plist")
	if err := os.MkdirAll(filepath.Dir(plist), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(plist, []byte("<plist/>"), 0o644); err != nil {
		t.Fatal(err)
	}
	store := &state.Store{Dir: opts.StateDir}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"revoked"}`))
	}))
	t.Cleanup(server.Close)
	if err := store.SaveNode(state.Node{Origin: server.URL, NodeID: "node-1", NodeCredential: "secret"}); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(opts.StateDir, state.TSNetDir), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveEnrollment(state.Enrollment{EnrollmentID: "enr-1", PreauthKey: "tskey-auth-staged"}); err != nil {
		t.Fatal(err)
	}
	return plist
}

// leave stops the daemon, removes its unit and forgets this machine's
// identity — and a second leave finds nothing to do and changes nothing.
func TestLeaveStopsTheDaemonAndForgetsTheIdentityOnce(t *testing.T) {
	cleaned := cleanUps(t)
	var out bytes.Buffer
	opts := leaveOptions(t, &out)
	plist := installed(t, opts)

	runner := &recorder{}
	if err := forget(context.Background(), opts, runner); err != nil {
		t.Fatalf("forget: %v", err)
	}

	if want := "launchctl bootout system/" + opts.Label; len(runner.calls) != 1 || runner.calls[0] != want {
		t.Fatalf("ran %v, want [%s]", runner.calls, want)
	}
	for _, gone := range []string{
		plist,
		filepath.Join(opts.StateDir, "node.json"),
		filepath.Join(opts.StateDir, state.TSNetDir),
		filepath.Join(opts.StateDir, "enrollment.json"),
	} {
		if _, err := os.Stat(gone); !os.IsNotExist(err) {
			t.Fatalf("%s survived leave: %v", gone, err)
		}
	}
	if strings.Contains(out.String(), "until an admin revokes it") {
		t.Fatalf("leave falsely reports pending server revocation:\n%s", out.String())
	}
	if *cleaned != 0 {
		t.Fatalf("a userspace daemon's leave ran the interface cleanup %d times", *cleaned)
	}

	out.Reset()
	again := &recorder{}
	if err := forget(context.Background(), opts, again); err != nil {
		t.Fatalf("second forget: %v", err)
	}
	if len(again.calls) != 0 {
		t.Fatalf("a second leave ran %v", again.calls)
	}
	if strings.TrimSpace(out.String()) != "no daemon is installed on this machine" {
		t.Fatalf("second leave said:\n%s", out.String())
	}
}

// A leave that removed the unit and then failed has left the identity behind
// it. The next leave finishes the job rather than reporting that there is
// nothing to do.
func TestLeaveFinishesALeaveThatStoppedHalfway(t *testing.T) {
	var out bytes.Buffer
	opts := leaveOptions(t, &out)
	plist := installed(t, opts)
	if err := os.Remove(plist); err != nil {
		t.Fatal(err)
	}

	runner := &recorder{}
	if err := forget(context.Background(), opts, runner); err != nil {
		t.Fatalf("forget: %v", err)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("with no unit, ran %v", runner.calls)
	}
	if (&state.Store{Dir: opts.StateDir}).HoldsMachine() {
		t.Fatalf("the identity survived")
	}
	if !strings.Contains(out.String(), "left 6net") {
		t.Fatalf("leave said:\n%s", out.String())
	}
}

// With neither a daemon nor an identity, leave says so and changes nothing —
// not even the directory.
func TestLeaveWithNothingToForgetChangesNothing(t *testing.T) {
	var out bytes.Buffer
	opts := leaveOptions(t, &out)

	runner := &recorder{}
	if err := forget(context.Background(), opts, runner); err != nil {
		t.Fatalf("forget: %v", err)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("ran %v", runner.calls)
	}
	if strings.TrimSpace(out.String()) != "no daemon is installed on this machine" {
		t.Fatalf("leave said:\n%s", out.String())
	}
	if _, err := os.Stat(opts.StateDir); !os.IsNotExist(err) {
		t.Fatalf("leave touched %s: %v", opts.StateDir, err)
	}
}

// An interface daemon stopped by leave owes the resolver cleanup
// systemd would run from ExecStopPost and launchd has no hook for.
func TestLeaveCleansUpAfterAnInterfaceDaemon(t *testing.T) {
	cleaned := cleanUps(t)
	var out bytes.Buffer
	opts := leaveOptions(t, &out)
	plist := installed(t, opts)
	if err := os.WriteFile(plist, []byte("<plist><string>--interface</string></plist>"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := forget(context.Background(), opts, &recorder{}); err != nil {
		t.Fatalf("forget: %v", err)
	}
	if *cleaned != 1 {
		t.Fatalf("the interface cleanup ran %d times, want 1", *cleaned)
	}
}

// A stop the supervisor refused may have left the daemon running, so leave
// keeps the identity, says what the supervisor said, and exits non-zero.
func TestLeaveKeepsTheIdentityWhenTheStopFails(t *testing.T) {
	cleaned := cleanUps(t)
	var out bytes.Buffer
	opts := leaveOptions(t, &out)
	plist := installed(t, opts)
	if err := os.WriteFile(plist, []byte("<plist><string>--interface</string></plist>"), 0o644); err != nil {
		t.Fatal(err)
	}

	err := forget(context.Background(), opts, &recorder{fail: errors.New("Boot-out failed: 5: Input/output error")})

	if err == nil || !strings.Contains(err.Error(), "Boot-out failed: 5") {
		t.Fatalf("forget = %v, want the supervisor's note", err)
	}
	if !(&state.Store{Dir: opts.StateDir}).HoldsMachine() {
		t.Fatal("the identity was forgotten after a failed stop")
	}
	if strings.Contains(out.String(), "left 6net") {
		t.Fatalf("leave claimed success:\n%s", out.String())
	}
	if *cleaned != 0 {
		t.Fatalf("the cleanup ran after a failed stop")
	}
}

// The same through the command: the note reaches stderr and the exit is 1.
// Root is the geteuid seam; the daemon is a recorder over a temporary root,
// through the forget seam's own test, so nothing here reaches this machine.
func TestRunLeaveReportsAFailedStop(t *testing.T) {
	asUID(t, 0)
	cleanUps(t)
	var out bytes.Buffer
	opts := leaveOptions(t, &out)
	installed(t, opts)
	previous := leaveWith
	leaveWith = func(ctx context.Context, stdout, stderr io.Writer) error {
		opts.Out = stdout
		return forget(ctx, opts, &recorder{fail: errors.New("Boot-out failed: 5: Input/output error")})
	}
	t.Cleanup(func() { leaveWith = previous })

	var stdout, stderr bytes.Buffer
	if code := run([]string{"leave"}, &stdout, &stderr); code != 1 {
		t.Fatalf("run(leave) = %d, want 1:\n%s%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "Boot-out failed: 5") || !strings.Contains(stderr.String(), "identity is kept") {
		t.Fatalf("stderr does not carry the note: %q", stderr.String())
	}
	if !(&state.Store{Dir: opts.StateDir}).HoldsMachine() {
		t.Fatal("the identity was forgotten after a failed stop")
	}
}

// And as root with a stop that works, run leave ends at exit 0 with the
// identity gone.
func TestRunLeaveAsRootForgetsTheMachine(t *testing.T) {
	asUID(t, 0)
	cleanUps(t)
	var out bytes.Buffer
	opts := leaveOptions(t, &out)
	installed(t, opts)
	previous := leaveWith
	leaveWith = func(ctx context.Context, stdout, stderr io.Writer) error {
		opts.Out = stdout
		return forget(ctx, opts, &recorder{})
	}
	t.Cleanup(func() { leaveWith = previous })

	var stdout, stderr bytes.Buffer
	if code := run([]string{"leave"}, &stdout, &stderr); code != exitcodes.OK {
		t.Fatalf("run(leave) = %d, want 0:\n%s%s", code, stdout.String(), stderr.String())
	}
	if (&state.Store{Dir: opts.StateDir}).HoldsMachine() {
		t.Fatal("the identity survived leave")
	}
	if !strings.Contains(stdout.String(), "left 6net") {
		t.Fatalf("leave said:\n%s", stdout.String())
	}
}

// cleanup touches root's configuration, so without root it is a usage error
// and runs nothing.
func TestRunCleanupWithoutRootIsAUsageError(t *testing.T) {
	asUID(t, 501)
	cleaned := cleanUps(t)

	var stdout, stderr bytes.Buffer
	if code := run([]string{"cleanup", "--interface"}, &stdout, &stderr); code != exitcodes.Usage {
		t.Fatalf("run(cleanup --interface) = %d, want %d", code, exitcodes.Usage)
	}
	if !strings.Contains(stderr.String(), "sudo "+invoked+" cleanup --interface") {
		t.Fatalf("stderr does not say to use sudo: %q", stderr.String())
	}
	if *cleaned != 0 {
		t.Fatal("cleanup ran without root")
	}
}

// As root it runs the engine's cleanup once.
func TestRunCleanupAsRootRunsTheCleanup(t *testing.T) {
	asUID(t, 0)
	cleaned := cleanUps(t)

	var stdout, stderr bytes.Buffer
	if code := run([]string{"cleanup", "--interface"}, &stdout, &stderr); code != exitcodes.OK {
		t.Fatalf("run(cleanup --interface) = %d, want 0: %s", code, stderr.String())
	}
	if *cleaned != 1 {
		t.Fatalf("cleanup ran %d times, want 1", *cleaned)
	}
}

// The daemon and its identity are root's, so a person who forgot sudo is told
// to use it, with the exit code that means "fix the command".
func TestRunLeaveWithoutRootIsAUsageError(t *testing.T) {
	asUID(t, 501)

	var stdout, stderr bytes.Buffer
	if code := run([]string{"leave"}, &stdout, &stderr); code != exitcodes.Usage {
		t.Fatalf("run(leave) = %d, want %d:\n%s%s", code, exitcodes.Usage, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "sudo "+invoked+" leave") {
		t.Fatalf("stderr does not say to use sudo: %q", stderr.String())
	}
}
