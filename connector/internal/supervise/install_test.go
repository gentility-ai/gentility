package supervise

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/gentility-ai/gentility/connector/internal/enroll"
	"github.com/gentility-ai/gentility/connector/internal/state"
)

// fakeRunner is the exec seam standing still: it records the argv it was
// handed and fails whichever command the test says is missing. No test in this
// package ever runs launchctl, systemctl or useradd — most machines have at
// most one of them, and none of them belong in a unit test.
//
// It stands in for launchd's memory too: `launchctl print` fails, as it does
// for a label launchd has not loaded, unless the test says loaded.
type fakeRunner struct {
	calls  [][]string
	fail   map[string]error
	loaded bool
	// printed is what `launchctl print` answers for a loaded label — launchd's
	// page about the job, of which Install reads the job state alone.
	printed string
	// lingers is how many `launchctl print`s still find the label after a
	// bootout. launchd's bootout returns while the job it stopped is still
	// exiting, and until it has, launchd holds the label and refuses to
	// bootstrap it ("Bootstrap failed: 5: Input/output error").
	lingers int
}

func (f *fakeRunner) Run(_ context.Context, name string, args ...string) error {
	f.calls = append(f.calls, append([]string{name}, args...))
	if name != "launchctl" || len(args) == 0 {
		return f.fail[name]
	}
	switch args[0] {
	case "bootout":
		if f.loaded && f.lingers == 0 {
			f.loaded = false
		}
	case "print":
		if f.loaded && f.lingers > 0 && f.booted() {
			f.lingers--
			if f.lingers == 0 {
				f.loaded = false
			}
			return nil
		}
		if !f.loaded {
			return errors.New("Could not find service in domain for port")
		}
	case "bootstrap":
		if f.loaded && f.booted() {
			return errors.New("Bootstrap failed: 5: Input/output error")
		}
	}
	return f.fail[name]
}

// booted says whether a bootout has run.
func (f *fakeRunner) booted() bool {
	for _, call := range f.calls {
		if len(call) > 1 && call[0] == "launchctl" && call[1] == "bootout" {
			return true
		}
	}
	return false
}

func (f *fakeRunner) Output(ctx context.Context, name string, args ...string) (string, error) {
	err := f.Run(ctx, name, args...)
	return f.printed, err
}

// argv renders the calls as one line each, which is what the assertions read.
func (f *fakeRunner) argv() []string {
	lines := make([]string, 0, len(f.calls))
	for _, call := range f.calls {
		lines = append(lines, strings.Join(call, " "))
	}
	return lines
}

func (f *fakeRunner) wants(t *testing.T, want ...string) {
	t.Helper()
	got := f.argv()
	if len(got) != len(want) {
		t.Fatalf("ran %d commands, want %d:\n got %v\nwant %v", len(got), len(want), got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("command %d = %q, want %q\n got %v\nwant %v", i, got[i], want[i], got, want)
		}
	}
}

// now is the instant every install in this file happens at. The enrollments
// below date themselves against it, so a test that means "expired" says so in
// the fixture rather than in the wall clock of whoever runs the suite.
var now = time.Date(2026, 9, 6, 11, 0, 0, 0, time.UTC)

// options is an installation whose every path is inside the test's own
// temporary directory, including the filesystem root the unit file lands
// under.
func options(t *testing.T, goos string) Options {
	t.Helper()
	dir := t.TempDir()
	return Options{
		GOOS:           goos,
		Root:           filepath.Join(dir, "root"),
		BinaryPath:     "/usr/local/bin/connector",
		StateDir:       filepath.Join(dir, "state"),
		LogDir:         filepath.Join(dir, "log"),
		SourceStateDir: filepath.Join(dir, "person"),
		Out:            io.Discard,
		Now:            func() time.Time { return now },
		// No join environment unless a test says so: an install that read the
		// shell it was run from would be an install whose tests pass or fail on
		// whether somebody had sourced an overlay recipe.
		LookupEnv: environment(nil),
		// Not root, whoever runs the suite: what root's install refuses is
		// asked for by name, by the tests that set Euid to 0.
		Euid: func() int { return 1000 },
	}
}

// homes is the passwd database standing still: a lookup that answers for the
// accounts a test invented and refuses every other name. No test in this
// package asks the machine it runs on who alice is.
func homes(m map[string]string) func(string) (*user.User, error) {
	return func(name string) (*user.User, error) {
		home, ok := m[name]
		if !ok {
			return nil, user.UnknownUserError(name)
		}
		return &user.User{Username: name, HomeDir: home}, nil
	}
}

func enrollment() state.Enrollment {
	return state.Enrollment{
		EnrollmentID: "enr_01",
		PreauthKey:   "tskey-auth-secret",
		Nonce:        "nonce-01",
		HeadURL:      "https://head.example",
		ExpiresAt:    "2026-09-06T12:00:00Z",
	}
}

// expiredEnrollment is what a preauth key's ten minutes look like an hour
// later: the same shape, a different id so an assertion can tell which one
// survived, and a fuse that burnt down before `now`.
func expiredEnrollment() state.Enrollment {
	e := enrollment()
	e.EnrollmentID = "enr_00"
	e.ExpiresAt = "2026-09-06T10:00:00Z"
	return e
}

func seedEnrollment(t *testing.T, dir string) {
	t.Helper()
	save(t, dir, enrollment())
}

func save(t *testing.T, dir string, e state.Enrollment) {
	t.Helper()
	if err := (&state.Store{Dir: dir}).SaveEnrollment(e); err != nil {
		t.Fatalf("seeding the enrollment in %s: %v", dir, err)
	}
}

func loadEnrollment(t *testing.T, dir string) state.Enrollment {
	t.Helper()
	e, err := (&state.Store{Dir: dir}).LoadEnrollment()
	if err != nil {
		t.Fatalf("LoadEnrollment from %s: %v", dir, err)
	}
	return e
}

// environment is the join environment standing still: the OVERLAY_* variables
// as a lookup rather than as the process's own environment, because a test that
// wrote the process environment would be a test every other test in the package
// raced with.
func environment(vars map[string]string) func(string) (string, bool) {
	return func(name string) (string, bool) {
		value, ok := vars[name]
		return value, ok
	}
}

// gatewayCA is the internal CA an operator hands over beside the trio. It is a
// public certificate rather than a secret, and nothing in an install parses it
// — OVERLAY_GATEWAY_CA names a file and the bytes are carried through — so
// these are bytes and not a certificate.
const gatewayCA = "-----BEGIN CERTIFICATE-----\nnot a certificate, only bytes\n-----END CERTIFICATE-----\n"

// minted is what `net_machine_enroll` returns, as the unattended recipe
// exports it: the mint's payload in OVERLAY_* variables, with the CA in a file
// because that is what the variable names.
func minted(t *testing.T) map[string]string {
	t.Helper()
	ca := filepath.Join(t.TempDir(), "gateway-ca.pem")
	if err := os.WriteFile(ca, []byte(gatewayCA), 0o644); err != nil {
		t.Fatalf("writing the CA: %v", err)
	}
	return map[string]string{
		enroll.VarEnrollmentID: "enr_env",
		enroll.VarNonce:        "nonce-env",
		enroll.VarPreauthKey:   "tskey-auth-env",
		enroll.VarHeadURL:      "https://head.example",
		enroll.VarGatewayAddr:  "[fd06:6e7:0:1::9]:443",
		enroll.VarGatewayCA:    ca,
	}
}

// mintedEnrollment is what minted() has to become once it is staged: the
// address and the port apart, the CA as its PEM, and a fuse ten minutes out
// from `now` because the environment did not say.
func mintedEnrollment() state.Enrollment {
	return state.Enrollment{
		EnrollmentID: "enr_env",
		PreauthKey:   "tskey-auth-env",
		Nonce:        "nonce-env",
		HeadURL:      "https://head.example",
		ExpiresAt:    "2026-09-06T11:10:00Z",
		GatewayAddr:  "fd06:6e7:0:1::9",
		GatewayPort:  443,
		GatewayCA:    gatewayCA,
	}
}

// exchanged is a machine that already holds an identity, with both halves of it
// on disk: the credential that is ours and the node key that is tsnet's.
func exchanged(t *testing.T, dir string) state.Node {
	t.Helper()
	node := state.Node{
		NodeID:         "node_01",
		NodeCredential: "cred",
		LastRevision:   7,
		HeadURL:        "https://head.example",
		Hostname:       "shipyard",
	}
	store := &state.Store{Dir: dir}
	if err := store.SaveNode(node); err != nil {
		t.Fatalf("SaveNode: %v", err)
	}
	if err := store.SaveStatus(state.Status{WrittenAt: now, Revision: 7}); err != nil {
		t.Fatalf("SaveStatus: %v", err)
	}
	tsnet := filepath.Join(dir, state.TSNetDir)
	if err := os.MkdirAll(tsnet, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tsnet, "tailscaled.state"), []byte(`{"_machinekey":"…"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	return node
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

func TestInstallOnDarwinWritesThePlistAndBootstrapsIt(t *testing.T) {
	opts := options(t, "darwin")
	seedEnrollment(t, opts.SourceStateDir)
	runner := &fakeRunner{}

	var out bytes.Buffer
	opts.Out = &out
	if err := Install(context.Background(), opts, runner); err != nil {
		t.Fatalf("Install: %v", err)
	}

	plist := filepath.Join(opts.Root, "Library", "LaunchDaemons", "ai.gentility.connector.plist")
	want, err := RenderLaunchd(Unit{
		BinaryPath: opts.BinaryPath,
		StateDir:   opts.StateDir,
		Label:      DefaultLabel,
		LogDir:     opts.LogDir,
	})
	if err != nil {
		t.Fatalf("RenderLaunchd: %v", err)
	}
	if got := read(t, plist); got != want {
		t.Fatalf("plist =\n%s\nwant\n%s", got, want)
	}

	runner.wants(t,
		"launchctl print system/ai.gentility.connector",
		"launchctl bootstrap system "+plist,
		"launchctl kickstart -k system/ai.gentility.connector",
	)

	if info, err := os.Stat(opts.LogDir); err != nil || !info.IsDir() {
		t.Fatalf("log dir %s: %v", opts.LogDir, err)
	}
	if !strings.Contains(out.String(), plist) {
		t.Fatalf("summary does not name the unit it wrote:\n%s", out.String())
	}
}

// The systemd unit names User=gentility, so installing it is also promising
// that user exists. getent is the question and useradd is the answer; chown
// hands the daemon the two directories it must write.
func TestInstallOnLinuxCreatesTheServiceUserThenEnablesTheUnit(t *testing.T) {
	opts := options(t, "linux")
	seedEnrollment(t, opts.SourceStateDir)
	runner := &fakeRunner{fail: map[string]error{"getent": errors.New("no such user")}}

	if err := Install(context.Background(), opts, runner); err != nil {
		t.Fatalf("Install: %v", err)
	}

	unit := filepath.Join(opts.Root, "etc", "systemd", "system", "gentility-connector.service")
	want, err := RenderSystemd(Unit{
		BinaryPath: opts.BinaryPath,
		StateDir:   opts.StateDir,
		Label:      DefaultLabel,
		LogDir:     opts.LogDir,
	})
	if err != nil {
		t.Fatalf("RenderSystemd: %v", err)
	}
	if got := read(t, unit); got != want {
		t.Fatalf("unit =\n%s\nwant\n%s", got, want)
	}

	runner.wants(t,
		"getent passwd gentility",
		"useradd --system --user-group --no-create-home --shell /usr/sbin/nologin gentility",
		"chown -R gentility:gentility "+opts.StateDir+" "+opts.LogDir,
		"systemctl daemon-reload",
		"systemctl enable --now gentility-connector",
		"systemctl restart gentility-connector",
	)
}

// An existing service user is left alone: useradd on a user that is already
// there is an error, and an install that reports one is an install nobody
// trusts the second time.
func TestInstallOnLinuxLeavesAnExistingServiceUserAlone(t *testing.T) {
	opts := options(t, "linux")
	seedEnrollment(t, opts.SourceStateDir)
	runner := &fakeRunner{}

	if err := Install(context.Background(), opts, runner); err != nil {
		t.Fatalf("Install: %v", err)
	}

	for _, line := range runner.argv() {
		if strings.HasPrefix(line, "useradd") {
			t.Fatalf("useradd ran for a user getent already found:\n%v", runner.argv())
		}
	}
}

// An interface install runs the daemon as root, so there is no service user
// to promise: getent and useradd would be work done for an account the unit
// never names. The directories go to root, recursively, so a root daemon never
// writes into a directory another account owns.
func TestInstallOnLinuxWithAnInterfaceRunsAsRootAndMakesNoServiceUser(t *testing.T) {
	opts := options(t, "linux")
	opts.Interface = true
	seedEnrollment(t, opts.SourceStateDir)
	runner := &fakeRunner{fail: map[string]error{"getent": errors.New("no such user")}}

	if err := Install(context.Background(), opts, runner); err != nil {
		t.Fatalf("Install: %v", err)
	}

	unit := filepath.Join(opts.Root, "etc", "systemd", "system", "gentility-connector.service")
	want, err := RenderSystemd(Unit{
		BinaryPath: opts.BinaryPath,
		StateDir:   opts.StateDir,
		Label:      DefaultLabel,
		LogDir:     opts.LogDir,
		Interface:  true,
	})
	if err != nil {
		t.Fatalf("RenderSystemd: %v", err)
	}
	if got := read(t, unit); got != want {
		t.Fatalf("unit =\n%s\nwant\n%s", got, want)
	}

	runner.wants(t,
		"chown -R root:root "+opts.StateDir+" "+opts.LogDir,
		"systemctl daemon-reload",
		"systemctl enable --now gentility-connector",
		"systemctl restart gentility-connector",
	)
}

// writeUnit puts u's unit where opts's install would, as an earlier install
// would have left it.
func writeUnit(t *testing.T, opts Options, u Unit) string {
	t.Helper()
	p, err := platformFor(opts.withDefaults())
	if err != nil {
		t.Fatalf("platformFor: %v", err)
	}
	text, err := p.render(u)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(p.unitPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.unitPath, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	return p.unitPath
}

// unitOf is the unit opts's install renders.
func unitOf(opts Options) Unit {
	return Unit{
		BinaryPath: opts.BinaryPath,
		StateDir:   opts.StateDir,
		Label:      DefaultLabel,
		LogDir:     opts.LogDir,
		Interface:  opts.Interface,
	}
}

// An enrolled machine switched to --interface stages nothing, so nothing about
// its material asks for a restart. Its unit changed, though: the process
// systemd is running was started as the gentility user under ProtectSystem=
// strict, and it stays that way until it is started again.
func TestInstallOnLinuxSwitchingAnEnrolledMachineToAnInterfaceRestartsIt(t *testing.T) {
	opts := options(t, "linux")
	exchanged(t, opts.StateDir)
	path := writeUnit(t, opts, unitOf(opts))
	opts.Interface = true
	runner := &fakeRunner{}

	if err := Install(context.Background(), opts, runner); err != nil {
		t.Fatalf("Install: %v", err)
	}

	want, err := RenderSystemd(unitOf(opts))
	if err != nil {
		t.Fatalf("RenderSystemd: %v", err)
	}
	if got := read(t, path); got != want {
		t.Fatalf("unit =\n%s\nwant\n%s", got, want)
	}
	runner.wants(t,
		"chown -R root:root "+opts.StateDir+" "+opts.LogDir,
		"systemctl daemon-reload",
		"systemctl enable --now gentility-connector",
		"systemctl restart gentility-connector",
	)
}

// launchd holds on to the plist it bootstrapped — kickstart restarts the old
// definition — so a changed plist is booted out before the new one goes in.
func TestInstallOnDarwinSwitchingAnEnrolledMachineToAnInterfaceBootsTheOldPlistOut(t *testing.T) {
	opts := options(t, "darwin")
	exchanged(t, opts.StateDir)
	path := writeUnit(t, opts, unitOf(opts))
	opts.Interface = true
	runner := &fakeRunner{}

	if err := Install(context.Background(), opts, runner); err != nil {
		t.Fatalf("Install: %v", err)
	}

	want, err := RenderLaunchd(unitOf(opts))
	if err != nil {
		t.Fatalf("RenderLaunchd: %v", err)
	}
	if got := read(t, path); got != want || !strings.Contains(got, "<string>--interface</string>") {
		t.Fatalf("plist =\n%s\nwant\n%s", got, want)
	}
	runner.wants(t,
		"launchctl bootout system/ai.gentility.connector",
		"launchctl print system/ai.gentility.connector",
		"launchctl bootstrap system "+path,
		"launchctl kickstart -k system/ai.gentility.connector",
	)
}

// A reinstall that changes nothing drops nothing: the daemon is healthy, its
// connections are live, and nothing it was started on has moved.
func TestInstallReinstallingTheSameUnitRestartsNothing(t *testing.T) {
	for _, goos := range []string{"linux", "darwin"} {
		t.Run(goos, func(t *testing.T) {
			opts := options(t, goos)
			exchanged(t, opts.StateDir)
			writeUnit(t, opts, unitOf(opts))
			// An unchanged unit was installed before, so launchd has it loaded.
			runner := &fakeRunner{loaded: true}

			if err := Install(context.Background(), opts, runner); err != nil {
				t.Fatalf("Install: %v", err)
			}

			switch goos {
			case "linux":
				runner.wants(t,
					"getent passwd gentility",
					"chown -R gentility:gentility "+opts.StateDir+" "+opts.LogDir,
					"systemctl daemon-reload",
					"systemctl enable --now gentility-connector",
				)
			case "darwin":
				// Not bootstrap: launchd refuses to bootstrap a loaded label.
				runner.wants(t, "launchctl print system/ai.gentility.connector")
			}
		})
	}
}

// A darwin reinstall that staged material on a label launchd already holds
// restarts it with kickstart and does not bootstrap it again, which launchd
// would refuse.
func TestInstallOnDarwinRestartsALoadedLabelWithoutBootstrappingIt(t *testing.T) {
	opts := options(t, "darwin")
	writeUnit(t, opts, unitOf(opts))
	seedEnrollment(t, opts.SourceStateDir)
	runner := &fakeRunner{loaded: true}

	if err := Install(context.Background(), opts, runner); err != nil {
		t.Fatalf("Install: %v", err)
	}
	runner.wants(t,
		"launchctl print system/ai.gentility.connector",
		"launchctl kickstart -k system/ai.gentility.connector",
	)
}

// A label launchd holds but cannot spawn is not a healthy daemon to leave
// alone: after a binary swap under a job registered with Background Task
// Management, launchd can refuse every respawn ("job state = spawn failed",
// exit 78) until the label is booted out and bootstrapped again. A kickstart
// repeats the refusal, so a reinstall onto an unchanged unit does the bootout
// and bootstrap instead.
func TestInstallOnDarwinRebootstrapsALabelLaunchdCannotSpawn(t *testing.T) {
	opts := options(t, "darwin")
	exchanged(t, opts.StateDir)
	path := writeUnit(t, opts, unitOf(opts))
	runner := &fakeRunner{loaded: true, printed: "system/ai.gentility.connector = {\n\tstate = spawn scheduled\n\truns = 2151\n\tlast exit code = 78: EX_CONFIG\n\tjob state = spawn failed\n}\n"}
	var out strings.Builder
	opts.Out = &out

	if err := Install(context.Background(), opts, runner); err != nil {
		t.Fatalf("Install: %v", err)
	}
	runner.wants(t,
		"launchctl print system/ai.gentility.connector",
		"launchctl bootout system/ai.gentility.connector",
		"launchctl print system/ai.gentility.connector",
		"launchctl bootstrap system "+path,
	)
	if !strings.Contains(out.String(), "spawn failed") {
		t.Fatalf("output says nothing about the job launchd could not spawn:\n%s", out.String())
	}
}

// The probe is skipped when the unit changed: the bootout before it has
// unloaded the label, so the bootstrap is owed whatever launchd held.
func TestInstallOnDarwinBootstrapsAChangedUnitEvenWhenLoaded(t *testing.T) {
	opts := options(t, "darwin")
	exchanged(t, opts.StateDir)
	path := writeUnit(t, opts, unitOf(opts))
	opts.Interface = true
	runner := &fakeRunner{loaded: true}

	if err := Install(context.Background(), opts, runner); err != nil {
		t.Fatalf("Install: %v", err)
	}
	runner.wants(t,
		"launchctl bootout system/ai.gentility.connector",
		"launchctl print system/ai.gentility.connector",
		"launchctl bootstrap system "+path,
		"launchctl kickstart -k system/ai.gentility.connector",
	)
}

// launchd's bootout returns while the job it stopped is still exiting, and a
// bootstrap in that window fails with exit 5 and leaves the label unloaded:
// no daemon, and nothing to bring one back. On 2026-11-10 a Connector update
// that changed the plist did exactly that to a Mac (T-450). Install waits for
// launchd to let go of the label before bootstrapping it.
func TestInstallOnDarwinWaitsForTheBootedOutJobBeforeBootstrapping(t *testing.T) {
	opts := options(t, "darwin")
	exchanged(t, opts.StateDir)
	path := writeUnit(t, opts, unitOf(opts))
	opts.Interface = true
	runner := &fakeRunner{loaded: true, lingers: 2}

	if err := Install(context.Background(), opts, runner); err != nil {
		t.Fatalf("Install: %v", err)
	}
	runner.wants(t,
		"launchctl bootout system/ai.gentility.connector",
		"launchctl print system/ai.gentility.connector",
		"launchctl print system/ai.gentility.connector",
		"launchctl print system/ai.gentility.connector",
		"launchctl bootstrap system "+path,
		"launchctl kickstart -k system/ai.gentility.connector",
	)
}

// `connector login --server` writes the enrollment into the person's state
// directory; the daemon runs against its own. Install is the only moment that
// can carry it across, and an install that does not is an install whose
// service comes up refusing to serve.
func TestInstallMovesTheEnrollmentIntoTheDaemonsStateDir(t *testing.T) {
	opts := options(t, "darwin")
	seedEnrollment(t, opts.SourceStateDir)

	if err := Install(context.Background(), opts, &fakeRunner{}); err != nil {
		t.Fatalf("Install: %v", err)
	}

	moved, err := (&state.Store{Dir: opts.StateDir}).LoadEnrollment()
	if err != nil {
		t.Fatalf("LoadEnrollment from the daemon's state dir: %v", err)
	}
	if moved != enrollment() {
		t.Fatalf("enrollment = %+v, want %+v", moved, enrollment())
	}

	if _, err := (&state.Store{Dir: opts.SourceStateDir}).LoadEnrollment(); !state.IsNoEnrollment(err) {
		t.Fatalf("the source still holds the enrollment: err = %v", err)
	}

	// One hour of two secrets deserves the same mode it was written with.
	info, err := os.Stat(filepath.Join(opts.StateDir, "enrollment.json"))
	if err != nil {
		t.Fatalf("stat the moved enrollment: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("moved enrollment is %v, want 0600", info.Mode().Perm())
	}
}

// A preauth key lives ten minutes. If the join did not happen inside them,
// `serve` says to log in again — and the documented recovery is exactly
// `connector login --server` then `sudo connector install`. An install that
// saw the dead material already staged and stopped there would keep the
// corpse, ignore the material the person just earned, and leave the service
// unable to join for good.
func TestInstallReplacesAnExpiredStagedEnrollmentWithTheFreshOne(t *testing.T) {
	opts := options(t, "darwin")
	save(t, opts.StateDir, expiredEnrollment())
	seedEnrollment(t, opts.SourceStateDir)
	var out bytes.Buffer
	opts.Out = &out

	if err := Install(context.Background(), opts, &fakeRunner{}); err != nil {
		t.Fatalf("Install: %v", err)
	}

	if got := loadEnrollment(t, opts.StateDir); got != enrollment() {
		t.Fatalf("staged enrollment = %+v, want the source's %+v", got, enrollment())
	}
	if _, err := (&state.Store{Dir: opts.SourceStateDir}).LoadEnrollment(); !state.IsNoEnrollment(err) {
		t.Fatalf("the source still holds the enrollment: err = %v", err)
	}
	if !strings.Contains(out.String(), opts.SourceStateDir) {
		t.Fatalf("summary does not say where the fresh enrollment came from:\n%s", out.String())
	}
}

// An expires_at this connector cannot read is material it does not
// understand, and staking the daemon's only join on it is worse than
// replacing it — `serve` refuses on the same reasoning.
func TestInstallReplacesAStagedEnrollmentWhoseExpiryIsUnreadable(t *testing.T) {
	opts := options(t, "darwin")
	unreadable := expiredEnrollment()
	unreadable.ExpiresAt = "in about an hour"
	save(t, opts.StateDir, unreadable)
	seedEnrollment(t, opts.SourceStateDir)

	if err := Install(context.Background(), opts, &fakeRunner{}); err != nil {
		t.Fatalf("Install: %v", err)
	}

	if got := loadEnrollment(t, opts.StateDir); got != enrollment() {
		t.Fatalf("staged enrollment = %+v, want the source's %+v", got, enrollment())
	}
}

// Dead material and nowhere fresh to take it from is the one case the install
// cannot rescue, so it says which of the two things is wrong and what to run
// — and it says it in the terminal rather than installing a service that will
// say it in a log nobody reads.
func TestInstallRefusesAnExpiredStagedEnrollmentWithNoFreshSource(t *testing.T) {
	opts := options(t, "darwin")
	save(t, opts.StateDir, expiredEnrollment())
	runner := &fakeRunner{}

	err := Install(context.Background(), opts, runner)
	if err == nil {
		t.Fatal("Install with only expired material = nil error, want a refusal")
	}
	for _, want := range []string{"expired", "2026-09-06T10:00:00Z", opts.SourceStateDir, "gentility net join", "connector login --server"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("Install error = %v, want it to mention %q", err, want)
		}
	}
	if got := loadEnrollment(t, opts.StateDir); got != expiredEnrollment() {
		t.Fatalf("a refused install disturbed the staged material: %+v", got)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("a refused install ran commands: %v", runner.argv())
	}
	if _, err := os.Stat(opts.Root); !os.IsNotExist(err) {
		t.Fatalf("a refused install wrote under %s: %v", opts.Root, err)
	}
}

// A staged enrollment still inside its ten minutes and no newer material
// anywhere is a reinstall, not a re-login: leave it exactly where it is.
func TestInstallLeavesAFreshStagedEnrollmentAlone(t *testing.T) {
	opts := options(t, "darwin")
	seedEnrollment(t, opts.StateDir)

	if err := Install(context.Background(), opts, &fakeRunner{}); err != nil {
		t.Fatalf("Install: %v", err)
	}

	if got := loadEnrollment(t, opts.StateDir); got != enrollment() {
		t.Fatalf("staged enrollment = %+v, want it untouched %+v", got, enrollment())
	}
}

// The person's state directory can be the daemon's — an install run without
// sudo, or one pointed at itself. There is then nothing to carry across, and a
// handoff that carried anyway would clear the file it had just written.
func TestInstallKeepsAFreshEnrollmentWhenTheSourceIsTheDaemonsOwnDir(t *testing.T) {
	opts := options(t, "darwin")
	opts.SourceStateDir = opts.StateDir
	seedEnrollment(t, opts.StateDir)

	if err := Install(context.Background(), opts, &fakeRunner{}); err != nil {
		t.Fatalf("Install: %v", err)
	}

	if got := loadEnrollment(t, opts.StateDir); got != enrollment() {
		t.Fatalf("staged enrollment = %+v, want it untouched %+v", got, enrollment())
	}
}

// Both live: the person went and logged in again, on purpose, between the two
// commands. The newer material is the one they meant.
func TestInstallPrefersFreshSourceMaterialOverAFreshStagedOne(t *testing.T) {
	opts := options(t, "darwin")
	stale := enrollment()
	stale.EnrollmentID = "enr_earlier"
	stale.ExpiresAt = "2026-09-06T11:30:00Z"
	save(t, opts.StateDir, stale)
	seedEnrollment(t, opts.SourceStateDir)

	if err := Install(context.Background(), opts, &fakeRunner{}); err != nil {
		t.Fatalf("Install: %v", err)
	}

	if got := loadEnrollment(t, opts.StateDir); got != enrollment() {
		t.Fatalf("staged enrollment = %+v, want the source's %+v", got, enrollment())
	}
	if _, err := (&state.Store{Dir: opts.SourceStateDir}).LoadEnrollment(); !state.IsNoEnrollment(err) {
		t.Fatalf("the source still holds the enrollment: err = %v", err)
	}
}

// Dead material anywhere is nobody offering this machine anything: an
// exchanged node keeps the identity it earned, both directories are left as
// they are, and the install is a reinstall of the unit.
func TestInstallLeavesBothDirectoriesAloneWhenNothingFreshIsOnOffer(t *testing.T) {
	opts := options(t, "darwin")
	node := exchanged(t, opts.StateDir)
	save(t, opts.StateDir, expiredEnrollment())
	save(t, opts.SourceStateDir, expiredEnrollment())

	if err := Install(context.Background(), opts, &fakeRunner{}); err != nil {
		t.Fatalf("Install: %v", err)
	}

	if got := loadEnrollment(t, opts.StateDir); got != expiredEnrollment() {
		t.Fatalf("staged enrollment = %+v, want it untouched %+v", got, expiredEnrollment())
	}
	if got := loadEnrollment(t, opts.SourceStateDir); got != expiredEnrollment() {
		t.Fatalf("source enrollment = %+v, want it untouched %+v", got, expiredEnrollment())
	}
	if got, err := (&state.Store{Dir: opts.StateDir}).LoadNode(); err != nil || got != node {
		t.Fatalf("node = %+v (%v), want it untouched %+v", got, err, node)
	}
}

// Fresh material for a machine that already has a node is somebody re-enrolling
// it, and that ends the node the mothership knows. The install will not decide
// it: it names the node that is at stake and the flag that says go ahead.
func TestInstallRefusesFreshSourceMaterialForANodeThatHasAlreadyExchanged(t *testing.T) {
	opts := options(t, "darwin")
	node := exchanged(t, opts.StateDir)
	seedEnrollment(t, opts.SourceStateDir)
	runner := &fakeRunner{}

	err := Install(context.Background(), opts, runner)
	if err == nil {
		t.Fatal("Install over an identity = nil error, want a refusal")
	}
	for _, want := range []string{node.NodeID, opts.SourceStateDir, "--replace"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("Install error = %v, want it to mention %q", err, want)
		}
	}
	if got, err := (&state.Store{Dir: opts.StateDir}).LoadNode(); err != nil || got != node {
		t.Fatalf("a refused install disturbed the identity: %+v (%v)", got, err)
	}
	if got := loadEnrollment(t, opts.SourceStateDir); got != enrollment() {
		t.Fatalf("a refused install disturbed the source material: %+v", got)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("a refused install ran commands: %v", runner.argv())
	}
	if _, err := os.Stat(opts.Root); !os.IsNotExist(err) {
		t.Fatalf("a refused install wrote under %s: %v", opts.Root, err)
	}
}

// The unattended install: `net_machine_enroll` mints a trio, the recipe exports
// it, and `sudo -E connector install` stages what the environment carries.
// Nobody is at a browser and no directory holds anything.
func TestInstallStagesTheEnrollmentTheEnvironmentCarries(t *testing.T) {
	opts := options(t, "darwin")
	opts.LookupEnv = environment(minted(t))
	var out bytes.Buffer
	opts.Out = &out

	if err := Install(context.Background(), opts, &fakeRunner{}); err != nil {
		t.Fatalf("Install: %v", err)
	}

	if got := loadEnrollment(t, opts.StateDir); got != mintedEnrollment() {
		t.Fatalf("staged enrollment = %+v, want %+v", got, mintedEnrollment())
	}
	for _, want := range []string{"enr_env", "OVERLAY_", opts.StateDir} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("summary does not say %q:\n%s", want, out.String())
		}
	}
}

// The environment is what an operator states at the moment of the install, and
// the directories hold whatever was left over from before it.
func TestInstallPrefersTheEnvironmentOverBothDirectories(t *testing.T) {
	opts := options(t, "darwin")
	opts.LookupEnv = environment(minted(t))
	save(t, opts.StateDir, enrollment())
	seedEnrollment(t, opts.SourceStateDir)

	if err := Install(context.Background(), opts, &fakeRunner{}); err != nil {
		t.Fatalf("Install: %v", err)
	}

	if got := loadEnrollment(t, opts.StateDir); got != mintedEnrollment() {
		t.Fatalf("staged enrollment = %+v, want the environment's %+v", got, mintedEnrollment())
	}
}

// OVERLAY_EXPIRES_AT is the mint's own answer about its own window, and it is
// carried through unchanged rather than guessed at.
func TestInstallHonoursOverlayExpiresAt(t *testing.T) {
	opts := options(t, "darwin")
	vars := minted(t)
	vars[enroll.VarExpiresAt] = "2026-09-06T11:03:30Z"
	opts.LookupEnv = environment(vars)

	if err := Install(context.Background(), opts, &fakeRunner{}); err != nil {
		t.Fatalf("Install: %v", err)
	}

	want := mintedEnrollment()
	want.ExpiresAt = "2026-09-06T11:03:30Z"
	if got := loadEnrollment(t, opts.StateDir); got != want {
		t.Fatalf("staged enrollment = %+v, want %+v", got, want)
	}
}

// An expiry nobody can read is the one field the staging decisions are made on,
// so it is a refusal rather than a reason to fall back on ten minutes.
func TestInstallRefusesAnUnreadableOverlayExpiresAt(t *testing.T) {
	opts := options(t, "darwin")
	vars := minted(t)
	vars[enroll.VarExpiresAt] = "in about ten minutes"
	opts.LookupEnv = environment(vars)
	runner := &fakeRunner{}

	err := Install(context.Background(), opts, runner)
	if err == nil {
		t.Fatal("Install with an unreadable expiry = nil error, want a refusal")
	}
	for _, want := range []string{enroll.VarExpiresAt, "in about ten minutes", "RFC 3339"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("Install error = %v, want it to mention %q", err, want)
		}
	}
	if len(runner.calls) != 0 {
		t.Fatalf("a refused install ran commands: %v", runner.argv())
	}
}

// Three variables out of six is a recipe that went wrong, not a machine with no
// nonce. Falling through to the person's directory would install a service on
// material nobody meant it to have — and would say nothing about the export
// that failed.
func TestInstallRefusesAPartialEnvironmentNamingTheMissingVariable(t *testing.T) {
	opts := options(t, "darwin")
	vars := minted(t)
	delete(vars, enroll.VarNonce)
	opts.LookupEnv = environment(vars)
	// There IS material in the person's directory: the refusal has to be about
	// the environment even so.
	seedEnrollment(t, opts.SourceStateDir)
	runner := &fakeRunner{}

	err := Install(context.Background(), opts, runner)
	if err == nil {
		t.Fatal("Install with a partial environment = nil error, want a refusal")
	}
	if !strings.Contains(err.Error(), enroll.VarNonce) {
		t.Fatalf("Install error = %v, want it to name %s", err, enroll.VarNonce)
	}
	if _, err := (&state.Store{Dir: opts.StateDir}).LoadEnrollment(); !state.IsNoEnrollment(err) {
		t.Fatalf("a refused install staged something: err = %v", err)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("a refused install ran commands: %v", runner.argv())
	}
	if _, err := os.Stat(opts.Root); !os.IsNotExist(err) {
		t.Fatalf("a refused install wrote under %s: %v", opts.Root, err)
	}
}

// The environment offering a trio to a machine that already has a node is the
// same refusal as the person's directory offering one, and names the same two
// things.
func TestInstallRefusesTheEnvironmentForAMachineThatAlreadyHasANode(t *testing.T) {
	opts := options(t, "darwin")
	opts.LookupEnv = environment(minted(t))
	node := exchanged(t, opts.StateDir)
	runner := &fakeRunner{}

	err := Install(context.Background(), opts, runner)
	if err == nil {
		t.Fatal("Install over an identity = nil error, want a refusal")
	}
	for _, want := range []string{node.NodeID, "OVERLAY_", "--replace"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("Install error = %v, want it to mention %q", err, want)
		}
	}
	if got, err := (&state.Store{Dir: opts.StateDir}).LoadNode(); err != nil || got != node {
		t.Fatalf("a refused install disturbed the identity: %+v (%v)", got, err)
	}
	if _, err := os.Stat(filepath.Join(opts.StateDir, state.TSNetDir)); err != nil {
		t.Fatalf("a refused install disturbed the tsnet state: %v", err)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("a refused install ran commands: %v", runner.argv())
	}
}

// --replace is the re-enrolment: the node the head no longer holds goes, both
// halves of it, and the machine comes up on the trio it was just handed.
func TestInstallWithReplaceDiscardsTheIdentityAndStagesTheEnvironments(t *testing.T) {
	opts := options(t, "darwin")
	opts.LookupEnv = environment(minted(t))
	opts.ReplaceIdentity = true
	node := exchanged(t, opts.StateDir)
	var out bytes.Buffer
	opts.Out = &out

	if err := Install(context.Background(), opts, &fakeRunner{}); err != nil {
		t.Fatalf("Install --replace: %v", err)
	}

	if _, err := (&state.Store{Dir: opts.StateDir}).LoadNode(); !state.IsNotEnrolled(err) {
		t.Fatalf("the identity survived --replace: err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(opts.StateDir, state.TSNetDir)); !os.IsNotExist(err) {
		t.Fatalf("the tsnet state survived --replace: %v", err)
	}
	if _, err := (&state.Store{Dir: opts.StateDir}).LoadStatus(); !state.IsNoStatus(err) {
		t.Fatalf("the status snapshot survived --replace: err = %v", err)
	}
	if got := loadEnrollment(t, opts.StateDir); got != mintedEnrollment() {
		t.Fatalf("staged enrollment = %+v, want %+v", got, mintedEnrollment())
	}
	// What was discarded and what was staged, both said out loud: this is the
	// only record an operator gets of a node that stopped existing.
	for _, want := range []string{node.NodeID, "discarded", "enr_env"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("summary does not say %q:\n%s", want, out.String())
		}
	}
}

// The same, from the other source: a person who logged in again and means this
// machine to come back as a new node.
func TestInstallWithReplaceTakesTheFreshEnrollmentFromThePersonsDirectory(t *testing.T) {
	opts := options(t, "darwin")
	opts.ReplaceIdentity = true
	exchanged(t, opts.StateDir)
	seedEnrollment(t, opts.SourceStateDir)

	if err := Install(context.Background(), opts, &fakeRunner{}); err != nil {
		t.Fatalf("Install --replace: %v", err)
	}

	if _, err := (&state.Store{Dir: opts.StateDir}).LoadNode(); !state.IsNotEnrolled(err) {
		t.Fatalf("the identity survived --replace: err = %v", err)
	}
	if got := loadEnrollment(t, opts.StateDir); got != enrollment() {
		t.Fatalf("staged enrollment = %+v, want the source's %+v", got, enrollment())
	}
	if _, err := (&state.Store{Dir: opts.SourceStateDir}).LoadEnrollment(); !state.IsNoEnrollment(err) {
		t.Fatalf("the source still holds the enrollment: err = %v", err)
	}
}

// The documented flow is `connector login --server` as the person, then
// `sudo connector install` — and sudo resets HOME. An install that asked
// state.DefaultDir() where the enrollment was would be asking root's config
// directory about a file the person wrote, and would refuse a machine that is
// in fact enrolled. With no flags at all, it finds it.
func TestInstallWithNoFlagsTakesTheEnrollmentFromTheSudoInvoker(t *testing.T) {
	t.Setenv("GENTILITY_STATE_DIR", "")
	t.Setenv("SUDO_USER", "alice")

	home := t.TempDir()
	person := filepath.Join(home, "Library", "Application Support", "gentility")
	seedEnrollment(t, person)

	opts := options(t, "darwin")
	opts.SourceStateDir = ""
	opts.LookupUser = homes(map[string]string{"alice": home})
	var out bytes.Buffer
	opts.Out = &out

	if err := Install(context.Background(), opts, &fakeRunner{}); err != nil {
		t.Fatalf("Install: %v", err)
	}

	moved, err := (&state.Store{Dir: opts.StateDir}).LoadEnrollment()
	if err != nil {
		t.Fatalf("LoadEnrollment from the daemon's state dir: %v", err)
	}
	if moved != enrollment() {
		t.Fatalf("enrollment = %+v, want %+v", moved, enrollment())
	}
	if _, err := (&state.Store{Dir: person}).LoadEnrollment(); !state.IsNoEnrollment(err) {
		t.Fatalf("the person's state dir still holds the enrollment: err = %v", err)
	}
	if !strings.Contains(out.String(), person) {
		t.Fatalf("summary does not say where it took the enrollment from:\n%s", out.String())
	}
}

// The default source is the directory state.DefaultDir() would have computed
// for the person who typed sudo — the same precedence, rooted at their home.
func TestSourceStateDirIsTheSudoInvokersConfigDir(t *testing.T) {
	for _, c := range []struct{ goos, xdg, want string }{
		{"darwin", "", "/home/someone/Library/Application Support/gentility"},
		// os.UserConfigDir reads $HOME on darwin and ignores XDG_CONFIG_HOME.
		{"darwin", "/xdg", "/home/someone/Library/Application Support/gentility"},
		{"linux", "", "/home/someone/.config/gentility"},
		{"linux", "/xdg", "/xdg/gentility"},
	} {
		t.Run(c.goos+" XDG="+c.xdg, func(t *testing.T) {
			t.Setenv("GENTILITY_STATE_DIR", "")
			t.Setenv("XDG_CONFIG_HOME", c.xdg)
			t.Setenv("SUDO_USER", "alice")

			opts := Options{GOOS: c.goos, LookupUser: homes(map[string]string{"alice": "/home/someone"})}
			got, err := sourceStateDir(opts.withDefaults())
			if err != nil {
				t.Fatalf("sourceStateDir: %v", err)
			}
			if got != c.want {
				t.Fatalf("sourceStateDir = %q, want %q", got, c.want)
			}
		})
	}
}

// No sudo, no invoker: the person running the install is the person who logged
// in, and their own state directory is the answer.
func TestSourceStateDirWithoutSudoIsThisAccountsOwn(t *testing.T) {
	t.Setenv("SUDO_USER", "")

	opts := Options{GOOS: "darwin", LookupUser: func(name string) (*user.User, error) {
		t.Fatalf("looked %q up with no $SUDO_USER set", name)
		return nil, nil
	}}
	got, err := sourceStateDir(opts.withDefaults())
	if err != nil {
		t.Fatalf("sourceStateDir: %v", err)
	}
	if want := state.DefaultDir(); got != want {
		t.Fatalf("sourceStateDir = %q, want %q", got, want)
	}
}

// --source-state-dir is the override, and an override that lost to a guess
// would be no override at all.
func TestSourceStateDirPrefersWhatTheFlagSaid(t *testing.T) {
	t.Setenv("GENTILITY_STATE_DIR", "/env")
	t.Setenv("SUDO_USER", "alice")

	opts := Options{
		GOOS:           "darwin",
		SourceStateDir: "/explicit",
		LookupUser:     homes(map[string]string{"alice": "/home/someone"}),
	}
	got, err := sourceStateDir(opts.withDefaults())
	if err != nil {
		t.Fatalf("sourceStateDir: %v", err)
	}
	if got != "/explicit" {
		t.Fatalf("sourceStateDir = %q, want /explicit", got)
	}
}

// $GENTILITY_STATE_DIR only reaches an install if someone passed it through
// sudo on purpose, so it is a statement of where the enrollment is and it
// outranks the guess made from $SUDO_USER.
func TestSourceStateDirHonoursAnExplicitStateDirEnv(t *testing.T) {
	t.Setenv("GENTILITY_STATE_DIR", "/env/gentility")
	t.Setenv("SUDO_USER", "alice")

	opts := Options{GOOS: "darwin", LookupUser: homes(map[string]string{"alice": "/home/someone"})}
	got, err := sourceStateDir(opts.withDefaults())
	if err != nil {
		t.Fatalf("sourceStateDir: %v", err)
	}
	if got != "/env/gentility" {
		t.Fatalf("sourceStateDir = %q, want /env/gentility", got)
	}
}

// A $SUDO_USER this machine has no account for is a question, not a fallback:
// quietly reading root's directory instead would report "not enrolled" about a
// directory nobody was ever going to look in.
func TestInstallRefusesASudoUserItCannotFind(t *testing.T) {
	t.Setenv("GENTILITY_STATE_DIR", "")
	t.Setenv("SUDO_USER", "ghost")

	opts := options(t, "darwin")
	opts.SourceStateDir = ""
	opts.LookupUser = homes(map[string]string{"alice": "/home/someone"})
	runner := &fakeRunner{}

	err := Install(context.Background(), opts, runner)
	if err == nil {
		t.Fatal("Install with an unknown $SUDO_USER = nil error, want a refusal")
	}
	for _, want := range []string{"ghost", "SUDO_USER", "--source-state-dir"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("Install error = %v, want it to mention %q", err, want)
		}
	}
	if len(runner.calls) != 0 {
		t.Fatalf("a refused install ran commands: %v", runner.argv())
	}
	if _, err := os.Stat(opts.Root); !os.IsNotExist(err) {
		t.Fatalf("a refused install wrote under %s: %v", opts.Root, err)
	}
}

// A connector that has already exchanged holds its identity in node.json and
// consumed its enrollment long ago. Reinstalling it must not go looking for
// material that is meant to be gone.
func TestInstallLeavesAnAlreadyExchangedNodeAlone(t *testing.T) {
	opts := options(t, "darwin")
	node := state.Node{NodeID: "node_01", NodeCredential: "cred", LastRevision: 7}
	if err := (&state.Store{Dir: opts.StateDir}).SaveNode(node); err != nil {
		t.Fatalf("SaveNode: %v", err)
	}

	if err := Install(context.Background(), opts, &fakeRunner{}); err != nil {
		t.Fatalf("Install: %v", err)
	}

	loaded, err := (&state.Store{Dir: opts.StateDir}).LoadNode()
	if err != nil {
		t.Fatalf("LoadNode: %v", err)
	}
	if loaded != node {
		t.Fatalf("node = %+v, want %+v", loaded, node)
	}
	if _, err := (&state.Store{Dir: opts.StateDir}).LoadEnrollment(); !state.IsNoEnrollment(err) {
		t.Fatalf("Install invented an enrollment: err = %v", err)
	}
}

// Desk's Update 6net: the bundled connector's `install --interface
// --place-binary` as root through osascript, which sets no $SUDO_USER, and with
// no --source-state-dir. The person's directory is then whatever $HOME names —
// theirs, root's, or none at all — and with an identity in hand, nothing there
// is a reinstall that keeps it. Material left there is refused rather than
// traded for the identity, because Desk never passes --replace.
func TestRootsReinstallWithoutSudoKeepsTheIdentityWhateverHomeHolds(t *testing.T) {
	t.Setenv("SUDO_USER", "")
	t.Setenv("GENTILITY_STATE_DIR", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	rootInstall := func(t *testing.T) (Options, state.Node) {
		opts := options(t, "darwin")
		opts.SourceStateDir = ""
		opts.Euid = func() int { return 0 }
		return opts.withDefaults(), exchanged(t, opts.StateDir)
	}

	for _, home := range []string{"", t.TempDir()} {
		t.Setenv("HOME", home)
		opts, node := rootInstall(t)

		handoff, err := handoffEnrollment(opts)
		if err != nil || len(handoff) != 0 {
			t.Fatalf("HOME=%q: handoff = %v, %v; want a reinstall with nothing staged", home, handoff, err)
		}
		if got, err := (&state.Store{Dir: opts.StateDir}).LoadNode(); err != nil || got != node {
			t.Fatalf("HOME=%q: the identity moved: %+v (%v)", home, got, err)
		}
	}

	t.Setenv("HOME", t.TempDir())
	opts, node := rootInstall(t)
	seedEnrollment(t, state.DefaultDir())
	_, err := handoffEnrollment(opts)
	if err == nil || !strings.Contains(err.Error(), "--replace") {
		t.Fatalf("handoff with material in $HOME's directory = %v, want the --replace refusal", err)
	}
	if got, err := (&state.Store{Dir: opts.StateDir}).LoadNode(); err != nil || got != node {
		t.Fatalf("a refused handoff disturbed the identity: %+v (%v)", got, err)
	}
}

// Installing a service that cannot serve is worse than not installing it: the
// supervisor restarts it forever and the person reads the refusal in a log
// rather than in the terminal they typed it into.
func TestInstallRefusesWhenNothingIsEnrolled(t *testing.T) {
	opts := options(t, "darwin")
	runner := &fakeRunner{}

	err := Install(context.Background(), opts, runner)
	if err == nil {
		t.Fatal("Install with nothing enrolled = nil error, want a refusal")
	}
	if !state.IsNoEnrollment(err) {
		t.Fatalf("Install error = %v, want it to be state.ErrNoEnrollment", err)
	}
	for _, want := range []string{"gentility net join", "connector login --server"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("Install error = %v, want it to name %q", err, want)
		}
	}
	if len(runner.calls) != 0 {
		t.Fatalf("a refused install ran commands: %v", runner.argv())
	}
	if _, err := os.Stat(opts.Root); !os.IsNotExist(err) {
		t.Fatalf("a refused install wrote under %s: %v", opts.Root, err)
	}
}

func TestInstallRefusesAnUnsupportedPlatform(t *testing.T) {
	opts := options(t, "windows")
	seedEnrollment(t, opts.SourceStateDir)
	runner := &fakeRunner{}

	err := Install(context.Background(), opts, runner)
	if err == nil {
		t.Fatal("Install on windows = nil error, want a refusal")
	}
	for _, want := range []string{"windows", "connector serve", "70"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("Install error = %v, want it to mention %q", err, want)
		}
	}
	if len(runner.calls) != 0 {
		t.Fatalf("a refused install ran commands: %v", runner.argv())
	}
	if _, err := os.Stat(opts.Root); !os.IsNotExist(err) {
		t.Fatalf("a refused install wrote under %s: %v", opts.Root, err)
	}
}

// Uninstall stops the supervision and nothing else. Running it twice is what a
// person does when the first one printed something they did not read.
func TestUninstallStopsTheServiceAndRemovesTheUnitTwice(t *testing.T) {
	opts := options(t, "darwin")
	seedEnrollment(t, opts.SourceStateDir)
	if err := Install(context.Background(), opts, &fakeRunner{}); err != nil {
		t.Fatalf("Install: %v", err)
	}

	plist := filepath.Join(opts.Root, "Library", "LaunchDaemons", "ai.gentility.connector.plist")
	runner := &fakeRunner{}
	var out bytes.Buffer
	opts.Out = &out
	if err := Uninstall(context.Background(), opts, runner); err != nil {
		t.Fatalf("Uninstall: %v", err)
	}

	runner.wants(t, "launchctl bootout system/ai.gentility.connector")
	if _, err := os.Stat(plist); !os.IsNotExist(err) {
		t.Fatalf("the unit survived the uninstall: %v", err)
	}
	if !strings.Contains(out.String(), opts.StateDir) {
		t.Fatalf("summary does not say the state directory stays:\n%s", out.String())
	}

	// The identity is not the supervision: uninstalling stops the service, it
	// does not un-enrol the machine.
	if _, err := (&state.Store{Dir: opts.StateDir}).LoadEnrollment(); err != nil {
		t.Fatalf("uninstall took the enrollment with it: %v", err)
	}

	if err := Uninstall(context.Background(), opts, &fakeRunner{}); err != nil {
		t.Fatalf("second Uninstall: %v", err)
	}
}

// A stop the supervisor refused still finishes the uninstall, as it always
// has, and is returned as a *StopError so a caller that cares can tell.
func TestUninstallReturnsAStopErrorWhenTheStopFails(t *testing.T) {
	opts := options(t, "darwin")
	plist := writeUnit(t, opts, unitOf(opts))
	var out bytes.Buffer
	opts.Out = &out

	err := Uninstall(context.Background(), opts, &fakeRunner{fail: map[string]error{"launchctl": errors.New("Boot-out failed: 5: Input/output error")}})

	var stop *StopError
	if !errors.As(err, &stop) || !strings.Contains(stop.Error(), "Boot-out failed") {
		t.Fatalf("Uninstall = %v, want a StopError carrying the supervisor's answer", err)
	}
	if _, err := os.Stat(plist); !os.IsNotExist(err) {
		t.Fatalf("the unit survived the uninstall: %v", err)
	}
	if !strings.Contains(out.String(), "note: Boot-out failed") {
		t.Fatalf("the note is gone from the summary:\n%s", out.String())
	}
}

// Only an interface unit owes the resolver cleanup.
func TestUnitIsInterfaceReadsTheInstalledUnit(t *testing.T) {
	for _, goos := range []string{"darwin", "linux"} {
		t.Run(goos, func(t *testing.T) {
			opts := options(t, goos)
			if UnitIsInterface(goos, opts.Root, DefaultLabel) {
				t.Fatal("no unit read as an interface unit")
			}
			writeUnit(t, opts, unitOf(opts))
			if UnitIsInterface(goos, opts.Root, DefaultLabel) {
				t.Fatal("a userspace unit read as an interface unit")
			}
			opts.Interface = true
			writeUnit(t, opts, unitOf(opts))
			if !UnitIsInterface(goos, opts.Root, DefaultLabel) {
				t.Fatal("an interface unit did not read as one")
			}
		})
	}
}

func TestUninstallOnLinuxDisablesTheService(t *testing.T) {
	opts := options(t, "linux")
	seedEnrollment(t, opts.SourceStateDir)
	if err := Install(context.Background(), opts, &fakeRunner{}); err != nil {
		t.Fatalf("Install: %v", err)
	}

	runner := &fakeRunner{}
	if err := Uninstall(context.Background(), opts, runner); err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	runner.wants(t, "systemctl disable --now gentility-connector")

	unit := filepath.Join(opts.Root, "etc", "systemd", "system", "gentility-connector.service")
	if _, err := os.Stat(unit); !os.IsNotExist(err) {
		t.Fatalf("the unit survived the uninstall: %v", err)
	}
}

// The launchd label is reverse-DNS and the systemd unit name is not; the
// derivation is what keeps the golden file's name honest.
func TestServiceNameDropsTheReverseDNSPrefix(t *testing.T) {
	for _, c := range []struct{ label, want string }{
		{"ai.gentility.connector", "gentility-connector"},
		{"ai.gentility.connector.staging", "gentility-connector-staging"},
		{"connector", "connector"},
		{"gentility.connector", "gentility-connector"},
	} {
		if got := serviceName(c.label); got != c.want {
			t.Errorf("serviceName(%q) = %q, want %q", c.label, got, c.want)
		}
	}
}

// --replace is not a wipe. With nothing fresh on offer there is nothing to
// replace the identity with, so the flag does nothing at all and the install is
// the reinstall it would have been without it — a machine left with neither an
// identity nor material to earn one is a machine somebody has to drive to.
func TestInstallWithReplaceAndNothingOnOfferLeavesTheIdentityAlone(t *testing.T) {
	opts := options(t, "darwin")
	opts.ReplaceIdentity = true
	node := exchanged(t, opts.StateDir)
	save(t, opts.SourceStateDir, expiredEnrollment())

	if err := Install(context.Background(), opts, &fakeRunner{}); err != nil {
		t.Fatalf("Install --replace: %v", err)
	}

	if got, err := (&state.Store{Dir: opts.StateDir}).LoadNode(); err != nil || got != node {
		t.Fatalf("node = %+v (%v), want it untouched %+v", got, err, node)
	}
	if _, err := os.Stat(filepath.Join(opts.StateDir, state.TSNetDir)); err != nil {
		t.Fatalf("--replace with nothing on offer removed the tsnet state: %v", err)
	}
}

// The wreck the flag is really for: a state directory that has lost node.json
// and kept tsnet's node key. That half alone is what a head which has forgotten
// this node answers by asking for an interactive login — the refused rejoin —
// and a join on the new trio would hit the same refusal until it is gone.
func TestInstallWithReplaceClearsATsnetStateLeftWithoutANode(t *testing.T) {
	opts := options(t, "darwin")
	opts.LookupEnv = environment(minted(t))
	opts.ReplaceIdentity = true
	exchanged(t, opts.StateDir)
	if err := (&state.Store{Dir: opts.StateDir}).ClearNode(); err != nil {
		t.Fatal(err)
	}

	if err := Install(context.Background(), opts, &fakeRunner{}); err != nil {
		t.Fatalf("Install --replace: %v", err)
	}

	if _, err := os.Stat(filepath.Join(opts.StateDir, state.TSNetDir)); !os.IsNotExist(err) {
		t.Fatalf("the stale tsnet state survived --replace: %v", err)
	}
	if got := loadEnrollment(t, opts.StateDir); got != mintedEnrollment() {
		t.Fatalf("staged enrollment = %+v, want %+v", got, mintedEnrollment())
	}
}

// `enable --now` starts a unit that is stopped and does nothing to one that is
// already running, and launchd's bootstrap is the same. So an install that
// staged material and stopped there would leave a daemon serving the identity
// it booted with while fresh material sat unread in its state directory — and
// the pre-auth key in that material would burn its ten minutes waiting for
// somebody to notice. Staging is only finished when the process has restarted
// onto it.
func TestInstallRestartsTheDaemonOntoTheMaterialItJustStaged(t *testing.T) {
	for _, c := range []struct{ goos, want string }{
		{"linux", "systemctl restart gentility-connector"},
		{"darwin", "launchctl kickstart -k system/ai.gentility.connector"},
	} {
		t.Run(c.goos, func(t *testing.T) {
			opts := options(t, c.goos)
			seedEnrollment(t, opts.SourceStateDir)
			runner := &fakeRunner{}
			var out bytes.Buffer
			opts.Out = &out

			if err := Install(context.Background(), opts, runner); err != nil {
				t.Fatalf("Install: %v", err)
			}

			argv := runner.argv()
			if last := argv[len(argv)-1]; last != c.want {
				t.Fatalf("activation ended with %q, want %q:\n%v", last, c.want, argv)
			}
			if !strings.Contains(out.String(), "restarted") {
				t.Fatalf("summary does not say the daemon was restarted:\n%s", out.String())
			}
		})
	}
}

// The same, for the environment and for a --replace: every source of staged
// material owes the restart, because the daemon's in-memory identity and the
// one on disk disagreeing is the same fault however the disk came to change.
func TestInstallRestartsTheDaemonAfterReplacingAnIdentity(t *testing.T) {
	opts := options(t, "linux")
	opts.LookupEnv = environment(minted(t))
	opts.ReplaceIdentity = true
	exchanged(t, opts.StateDir)
	runner := &fakeRunner{}

	if err := Install(context.Background(), opts, runner); err != nil {
		t.Fatalf("Install --replace: %v", err)
	}

	argv := runner.argv()
	if last := argv[len(argv)-1]; last != "systemctl restart gentility-connector" {
		t.Fatalf("activation ended with %q, want a restart:\n%v", last, argv)
	}
}

// A reinstall of the unit alone stages nothing, and restarting a healthy daemon
// for it would drop every connection this machine is carrying for no reason at
// all.
func TestInstallDoesNotRestartADaemonItStagedNothingFor(t *testing.T) {
	opts := options(t, "linux")
	exchanged(t, opts.StateDir)
	runner := &fakeRunner{}
	var out bytes.Buffer
	opts.Out = &out

	if err := Install(context.Background(), opts, runner); err != nil {
		t.Fatalf("Install: %v", err)
	}

	for _, line := range runner.argv() {
		if strings.Contains(line, "restart") || strings.Contains(line, "kickstart") {
			t.Fatalf("a reinstall restarted the daemon: %v", runner.argv())
		}
	}
	if strings.Contains(out.String(), "restarted") {
		t.Fatalf("summary claims a restart that did not happen:\n%s", out.String())
	}
}

func TestUnitInstalledLooksForTheUnitFileUnderRoot(t *testing.T) {
	for _, tc := range []struct {
		goos string
		path string
	}{
		{"darwin", "Library/LaunchDaemons/" + DefaultLabel + ".plist"},
		{"linux", "etc/systemd/system/gentility-connector.service"},
	} {
		t.Run(tc.goos, func(t *testing.T) {
			root := t.TempDir()
			if UnitInstalled(tc.goos, root, DefaultLabel) {
				t.Fatalf("UnitInstalled = true with no unit file under %s", root)
			}

			unit := filepath.Join(root, tc.path)
			if err := os.MkdirAll(filepath.Dir(unit), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(unit, []byte("unit"), 0o644); err != nil {
				t.Fatal(err)
			}
			if !UnitInstalled(tc.goos, root, DefaultLabel) {
				t.Fatalf("UnitInstalled = false with %s present", unit)
			}
		})
	}

	if UnitInstalled("plan9", t.TempDir(), DefaultLabel) {
		t.Fatal("UnitInstalled = true on a goos with no supervisor")
	}
}

// placing is an install that copies its own binary to /usr/local/bin under the
// test's root, the way Desk asks for one: the running binary is a file the test
// wrote, so the bytes that land can be checked against it.
func placing(t *testing.T, opts Options, content string) Options {
	t.Helper()
	source := filepath.Join(t.TempDir(), "gentility")
	if err := os.WriteFile(source, []byte(content), 0o700); err != nil {
		t.Fatal(err)
	}
	opts.BinaryPath = ""
	opts.PlaceBinary = "/usr/local/bin/gentility"
	opts.Executable = func() (string, error) { return source, nil }
	return opts
}

// Desk runs install from inside its own bundle, which the person owns. A root
// daemon started from there is a root daemon that person can rewrite, so the
// binary is copied somewhere root owns and the unit names the copy.
func TestInstallPlaceBinaryCopiesThisBinaryAndRunsTheDaemonFromThere(t *testing.T) {
	opts := placing(t, options(t, "darwin"), "connector v1")
	exchanged(t, opts.StateDir)
	runner := &fakeRunner{}

	if err := Install(context.Background(), opts, runner); err != nil {
		t.Fatalf("Install: %v", err)
	}

	placed := filepath.Join(opts.Root, "usr", "local", "bin", "gentility")
	if got := read(t, placed); got != "connector v1" {
		t.Fatalf("placed binary = %q, want the running binary's bytes", got)
	}
	info, err := os.Stat(placed)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Fatalf("placed binary mode = %v, want 0755", info.Mode().Perm())
	}

	plist := filepath.Join(opts.Root, "Library", "LaunchDaemons", "ai.gentility.connector.plist")
	want, err := RenderLaunchd(Unit{
		BinaryPath: "/usr/local/bin/gentility",
		StateDir:   opts.StateDir,
		Label:      DefaultLabel,
		LogDir:     opts.LogDir,
	})
	if err != nil {
		t.Fatalf("RenderLaunchd: %v", err)
	}
	if got := read(t, plist); got != want {
		t.Fatalf("plist =\n%s\nwant\n%s", got, want)
	}
}

// Joining again with the same Desk places the same bytes, which is no reason to
// cut a healthy daemon's connections; an updated Desk places new ones, and the
// daemon still running the old binary is restarted onto them.
func TestInstallPlaceBinaryRestartsOnlyWhenTheBytesChanged(t *testing.T) {
	opts := placing(t, options(t, "darwin"), "connector v1")
	exchanged(t, opts.StateDir)
	if err := Install(context.Background(), opts, &fakeRunner{}); err != nil {
		t.Fatalf("first Install: %v", err)
	}

	var out bytes.Buffer
	opts.Out = &out
	same := &fakeRunner{loaded: true}
	if err := Install(context.Background(), opts, same); err != nil {
		t.Fatalf("second Install: %v", err)
	}
	same.wants(t, "launchctl print system/ai.gentility.connector")
	if strings.Contains(out.String(), "placed") || strings.Contains(out.String(), "restarted") {
		t.Fatalf("an install placing the same bytes reports a replacement:\n%s", out.String())
	}

	opts = placing(t, opts, "connector v2")
	out.Reset()
	changed := &fakeRunner{loaded: true}
	if err := Install(context.Background(), opts, changed); err != nil {
		t.Fatalf("third Install: %v", err)
	}
	changed.wants(t,
		"launchctl print system/ai.gentility.connector",
		"launchctl kickstart -k system/ai.gentility.connector",
	)
	if got := read(t, filepath.Join(opts.Root, "usr", "local", "bin", "gentility")); got != "connector v2" {
		t.Fatalf("placed binary = %q, want the new bytes", got)
	}
	if !strings.Contains(out.String(), "placed") {
		t.Fatalf("summary does not report the replacement:\n%s", out.String())
	}
}

// --binary names a connector to supervise where it is; --place-binary copies
// this one. Asked for both, an install would have to pick, so it refuses.
func TestInstallRefusesBinaryWithPlaceBinary(t *testing.T) {
	opts := placing(t, options(t, "darwin"), "connector v1")
	opts.BinaryPath = "/usr/local/bin/connector"
	seedEnrollment(t, opts.SourceStateDir)
	runner := &fakeRunner{}

	err := Install(context.Background(), opts, runner)
	if !errors.Is(err, ErrBinaryAndPlaceBinary) {
		t.Fatalf("Install error = %v, want ErrBinaryAndPlaceBinary", err)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("a refused install ran commands: %v", runner.argv())
	}
	if _, err := os.Stat(opts.Root); !os.IsNotExist(err) {
		t.Fatalf("a refused install wrote under %s: %v", opts.Root, err)
	}
	if _, err := os.Stat(opts.StateDir); !os.IsNotExist(err) {
		t.Fatalf("a refused install staged into %s: %v", opts.StateDir, err)
	}
}

// A root daemon's binary is only as trustworthy as every directory above it:
// whoever can write to one of them can rename the binary away and put their
// own in its place. sshd's StrictModes asks the same of authorized_keys.
func TestTrustedForRootRefusesAnyDirectoryAnotherAccountCanWrite(t *testing.T) {
	root := func(path string, mode fs.FileMode) dirOwner { return dirOwner{path: path, uid: 0, mode: mode} }
	for _, tc := range []struct {
		name    string
		chain   []dirOwner
		refuses string
	}{
		{"all root and 0755", []dirOwner{root("/usr/local/bin", 0o755), root("/usr/local", 0o755), root("/usr", 0o755), root("/", 0o755)}, ""},
		{"a group-writable ancestor", []dirOwner{root("/usr/local/bin", 0o755), root("/usr/local", 0o775), root("/usr", 0o755), root("/", 0o755)}, "/usr/local"},
		{"an other-writable parent", []dirOwner{root("/tmp/bin", 0o757), root("/tmp", 0o755), root("/", 0o755)}, "/tmp/bin"},
		{"a sticky world-writable parent", []dirOwner{root("/tmp", 0o777|fs.ModeSticky), root("/", 0o755)}, "/tmp"},
		{"a symlinked ancestor", []dirOwner{root("/Users/someone/bin", 0o755|fs.ModeSymlink), root("/Users/someone", 0o755), root("/Users", 0o755), root("/", 0o755)}, "/Users/someone/bin"},
		{"a non-root-owned ancestor", []dirOwner{root("/usr/local/bin", 0o755), {path: "/usr/local", uid: 501, mode: 0o755}, root("/usr", 0o755), root("/", 0o755)}, "/usr/local"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := trustedForRoot(tc.chain)
			if tc.refuses == "" {
				if err != nil {
					t.Fatalf("trustedForRoot = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("trustedForRoot = nil, want a refusal naming %s", tc.refuses)
			}
			if !strings.Contains(err.Error(), tc.refuses+":") {
				t.Fatalf("trustedForRoot = %v, want it to name %s", err, tc.refuses)
			}
		})
	}
}

// The directory a binary is placed into may not exist yet; what MkdirAll will
// create is root's, so the chain starts at the first directory that does. The
// chain is the path as written, every parent in turn, with a symlink reported
// as one rather than followed: the unit runs the path as written.
func TestAncestryWalksEveryParentOfThePathAsWritten(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}

	chain, err := ancestry(filepath.Join(dir, "link", "usr", "local", "bin"))
	if err != nil {
		t.Fatalf("ancestry: %v", err)
	}
	want := []string{filepath.Join(dir, "link")}
	for p := dir; ; p = filepath.Dir(p) {
		want = append(want, p)
		if p == "/" {
			break
		}
	}
	if len(chain) != len(want) {
		t.Fatalf("chain has %d entries, want %d: %+v", len(chain), len(want), chain)
	}
	for i, p := range want {
		if chain[i].path != p {
			t.Fatalf("chain[%d] = %s, want %s", i, chain[i].path, p)
		}
	}
	if chain[0].mode&fs.ModeSymlink == 0 {
		t.Fatalf("chain[0] = %+v, want it reported as a symlink", chain[0])
	}
	if chain[1].mode&fs.ModeSymlink != 0 || chain[1].uid != uint32(os.Getuid()) {
		t.Fatalf("chain[1] = %+v, want a directory owned by uid %d", chain[1], os.Getuid())
	}
	if last := chain[len(chain)-1]; last.uid != 0 {
		t.Fatalf("chain ends at %+v, want / owned by root", last)
	}
}

// A path left at the target by someone else — here a symlink back into the
// bundle the person owns — is replaced by a file root owns, not mistaken for
// the binary already being in place.
func TestInstallPlaceBinaryReplacesASymlinkAtTheTarget(t *testing.T) {
	opts := placing(t, options(t, "darwin"), "connector v1")
	exchanged(t, opts.StateDir)
	source, err := opts.Executable()
	if err != nil {
		t.Fatal(err)
	}
	placed := filepath.Join(opts.Root, "usr", "local", "bin", "gentility")
	if err := os.MkdirAll(filepath.Dir(placed), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(source, placed); err != nil {
		t.Fatal(err)
	}

	if err := Install(context.Background(), opts, &fakeRunner{}); err != nil {
		t.Fatalf("Install: %v", err)
	}
	info, err := os.Lstat(placed)
	if err != nil {
		t.Fatal(err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o755 {
		t.Fatalf("placed = %v, want a regular 0755 file", info.Mode())
	}
	if got := read(t, placed); got != "connector v1" {
		t.Fatalf("placed binary = %q, want the source's bytes", got)
	}
}

// The unit runs PlaceBinary as written, so it is refused unless it is
// absolute and already clean: a path with a .. in it names a directory the
// ancestry walk did not.
func TestInstallRefusesAPlaceBinaryThatIsNotAbsoluteAndClean(t *testing.T) {
	for _, path := range []string{"usr/local/bin/gentility", "/usr/local/../local/bin/gentility", "/usr/local/bin/"} {
		t.Run(path, func(t *testing.T) {
			opts := placing(t, options(t, "darwin"), "connector v1")
			opts.PlaceBinary = path
			seedEnrollment(t, opts.SourceStateDir)

			if err := Install(context.Background(), opts, &fakeRunner{}); !errors.Is(err, ErrPlaceBinaryPath) {
				t.Fatalf("Install error = %v, want ErrPlaceBinaryPath", err)
			}
			if _, err := os.Stat(opts.Root); !os.IsNotExist(err) {
				t.Fatalf("a refused install wrote under %s: %v", opts.Root, err)
			}
		})
	}
}

// Run as root, an install whose target sits under a directory anyone can write
// refuses before it writes anything at all.
func TestInstallAsRootRefusesAnUnsafeDirectoryAndWritesNothing(t *testing.T) {
	opts := placing(t, options(t, "darwin"), "connector v1")
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	opts.Root = root
	opts.Euid = func() int { return 0 }
	seedEnrollment(t, opts.SourceStateDir)
	bin := filepath.Join(root, "usr", "local", "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(bin, 0o777); err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{}

	err = Install(context.Background(), opts, runner)
	if err == nil || !strings.Contains(err.Error(), bin+":") {
		t.Fatalf("Install error = %v, want a refusal naming %s", err, bin)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("a refused install ran commands: %v", runner.argv())
	}
	for _, path := range []string{
		filepath.Join(bin, "gentility"),
		filepath.Join(root, "Library"),
		opts.StateDir,
		opts.LogDir,
	} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatalf("a refused install wrote %s: %v", path, err)
		}
	}
}

// Material whose ten minutes are gone cannot join anything, so staging it
// would install a unit the supervisor never restarts. The install refuses
// before it stages or writes anything, names the command that earns fresh
// material, and leaves the person's file where it is.
func TestInstallRefusesExpiredSourceMaterial(t *testing.T) {
	opts := options(t, "linux")
	opts.Interface = true
	save(t, opts.SourceStateDir, expiredEnrollment())
	runner := &fakeRunner{}

	err := Install(context.Background(), opts, runner)
	if err == nil {
		t.Fatal("Install with expired source material = nil error, want a refusal")
	}
	for _, want := range []string{"expired at 2026-09-06T10:00:00Z", opts.SourceStateDir, "gentility net join", "connector login --server"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("Install error = %v, want it to mention %q", err, want)
		}
	}
	if len(runner.calls) != 0 {
		t.Fatalf("a refused install ran commands: %v", runner.argv())
	}
	if _, err := os.Stat(opts.Root); !os.IsNotExist(err) {
		t.Fatalf("a refused install wrote under %s: %v", opts.Root, err)
	}
	if _, err := (&state.Store{Dir: opts.StateDir}).LoadEnrollment(); !state.IsNoEnrollment(err) {
		t.Fatalf("a refused install staged material: err = %v", err)
	}
	if got := loadEnrollment(t, opts.SourceStateDir); got != expiredEnrollment() {
		t.Fatalf("a refused install disturbed the source: %+v", got)
	}
}

// personsBinary is a connector in a directory the test's own account owns,
// which is what `./gentility` in a home directory is to root.
func personsBinary(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "gentility")
	if err := os.WriteFile(path, []byte("connector"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// An interface daemon runs as root. Run from a path the person can rewrite, it
// is root running whatever that person puts there next, so root's install
// refuses before it writes anything and names the two ways round it.
func TestInstallAsRootRefusesAnInterfaceDaemonOnABinaryRootDoesNotOwn(t *testing.T) {
	opts := options(t, "linux")
	opts.Interface = true
	opts.BinaryPath = personsBinary(t)
	opts.Euid = func() int { return 0 }
	seedEnrollment(t, opts.SourceStateDir)
	runner := &fakeRunner{}

	err := Install(context.Background(), opts, runner)
	if !errors.Is(err, ErrUntrustedBinary) {
		t.Fatalf("Install error = %v, want ErrUntrustedBinary", err)
	}
	for _, want := range []string{opts.BinaryPath, "use `gentility net join` instead", "--place-binary /usr/local/bin/gentility"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("Install error = %v, want it to mention %q", err, want)
		}
	}
	if len(runner.calls) != 0 {
		t.Fatalf("a refused install ran commands: %v", runner.argv())
	}
	if _, err := os.Stat(opts.Root); !os.IsNotExist(err) {
		t.Fatalf("a refused install wrote under %s: %v", opts.Root, err)
	}
	if _, err := (&state.Store{Dir: opts.StateDir}).LoadEnrollment(); !state.IsNoEnrollment(err) {
		t.Fatalf("a refused install staged material: err = %v", err)
	}
}

// A symlink is refused even when it points at a binary root owns: the unit
// runs the link, and the check would have seen only what it points at today.
// The refusal names the way round it a symlink has that a binary does not.
func TestInstallAsRootRefusesAnInterfaceDaemonOnASymlinkedBinary(t *testing.T) {
	opts := options(t, "linux")
	opts.Interface = true
	opts.BinaryPath = filepath.Join(t.TempDir(), "gentility")
	if err := os.Symlink("/usr/bin/env", opts.BinaryPath); err != nil {
		t.Fatal(err)
	}
	opts.Euid = func() int { return 0 }
	seedEnrollment(t, opts.SourceStateDir)
	runner := &fakeRunner{}

	err := Install(context.Background(), opts, runner)
	if !errors.Is(err, ErrUntrustedBinary) {
		t.Fatalf("Install error = %v, want ErrUntrustedBinary", err)
	}
	for _, want := range []string{opts.BinaryPath, "symlink", "pass the path it resolves to", "use `gentility net join` instead", "--place-binary /usr/local/bin/gentility"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("Install error = %v, want it to mention %q", err, want)
		}
	}
	if len(runner.calls) != 0 {
		t.Fatalf("a refused install ran commands: %v", runner.argv())
	}
}

// A binary that is not there is not a question of trust: it is reported as
// missing, once, not refused as untrusted.
func TestInstallAsRootReportsAMissingBinaryAsMissing(t *testing.T) {
	opts := options(t, "linux")
	opts.Interface = true
	opts.BinaryPath = filepath.Join(t.TempDir(), "gentility")
	opts.Euid = func() int { return 0 }
	seedEnrollment(t, opts.SourceStateDir)
	runner := &fakeRunner{}

	err := Install(context.Background(), opts, runner)
	if !errors.Is(err, fs.ErrNotExist) || errors.Is(err, ErrUntrustedBinary) {
		t.Fatalf("Install error = %v, want a missing binary and no ErrUntrustedBinary", err)
	}
	if !strings.Contains(err.Error(), opts.BinaryPath) || strings.Count(err.Error(), "supervise:") != 1 {
		t.Fatalf("Install error = %v, want it to name the binary under one supervise: prefix", err)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("a refused install ran commands: %v", runner.argv())
	}
}

// The same binary run as itself is fine wherever root's install would not
// question it: a root-owned binary on a root-owned path, a placed copy, a
// server's daemon (which runs as the service user, not root), and an install
// that is not root's (a test's).
func TestInstallRunsTheBinaryWhereItIsWhenRootHasNothingToFear(t *testing.T) {
	for _, tc := range []struct {
		name   string
		adjust func(*Options)
	}{
		// /usr/bin/env is a regular file root owns under root-owned, real
		// directories on macOS and on Linux alike.
		{"a binary root owns", func(o *Options) { o.BinaryPath = "/usr/bin/env" }},
		{"a server's daemon", func(o *Options) { o.Interface = false }},
		{"an install that is not root's", func(o *Options) { o.Euid = func() int { return 1000 } }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts := options(t, "linux")
			opts.Interface = true
			opts.BinaryPath = personsBinary(t)
			opts.Euid = func() int { return 0 }
			tc.adjust(&opts)
			seedEnrollment(t, opts.SourceStateDir)

			if err := Install(context.Background(), opts, &fakeRunner{}); err != nil {
				t.Fatalf("Install: %v", err)
			}
		})
	}
}

// --place-binary is the fix the refusal names, so it is not refused for the
// binary's sake: the copy root owns is what the daemon runs. Root's install
// still asks the placed path's own directories, which under a test's root are
// the test's, and that is the only refusal left.
func TestInstallAsRootWithPlaceBinaryDoesNotAskAfterTheSourceBinary(t *testing.T) {
	opts := placing(t, options(t, "linux"), "connector v1")
	opts.Interface = true
	opts.Euid = func() int { return 0 }
	seedEnrollment(t, opts.SourceStateDir)

	err := Install(context.Background(), opts, &fakeRunner{})
	if errors.Is(err, ErrUntrustedBinary) {
		t.Fatalf("Install error = %v, want no question about the binary being placed", err)
	}
	if err == nil || !strings.Contains(err.Error(), "refusing to place the binary under") {
		t.Fatalf("Install error = %v, want only the placed path's own refusal", err)
	}
}

// Root reads the person's staged material only out of a real directory only
// its owner can write, only when it is a regular file that same account owns,
// and never through a symlink: a link there would have root read, and then
// remove, whatever file it named. No sudo environment is consulted — Desk
// elevates through osascript, which sets none.
func TestInstallAsRootReadsOnlyThePersonsOwnStagedFile(t *testing.T) {
	rootInstall := func(t *testing.T) Options {
		opts := options(t, "linux")
		opts.Euid = func() int { return 0 }
		return opts
	}
	staged := func(t *testing.T, opts Options) {
		t.Helper()
		if err := Install(context.Background(), opts, &fakeRunner{}); err != nil {
			t.Fatalf("Install: %v", err)
		}
		if got := loadEnrollment(t, opts.StateDir); got.EnrollmentID != enrollment().EnrollmentID {
			t.Fatalf("staged %+v, want the person's material", got)
		}
	}
	refused := func(t *testing.T, opts Options, want string) {
		t.Helper()
		err := Install(context.Background(), opts, &fakeRunner{})
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("Install error = %v, want one saying %q", err, want)
		}
		if _, err := (&state.Store{Dir: opts.StateDir}).LoadEnrollment(); !state.IsNoEnrollment(err) {
			t.Fatalf("staged something from a refused source: %v", err)
		}
	}

	t.Run("no SUDO_UID: the person's own directory and file are staged", func(t *testing.T) {
		opts := rootInstall(t)
		seedEnrollment(t, opts.SourceStateDir)
		staged(t, opts)
	})

	// Desk's Join: osascript's administrator privileges make the install root
	// with no SUDO_* variables at all, reading the person's 0700 directory and
	// 0600 file.
	t.Run("Desk's osascript elevation is staged", func(t *testing.T) {
		opts := rootInstall(t)
		seedEnrollment(t, opts.SourceStateDir)
		if err := os.Chmod(opts.SourceStateDir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(filepath.Join(opts.SourceStateDir, "enrollment.json"), 0o600); err != nil {
			t.Fatal(err)
		}
		staged(t, opts)
	})

	t.Run("a symlinked file is refused", func(t *testing.T) {
		opts := rootInstall(t)
		elsewhere := t.TempDir()
		seedEnrollment(t, elsewhere)
		if err := os.MkdirAll(opts.SourceStateDir, 0o700); err != nil {
			t.Fatal(err)
		}
		target := filepath.Join(elsewhere, "enrollment.json")
		if err := os.Symlink(target, filepath.Join(opts.SourceStateDir, "enrollment.json")); err != nil {
			t.Fatal(err)
		}
		refused(t, opts, "not a regular file")
		if _, err := os.Stat(target); err != nil {
			t.Fatalf("the file the link named was touched: %v", err)
		}
	})

	t.Run("a symlinked directory is refused", func(t *testing.T) {
		opts := rootInstall(t)
		elsewhere := t.TempDir()
		seedEnrollment(t, elsewhere)
		if err := os.Symlink(elsewhere, opts.SourceStateDir); err != nil {
			t.Fatal(err)
		}
		refused(t, opts, "not a directory")
		if _, err := os.Stat(filepath.Join(elsewhere, "enrollment.json")); err != nil {
			t.Fatalf("the file the link led to was touched: %v", err)
		}
	})

	t.Run("a group-writable directory is refused", func(t *testing.T) {
		opts := rootInstall(t)
		seedEnrollment(t, opts.SourceStateDir)
		if err := os.Chmod(opts.SourceStateDir, 0o770); err != nil {
			t.Fatal(err)
		}
		refused(t, opts, "accounts other than its owner can write to it")
	})

	t.Run("a FIFO is refused without blocking", func(t *testing.T) {
		opts := rootInstall(t)
		if err := os.MkdirAll(opts.SourceStateDir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := syscall.Mkfifo(filepath.Join(opts.SourceStateDir, "enrollment.json"), 0o600); err != nil {
			t.Fatal(err)
		}
		refused(t, opts, "not a regular file")
	})
}

// A file owned by another account than the directory it sits in is refused —
// root's included, since on macOS a person can hardlink a root-owned file into
// their own directory. A test cannot chown, so the owners are stated.
func TestOwnedByRefusesAFileAnotherAccountThanTheDirectorysOwns(t *testing.T) {
	const person = 501
	if err := ownedBy("enrollment.json", ownedInfo{uid: person}, person); err != nil {
		t.Fatalf("ownedBy the directory's own account = %v, want nil", err)
	}
	for _, uid := range []uint32{0, person + 1} {
		err := ownedBy("enrollment.json", ownedInfo{uid: uid}, person)
		if err == nil || !strings.Contains(err.Error(), "whose directory it is in") {
			t.Fatalf("ownedBy a file of uid %d = %v, want a refusal", uid, err)
		}
	}
}

// ownedInfo is a regular file's metadata naming the owner a test says.
type ownedInfo struct{ uid uint32 }

func (ownedInfo) Name() string       { return "enrollment.json" }
func (ownedInfo) Size() int64        { return 0 }
func (ownedInfo) Mode() fs.FileMode  { return 0o600 }
func (ownedInfo) ModTime() time.Time { return time.Time{} }
func (ownedInfo) IsDir() bool        { return false }
func (i ownedInfo) Sys() any         { return &syscall.Stat_t{Uid: i.uid} }

// CheckPlacement is what `net join` asks before it fetches anything: a
// directory the person owns above the placed path is refused, by metadata
// alone, and root's own directories pass.
func TestCheckPlacementRefusesADirectoryAnotherAccountOwns(t *testing.T) {
	if err := CheckPlacement("/usr/bin/gentility-not-there"); err != nil {
		t.Fatalf("CheckPlacement under /usr/bin = %v, want nil", err)
	}
	if os.Getuid() == 0 {
		t.Skip("root owns the temporary directory")
	}
	err := CheckPlacement(filepath.Join(t.TempDir(), "bin", "gentility"))
	if err == nil || !strings.Contains(err.Error(), "refusing to place the binary under") {
		t.Fatalf("CheckPlacement under the test's directory = %v, want a refusal", err)
	}
}
