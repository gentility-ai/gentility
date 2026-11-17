package supervise

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/gentility-ai/gentility/connector/internal/enroll"
	"github.com/gentility-ai/gentility/connector/internal/exitcodes"
	"github.com/gentility-ai/gentility/connector/internal/state"
)

// The defaults an install takes when nobody says otherwise.
//
// The state and log directories are the system's, not the person's. A daemon
// that kept its identity under someone's home directory would be a daemon that
// stops working when that account is locked, and a daemon whose secrets live
// wherever that home is backed up to. /var/lib and /var/log are where a Unix
// machine keeps the things its services own, on both platforms this installs
// on.
const (
	DefaultLabel    = "ai.gentility.connector"
	DefaultStateDir = "/var/lib/gentility"
	DefaultLogDir   = "/var/log/gentility"

	// serviceUser is the account the systemd unit names. It exists so the
	// connector is not root on Linux; on macOS launchd daemons run as root and
	// there is no equivalent step.
	serviceUser = "gentility"
)

// Runner runs one command. It is the whole of this package's contact with the
// machine's supervisor, and it is an interface so that a test can watch an
// install without a launchctl, a systemctl or a useradd anywhere in sight.
type Runner interface {
	Run(ctx context.Context, name string, args ...string) error
}

// Exec is the Runner that actually runs things. Output goes to Stderr — both
// streams, because systemctl and launchctl say the useful half on either one
// and the person who typed `connector install` is the one who needs to read it.
type Exec struct {
	Stderr io.Writer
}

func (e Exec) Run(ctx context.Context, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout, cmd.Stderr = e.Stderr, e.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s: %w", strings.Join(append([]string{name}, args...), " "), err)
	}
	return nil
}

// outputRunner is a Runner that can also hand back what a command printed.
// Install reads one answer that way — launchd's page about a loaded label —
// and a Runner that cannot is probed for its exit status alone.
type outputRunner interface {
	Output(ctx context.Context, name string, args ...string) (string, error)
}

// Output runs the command for what it prints, both streams, and says nothing
// to Stderr: this is a question Install asks the supervisor, not something
// the person running install asked to see.
func (e Exec) Output(ctx context.Context, name string, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("%s: %w", strings.Join(append([]string{name}, args...), " "), err)
	}
	return string(out), nil
}

// Options is one installation.
//
// Everything is optional: the zero Options installs this binary under the
// default label against the default directories, which is what
// `connector install` on a real server means.
type Options struct {
	// BinaryPath is the connector to supervise. Empty means this one.
	BinaryPath string
	// StateDir is what the daemon runs against — `serve --state-dir`.
	StateDir string
	// LogDir is where its output goes.
	LogDir string
	// Label names the service.
	Label string
	// SourceStateDir is where `connector login --server` put the enrollment,
	// which is the person's state directory and not the daemon's. Install
	// carries it across; see handoffEnrollment. Empty means the directory
	// sourceStateDir works out from $SUDO_USER.
	SourceStateDir string
	// Out receives the summary of what was written and activated.
	Out io.Writer

	// GOOS chooses the supervisor. Empty means this machine's, which is the
	// only thing a real install ever wants; naming it is how a test on a
	// laptop can check the Linux path.
	GOOS string
	// Root is the filesystem the unit file is written under — DESTDIR, in the
	// packaging sense. Empty means /, which is the only thing a real install
	// ever wants; a test points it at a temporary directory so that checking
	// an install does not need /Library or /etc.
	Root string
	// LookupUser answers who $SUDO_USER is. Empty means user.Lookup, which is
	// the only thing a real install ever wants; naming it is how a test asks
	// about an account without a passwd database anywhere in sight.
	LookupUser func(username string) (*user.User, error)
	// Now says what time it is, which is what decides whether an enrollment
	// already staged for the daemon is still worth keeping. Empty means
	// time.Now, which is the only thing a real install ever wants; naming it
	// is how a test dates its own material.
	Now func() time.Time
	// LookupEnv answers the OVERLAY_* join environment an unattended install
	// is handed — see environmentOffer. Empty means os.LookupEnv, which is the
	// only thing a real install ever wants; naming it is how a test hands one
	// install an environment without every other test in the package racing
	// with the process's.
	LookupEnv func(string) (string, bool)
	// ReplaceIdentity discards the identity this state directory already holds
	// before staging the new material — `--replace`. Without it, an install
	// offered a fresh enrollment for a machine that already has a node refuses,
	// because choosing between two identities on an operator's behalf means
	// ending one of them.
	ReplaceIdentity bool
	// Interface installs the daemon to run with a real network interface —
	// `--interface`, for a person's machine. The daemon then runs as root on
	// both platforms, so on Linux there is no service user to make.
	Interface bool
	// PlaceBinary copies this binary to the path it names, under Root, and
	// runs the daemon from there — `--place-binary`. Desk installs from inside
	// its own bundle, which the person owns; a root daemon run from there is
	// one that person can rewrite, so the daemon runs a copy root owns.
	PlaceBinary string
	// Executable answers where this binary is. Empty means os.Executable,
	// which is the only thing a real install ever wants; naming it is how a
	// test places a binary of its own making.
	Executable func() (string, error)
	// Euid answers who is running the install. Empty means os.Geteuid, which
	// is the only thing a real install ever wants; naming it is how a test
	// asks what root's install would refuse without being root.
	Euid func() int
}

// ErrPlaceBinaryPath is a --place-binary that is not an absolute, clean path.
// The unit runs the path as written, so it must be the path whose directories
// were checked.
var ErrPlaceBinaryPath = errors.New("supervise: --place-binary must be an absolute path with no . or .. and no trailing slash")

// ErrBinaryAndPlaceBinary is an install asked both to supervise a binary where
// it is and to copy this one somewhere else. It cannot do both, and choosing
// would be guessing which the operator meant.
var ErrBinaryAndPlaceBinary = errors.New("supervise: --binary and --place-binary name two different binaries; pass one")

// ErrUntrustedBinary is root's interface install asked to run a binary from a
// path another account could change. The daemon runs as root, so whoever can
// replace that file, or rename it out of one of its directories, chooses what
// root runs next.
var ErrUntrustedBinary = errors.New("supervise: an interface daemon runs as root, and refuses a binary another account could replace")

// joinAgain is what earns fresh material, for a person's machine and for a
// server, as every refusal over missing or dead material says it.
const joinAgain = "run `gentility net join` (a server: `connector login --server`)"

func (o Options) withDefaults() Options {
	if o.Label == "" {
		o.Label = DefaultLabel
	}
	if o.StateDir == "" {
		o.StateDir = DefaultStateDir
	}
	if o.LogDir == "" {
		o.LogDir = DefaultLogDir
	}
	if o.LookupUser == nil {
		o.LookupUser = user.Lookup
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.LookupEnv == nil {
		o.LookupEnv = os.LookupEnv
	}
	if o.Executable == nil {
		o.Executable = os.Executable
	}
	if o.Euid == nil {
		o.Euid = os.Geteuid
	}
	if o.GOOS == "" {
		o.GOOS = runtime.GOOS
	}
	// Spelled out rather than left empty: filepath.Join("", "/etc/…") cleans
	// to a relative path, and a unit file written relative to the working
	// directory is a unit file no supervisor will ever read.
	if o.Root == "" {
		o.Root = "/"
	}
	if o.Out == nil {
		o.Out = io.Discard
	}
	return o
}

// unit is what the renderer needs, with the binary resolved.
//
// os.Executable answers through whatever the person typed, and /usr/local/bin
// is very often a symlink into a versioned directory or a Homebrew cellar. A
// unit naming the link is a unit that silently follows the link somewhere else
// the next time it is repointed, so the link is resolved once, here, and the
// unit records where the binary actually is.
func (o Options) unit() (Unit, error) {
	binary := o.BinaryPath
	switch {
	case o.PlaceBinary != "":
		binary = o.PlaceBinary
	case binary == "":
		var err error
		if binary, err = o.self(); err != nil {
			return Unit{}, err
		}
	default:
		if abs, err := filepath.Abs(binary); err == nil {
			binary = abs
		}
	}

	return Unit{BinaryPath: binary, StateDir: o.StateDir, Label: o.Label, LogDir: o.LogDir, Interface: o.Interface}, nil
}

// self is this binary, with its symlinks resolved.
func (o Options) self() (string, error) {
	exe, err := o.Executable()
	if err != nil {
		return "", fmt.Errorf("supervise: finding this binary: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(exe)
	if err != nil {
		return "", fmt.Errorf("supervise: resolving %s: %w", exe, err)
	}
	return resolved, nil
}

// placeBinary copies this binary to PlaceBinary under Root, and says whether
// the bytes there changed — the daemon still running the old ones is then
// owed a restart. Content decides, not mtime: Desk places the binary on every
// join, and the same bytes placed again are no reason to cut a healthy
// daemon's connections.
//
// The copy is written beside the target and renamed over it, so a daemon
// starting at any moment finds either the old binary or the new one, whole.
// It is renamed in even when the bytes are the same, so a file someone else
// left at the path with the right bytes and the wrong owner is not kept. It is
// owned by root when root is running the install; a test is not root, and the
// ownership is the only part of the copy it cannot check.
func placeBinary(o Options) (bool, error) {
	source, err := o.self()
	if err != nil {
		return false, err
	}
	target := filepath.Join(o.Root, o.PlaceBinary)

	// Lstat, not Stat: a symlink at the target pointing at this very binary
	// would otherwise pass for it, and the daemon would run the person's copy
	// as root. Only a regular file is the binary in place; anything else is
	// replaced by the rename, and counts as a change.
	var before [sha256.Size]byte
	existing, err := os.Lstat(target)
	switch {
	case err == nil && existing.Mode().IsRegular():
		if a, err := os.Stat(source); err == nil && os.SameFile(a, existing) {
			return false, nil
		}
		if before, err = copyHashing(io.Discard, target); err != nil {
			return false, fmt.Errorf("supervise: reading %s: %w", target, err)
		}
	case err != nil && !errors.Is(err, fs.ErrNotExist):
		return false, fmt.Errorf("supervise: reading %s: %w", target, err)
	}

	dir := filepath.Dir(target)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return false, fmt.Errorf("supervise: creating %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(target)+".*")
	if err != nil {
		return false, fmt.Errorf("supervise: placing %s: %w", target, err)
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()

	after, err := copyHashing(tmp, source)
	if err != nil {
		return false, fmt.Errorf("supervise: copying %s to %s: %w", source, target, err)
	}
	if err := tmp.Chmod(0o755); err != nil {
		return false, fmt.Errorf("supervise: placing %s: %w", target, err)
	}
	if o.Euid() == 0 {
		if err := tmp.Chown(0, 0); err != nil {
			return false, fmt.Errorf("supervise: placing %s: %w", target, err)
		}
	}
	if err := tmp.Sync(); err != nil {
		return false, fmt.Errorf("supervise: placing %s: %w", target, err)
	}
	if err := tmp.Close(); err != nil {
		return false, fmt.Errorf("supervise: placing %s: %w", target, err)
	}
	if err := os.Rename(tmp.Name(), target); err != nil {
		return false, fmt.Errorf("supervise: placing %s: %w", target, err)
	}
	return after != before, nil
}

// dirOwner is one directory above a placed binary: who owns it, who may write
// to it, and whether it is a symlink rather than a directory.
type dirOwner struct {
	path string
	uid  uint32
	mode fs.FileMode
}

// ancestry is every parent from dir up to /, as written: each is Lstat'd, so a
// symlink is reported as one and not followed. The unit runs the path as
// written, and a symlink anywhere along it can be repointed later by whoever
// owns the directory it sits in. A dir that does not exist yet starts the
// chain at its first ancestor that does: what MkdirAll makes beneath that is
// root's.
func ancestry(dir string) ([]dirOwner, error) {
	var chain []dirOwner
	for {
		info, err := os.Lstat(dir)
		switch {
		case err == nil:
			sys, ok := info.Sys().(*syscall.Stat_t)
			if !ok {
				return nil, fmt.Errorf("supervise: %s has no owner this machine reports", dir)
			}
			chain = append(chain, dirOwner{path: dir, uid: sys.Uid, mode: info.Mode()})
		case !errors.Is(err, fs.ErrNotExist) || len(chain) > 0:
			return nil, fmt.Errorf("supervise: reading %s: %w", dir, err)
		}
		if dir == filepath.Dir(dir) {
			return chain, nil
		}
		dir = filepath.Dir(dir)
	}
}

// trustedForRoot refuses a chain with any directory another account could
// rename the binary out of: one not owned by root, or writable by its group or
// by everyone. A sticky bit does not help — it stops renaming another
// account's files, and the attacker only needs to remove theirs. It refuses a
// symlink anywhere in the chain too, since the path the unit runs is only the
// path that was checked while nothing along it is repointed. This is sshd's
// StrictModes, asked of the directories a root daemon's binary sits in.
//
// The refusal starts with the path, so each caller says what it was refusing.
func trustedForRoot(chain []dirOwner) error {
	for _, d := range chain {
		switch {
		case d.mode&fs.ModeSymlink != 0:
			return fmt.Errorf("%s: it is a symlink, which could be repointed at a directory root does not own", d.path)
		case d.uid != 0:
			return fmt.Errorf("%s: it is owned by uid %d, not root, so that account could replace what the daemon runs", d.path, d.uid)
		case d.mode.Perm()&0o022 != 0:
			return fmt.Errorf("%s: it is %v, writable by accounts other than root, so they could replace what the daemon runs", d.path, d.mode.Perm())
		}
	}
	return nil
}

// CheckPlacement says whether root may place a binary at path: every directory
// above it must pass trustedForRoot. It only reads metadata, so anyone may ask
// it — `net join` does, as the person, before it fetches material it would
// otherwise spend on an install that refuses (an Intel Mac whose Homebrew gave
// the person /usr/local/bin is the case in point).
func CheckPlacement(path string) error {
	chain, err := ancestry(filepath.Dir(path))
	if err != nil {
		return err
	}
	if err := trustedForRoot(chain); err != nil {
		return fmt.Errorf("supervise: refusing to place the binary under %w", err)
	}
	return nil
}

// ownedByRoot refuses a binary root's daemon could not run safely where it
// stands: the file itself must be a regular file root owns that no one else
// may write, and every directory above it must pass trustedForRoot. Lstat,
// not Stat: a symlink is a file whose target the check never saw.
//
// Only a refusal is an ErrUntrustedBinary. A binary that cannot be read at
// all is not a question of trust, and is returned as it is.
func ownedByRoot(binary string) error {
	info, err := os.Lstat(binary)
	if err != nil {
		return fmt.Errorf("supervise: reading %s: %w", binary, err)
	}
	sys, ok := info.Sys().(*syscall.Stat_t)
	switch {
	case info.Mode()&fs.ModeSymlink != 0:
		return untrusted(fmt.Errorf("%s: it is a symlink, which could be repointed, so pass the path it resolves to", binary))
	case !info.Mode().IsRegular():
		return untrusted(fmt.Errorf("%s: it is not a regular file, so what it runs could be repointed", binary))
	case !ok:
		return fmt.Errorf("supervise: %s has no owner this machine reports", binary)
	}
	chain, err := ancestry(filepath.Dir(binary))
	if err != nil {
		return err
	}
	if err := trustedForRoot(append([]dirOwner{{path: binary, uid: sys.Uid, mode: info.Mode()}}, chain...)); err != nil {
		return untrusted(err)
	}
	return nil
}

// untrusted is ownedByRoot's refusal, with the two ways round it.
func untrusted(err error) error {
	return fmt.Errorf("%w: %w — use `gentility net join` instead, or pass --place-binary /usr/local/bin/gentility", ErrUntrustedBinary, err)
}

// copyHashing copies the file at path into w and returns the SHA-256 of what
// it copied, in the one read.
func copyHashing(w io.Writer, path string) ([sha256.Size]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return [sha256.Size]byte{}, err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(w, h), f); err != nil {
		return [sha256.Size]byte{}, err
	}
	return [sha256.Size]byte(h.Sum(nil)), nil
}

// platform is one machine's supervisor, reduced to the four things an install
// does with it: where the unit file goes, how to render it, and the commands
// that make it live and make it stop.
type platform struct {
	unitPath   string
	render     func(Unit) (string, error)
	activate   [][]string
	deactivate [][]string
	// restart is what it takes to put a daemon that is ALREADY RUNNING onto
	// material that has just changed under it.
	//
	// Neither activation does that. `systemctl enable --now` starts a stopped
	// unit and is a no-op on a live one, and launchd's bootstrap is the same —
	// so an install that staged an enrollment and stopped at activate would
	// leave a process serving the identity it booted with, in memory, over a
	// state directory that no longer agrees with it. The pre-auth key in the
	// new material would burn its ten minutes waiting to be read.
	//
	// A changed unit is the same problem one level up: `--interface` changes
	// who the daemon runs as and what it may write, and a process started
	// under the old unit keeps the old answers until it is started again. So
	// an install runs this when it staged material or when the unit it wrote
	// differs from the one already on disk. A reinstall whose unit is
	// unchanged and whose material is unchanged drops nothing: it has no
	// reason to cut the connections a healthy daemon is carrying.
	restart [][]string
	// unload is what a supervisor that caches a unit's definition needs before
	// it will read a changed one. launchd keeps the plist it bootstrapped,
	// through kickstart and all, until the label is booted out; systemd's
	// daemon-reload already rereads, so Linux has none. It runs only when the
	// unit changed, and its failure is noted rather than fatal: a label that
	// was not loaded has nothing to unload.
	unload [][]string
	// loaded asks the supervisor whether the label is already loaded, and is
	// empty where the activation is safe to repeat. launchd's bootstrap is
	// not: on a loaded label it fails ("Bootstrap failed: 5: Input/output
	// error"), so a reinstall of an unchanged plist would refuse. A label that
	// is loaded and unchanged needs no bootstrap, only the restart when one is
	// owed. systemd's enable --now repeats harmlessly, so Linux has none.
	//
	// Its answer is read for one more thing: whether the loaded label is one
	// the supervisor has given up spawning. launchd registers a daemon with
	// Background Task Management and keeps that registration in the loaded
	// job; when the binary under it is swapped, BTM can re-register the item
	// while the job keeps the old record, and from then on every spawn fails
	// before the binary runs ("job state = spawn failed", exit 78 EX_CONFIG),
	// thirty seconds apart, forever. A kickstart repeats the refusal. Only
	// unloading the label and bootstrapping it again registers it afresh, so
	// a label in that state is treated as if its unit had changed.
	loaded []string
	// needsServiceUser is Linux's: the systemd unit names User=gentility, so
	// the install owes that user's existence. launchd daemons run as root.
	needsServiceUser bool
}

func platformFor(o Options) (platform, error) {
	switch o.GOOS {
	case "darwin":
		// A system daemon, not a per-user agent: the connector is the
		// machine's obligation to the organization and must be up before
		// anyone logs in. bootstrap/bootout are the modern spellings —
		// load/unload have been deprecated since launchd's domain rework.
		plist := filepath.Join(o.Root, "/Library/LaunchDaemons", o.Label+".plist")
		return platform{
			unitPath:   plist,
			render:     RenderLaunchd,
			activate:   [][]string{{"launchctl", "bootstrap", "system", plist}},
			deactivate: [][]string{{"launchctl", "bootout", "system/" + o.Label}},
			// kickstart -k is launchd's restart: -k kills the running instance
			// first, and on a service that is merely loaded it starts it.
			restart: [][]string{{"launchctl", "kickstart", "-k", "system/" + o.Label}},
			unload:  [][]string{{"launchctl", "bootout", "system/" + o.Label}},
			loaded:  []string{"launchctl", "print", "system/" + o.Label},
		}, nil

	case "linux":
		name := serviceName(o.Label)
		// daemon-reload before enable, because systemd will not see a unit
		// file written a moment ago until it rereads the directory.
		return platform{
			unitPath:         filepath.Join(o.Root, "/etc/systemd/system", name+".service"),
			render:           RenderSystemd,
			activate:         [][]string{{"systemctl", "daemon-reload"}, {"systemctl", "enable", "--now", name}},
			deactivate:       [][]string{{"systemctl", "disable", "--now", name}},
			restart:          [][]string{{"systemctl", "restart", name}},
			needsServiceUser: true,
		}, nil

	default:
		return platform{}, fmt.Errorf(
			"supervise: %s has no supervisor this connector installs into — run `connector serve --state-dir %s` under your own, restarting it on any exit; exit code %d is the node saying it updated itself and wants the new binary started",
			o.GOOS, o.StateDir, exitcodes.Restart)
	}
}

// UnitInstalled says whether the connector's unit file for label is on disk
// under root on goos, the mark `install` leaves that this machine has joined.
// A goos with no supervisor has no unit, so it answers false.
func UnitInstalled(goos, root, label string) bool {
	p, err := platformFor(Options{GOOS: goos, Root: root, Label: label})
	if err != nil {
		return false
	}
	_, err = os.Stat(p.unitPath)
	return err == nil
}

// UnitIsInterface says whether the unit installed for label runs the daemon in
// interface mode, which is what owes a cleanup of the system's resolver once
// it stops. No unit, or one this machine cannot read, is not.
func UnitIsInterface(goos, root, label string) bool {
	p, err := platformFor(Options{GOOS: goos, Root: root, Label: label})
	if err != nil {
		return false
	}
	text, err := os.ReadFile(p.unitPath)
	return err == nil && strings.Contains(string(text), "--interface")
}

// serviceName turns the reverse-DNS launchd label into the name systemd files
// a unit under: ai.gentility.connector is gentility-connector.service, which
// is what the golden unit is called and what an operator types after
// `systemctl status`. The leading segment is the DNS suffix and says nothing
// about the service, so it goes; the rest is joined with hyphens.
func serviceName(label string) string {
	parts := strings.Split(label, ".")
	if len(parts) > 2 {
		parts = parts[1:]
	}
	return strings.Join(parts, "-")
}

// Install puts the connector under this machine's supervisor and leaves it
// running.
//
// The order is the order of what cannot be undone: everything that can refuse
// refuses before anything is written, so a machine that is not enrolled or not
// supported ends the command exactly as it began it.
func Install(ctx context.Context, opts Options, runner Runner) error {
	opts = opts.withDefaults()
	if opts.BinaryPath != "" && opts.PlaceBinary != "" {
		return ErrBinaryAndPlaceBinary
	}
	if opts.PlaceBinary != "" && (!filepath.IsAbs(opts.PlaceBinary) || filepath.Clean(opts.PlaceBinary) != opts.PlaceBinary) {
		return fmt.Errorf("%w: got %q", ErrPlaceBinaryPath, opts.PlaceBinary)
	}

	p, err := platformFor(opts)
	if err != nil {
		return err
	}
	unit, err := opts.unit()
	if err != nil {
		return err
	}
	// Rendering is also the validation — a relative binary path or an empty
	// directory is refused here, before the first mkdir.
	text, err := p.render(unit)
	if err != nil {
		return err
	}

	// Only root's install is placing a binary for a root daemon, and only root
	// can own the directories it asks for. An unprivileged run is a test.
	if opts.PlaceBinary != "" && opts.Euid() == 0 {
		if err := CheckPlacement(filepath.Join(opts.Root, opts.PlaceBinary)); err != nil {
			return err
		}
	}
	// An interface daemon is root's, so root's install will not run it from
	// where it stands unless root alone could have put it there. --place-binary
	// answers that by running a copy root owns instead; a server's daemon runs
	// as the service user and is not asked.
	if opts.Interface && opts.PlaceBinary == "" && opts.Euid() == 0 {
		if err := ownedByRoot(unit.BinaryPath); err != nil {
			return err
		}
	}

	// Read before anything is written, so that whether the unit changed is
	// decided against what the supervisor was last given. No unit at all is a
	// first install, which activation alone starts.
	previous, err := os.ReadFile(p.unitPath)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("supervise: reading %s: %w", p.unitPath, err)
	}
	changed := err == nil && string(previous) != text

	handoff, err := handoffEnrollment(opts)
	if err != nil {
		return err
	}

	var placed bool
	if opts.PlaceBinary != "" {
		if placed, err = placeBinary(opts); err != nil {
			return err
		}
	}

	// The log directory exists before the service user does, because a
	// userspace install hands it to that user.
	if err := os.MkdirAll(opts.LogDir, 0o755); err != nil {
		return fmt.Errorf("supervise: creating %s: %w", opts.LogDir, err)
	}
	// An interface unit runs as root and names no user, so it is owed none.
	// It is owed root's directories instead: a machine switched from a
	// userspace install has them owned by the service user, and a root daemon
	// writing into a directory another account owns is one that account can
	// swap a symlink into.
	if p.needsServiceUser {
		owner := ensureServiceUser
		if opts.Interface {
			owner = ownByRoot
		}
		if err := owner(ctx, opts, runner); err != nil {
			return err
		}
	}

	if err := os.MkdirAll(filepath.Dir(p.unitPath), 0o755); err != nil {
		return fmt.Errorf("supervise: creating %s: %w", filepath.Dir(p.unitPath), err)
	}
	// 0644: launchd refuses a plist anyone but root can write, and systemd
	// expects the same of a unit.
	if err := os.WriteFile(p.unitPath, []byte(text), 0o644); err != nil {
		return fmt.Errorf("supervise: writing %s: %w", p.unitPath, err)
	}

	activate, wedged, err := activateUnit(ctx, runner, p, opts.Label, changed, len(handoff) > 0 || placed, opts.Out)
	if err != nil {
		return err
	}

	if placed {
		fmt.Fprintf(opts.Out, "placed this binary at %s\n", unit.BinaryPath)
	}
	fmt.Fprintf(opts.Out, "wrote %s\n", p.unitPath)
	for _, line := range handoff {
		fmt.Fprintln(opts.Out, line)
	}
	fmt.Fprintf(opts.Out, "state %s, logs %s\n", opts.StateDir, opts.LogDir)
	for _, argv := range activate {
		fmt.Fprintf(opts.Out, "ran %s\n", strings.Join(argv, " "))
	}
	if len(handoff) > 0 {
		fmt.Fprintf(opts.Out, "restarted %s, so it comes up on the enrollment just staged rather than on whatever it was already holding\n", opts.Label)
	}
	if placed {
		fmt.Fprintf(opts.Out, "restarted %s, so it runs the binary just placed rather than the one it was started from\n", opts.Label)
	}
	if changed {
		fmt.Fprintf(opts.Out, "restarted %s, so it runs as the unit just written says rather than as the one it was started under\n", opts.Label)
	}
	if wedged {
		fmt.Fprintf(opts.Out, "bootstrapped %s afresh, so the supervisor spawns it again rather than refusing every thirty seconds\n", opts.Label)
	}
	fmt.Fprintf(opts.Out, "%s is now supervised and will come back after a reboot, a crash and a binary swap\n", opts.Label)
	return nil
}

// loadedState is the supervisor's answer about a label: not held, held and
// spawnable, or held but given up on.
type loadedState int

const (
	notLoaded loadedState = iota
	loadedLive
	loadedWedged
)

// probe asks the supervisor about a label. A runner that can hand back what
// the command printed is read for launchd's job state; any other is read for
// its exit status alone, and the page launchctl print answers with — detail
// the person running install did not ask for — is never shown to them.
func probe(ctx context.Context, runner Runner, argv []string) loadedState {
	if r, ok := runner.(outputRunner); ok {
		out, err := r.Output(ctx, argv[0], argv[1:]...)
		switch {
		case err != nil:
			return notLoaded
		case strings.Contains(out, "job state = spawn failed"):
			return loadedWedged
		default:
			return loadedLive
		}
	}
	if runner.Run(ctx, argv[0], argv[1:]...) != nil {
		return notLoaded
	}
	return loadedLive
}

// StopError is an Uninstall whose supervisor refused to stop the daemon. It is
// returned after everything else Uninstall does has been done, so `uninstall`
// can read it as the note it has always printed. `leave` reads it as a reason
// not to forget an identity a daemon that may still be running holds.
type StopError struct {
	// Failures are the supervisor's own answers, one per command it refused.
	Failures []error
}

func (e *StopError) Error() string {
	notes := make([]string, len(e.Failures))
	for i, f := range e.Failures {
		notes[i] = f.Error()
	}
	return "the supervisor did not stop the daemon: " + strings.Join(notes, "; ")
}

// Uninstall stops the supervision and removes the unit, and stops there.
//
// It deliberately leaves the state directory alone. Uninstalling is "stop
// running this here", and un-enrolling is "this machine is no longer ours" —
// one is a local decision and the other belongs to the organization, and a
// command that quietly did both would make the second one unrecoverable.
//
// A stop the supervisor refused is a *StopError, returned last; any other
// error ends Uninstall where it happened.
func Uninstall(ctx context.Context, opts Options, runner Runner) error {
	opts = opts.withDefaults()

	p, err := platformFor(opts)
	if err != nil {
		return err
	}

	var refused []error
	for _, argv := range p.deactivate {
		if err := runner.Run(ctx, argv[0], argv[1:]...); err != nil {
			// A supervisor asked to stop something it is not running says so
			// and exits non-zero. That is the answer to a second uninstall,
			// not a failure of this one — say it and carry on removing. Which
			// of the two it was is the caller's to judge: see StopError.
			fmt.Fprintf(opts.Out, "note: %v\n", err)
			refused = append(refused, err)
			continue
		}
		fmt.Fprintf(opts.Out, "ran %s\n", strings.Join(argv, " "))
	}

	if err := os.Remove(p.unitPath); err != nil {
		if !os.IsNotExist(err) {
			return fmt.Errorf("supervise: removing %s: %w", p.unitPath, err)
		}
		fmt.Fprintf(opts.Out, "no unit at %s\n", p.unitPath)
	} else {
		fmt.Fprintf(opts.Out, "removed %s\n", p.unitPath)
	}

	if err := os.Remove(filepath.Join(opts.StateDir, "discovery.json")); err != nil && !os.IsNotExist(err) {
		return err
	}

	fmt.Fprintf(opts.Out, "%s is left in place — this stops supervising the connector, it does not un-enrol this machine\n", opts.StateDir)
	if len(refused) > 0 {
		return &StopError{Failures: refused}
	}
	return nil
}

// sourceStateDir is the directory the handoff takes the enrollment out of.
//
// The documented flow is `connector login --server` as the person, then `sudo
// connector install` — and sudo resets HOME on very nearly every system. So
// state.DefaultDir() called inside the install answers with root's config
// directory, and the enrollment the person just earned is not in it: the
// advertised flow would refuse a machine that is in fact enrolled, on its first
// contact with anybody.
//
// $SUDO_USER is sudo's record of who typed it, and the answer is the directory
// state.DefaultDir() would have computed for that account: the same
// precedence, rooted at their home. Two things outrank the guess — an explicit
// --source-state-dir, and $GENTILITY_STATE_DIR, which only reaches a command
// through sudo if someone passed it through deliberately.
func sourceStateDir(o Options) (string, error) {
	if o.SourceStateDir != "" {
		return o.SourceStateDir, nil
	}
	invoker := os.Getenv("SUDO_USER")
	if invoker == "" || os.Getenv("GENTILITY_STATE_DIR") != "" {
		return state.DefaultDir(), nil
	}

	// A name this machine has no account for is a question, not a fallback:
	// reading root's directory instead would report "not enrolled" about a
	// directory nobody was ever going to look in.
	person, err := o.LookupUser(invoker)
	if err != nil {
		return "", fmt.Errorf(
			"supervise: $SUDO_USER names %q, which this machine has no account for — pass --source-state-dir with the directory `connector login --server` wrote to: %w",
			invoker, err)
	}
	if person.HomeDir == "" {
		return "", fmt.Errorf(
			"supervise: $SUDO_USER names %q, which has no home directory — pass --source-state-dir with the directory `connector login --server` wrote to",
			invoker)
	}
	return filepath.Join(configDir(o.GOOS, person.HomeDir), "gentility"), nil
}

// configDir is os.UserConfigDir's answer for a home directory that is not this
// process's, which is the whole trick: os.UserConfigDir reads $HOME, and under
// sudo $HOME is root's. The two branches are exactly what it does per platform.
func configDir(goos, home string) string {
	if goos == "darwin" {
		return filepath.Join(home, "Library", "Application Support")
	}
	// Unix: $XDG_CONFIG_HOME when it is set and absolute, else ~/.config. sudo
	// strips XDG_CONFIG_HOME as a rule, so a value here was passed through on
	// purpose and means what it says.
	if xdg := os.Getenv("XDG_CONFIG_HOME"); filepath.IsAbs(xdg) {
		return xdg
	}
	return filepath.Join(home, ".config")
}

// handoffEnrollment settles which enrollment the daemon is going to join on,
// puts it where the daemon will look for it, and returns the lines the summary
// says about that.
//
// There are three places material can come from, and they are tried in that
// order: the OVERLAY_* environment, an enrollment already staged for the
// daemon, and the person's state directory. The environment is first because
// it is the one an operator states at the moment of the install — an
// unattended recipe exports what the mint just returned and runs this — and
// the two directories hold whatever was left over from before.
//
// The directories are different on purpose and the gap between them is real:
// `connector login --server` happens as a person and writes into that person's
// config directory, while the daemon runs as root or as `gentility` against
// /var/lib. Nothing else in the lifetime of a connector can bridge them —
// `serve` under the supervisor only ever sees its own state directory — so the
// install is the moment, and an install that skipped it would hand the
// supervisor a service that comes up refusing to serve, forever, in a log
// nobody is reading.
func handoffEnrollment(opts Options) ([]string, error) {
	daemon := &state.Store{Dir: opts.StateDir}

	fromEnv, err := environmentOffer(opts)
	if err != nil {
		return nil, err
	}

	// An identity in node.json means the exchange already happened and the
	// enrollment it was bought with was consumed and purged. The only question
	// left is whether anybody is offering this machine a new one.
	if node, err := daemon.LoadNode(); err == nil {
		return replaceIdentity(opts, daemon, node, fromEnv)
	} else if !state.IsNotEnrolled(err) {
		return nil, err
	}

	if fromEnv != nil {
		if err := stage(opts, daemon, *fromEnv); err != nil {
			return nil, err
		}
		return []string{fromEnv.staged(opts.StateDir)}, nil
	}

	// Something is already staged where the daemon will look — including the
	// case where the person's state directory is the daemon's. Whether that
	// ends the handoff depends on whether the material is still alive.
	if staged, err := daemon.LoadEnrollment(); err == nil {
		return restage(opts, daemon, staged)
	} else if !state.IsNoEnrollment(err) {
		return nil, err
	}

	source, err := sourceStateDir(opts)
	if err != nil {
		return nil, err
	}

	enrolling, err := loadSourceEnrollment(opts, source)
	if err != nil {
		if state.IsNoEnrollment(err) {
			return nil, fmt.Errorf(
				"supervise: %w in %s, and no identity in %s — %s, "+
					"or export the OVERLAY_* variables a machine enrollment carries",
				state.ErrNoEnrollment, source, opts.StateDir, joinAgain)
		}
		return nil, err
	}
	// Dead material staged is a unit that comes up refusing to serve, which the
	// supervisor will not restart into. The person's file is left where it is:
	// it is their directory, and `net join` removes its own staging anyway.
	if !unexpired(enrolling, opts.Now()) {
		return nil, fmt.Errorf("supervise: the enrollment in %s expired at %s — %s",
			source, enrolling.ExpiresAt, joinAgain)
	}

	offered := offer{enrollment: enrolling, from: source, origin: source}
	if err := stage(opts, daemon, offered); err != nil {
		return nil, err
	}
	return []string{offered.staged(opts.StateDir)}, nil
}

// offer is fresh enrollment material and where an install got it.
type offer struct {
	enrollment state.Enrollment
	// from is the directory the material has to be cleared out of once it has
	// been staged, and is empty when there is nothing to clear: material read
	// out of the environment was never on disk, and material already sitting in
	// the daemon's own directory is where it belongs.
	from string
	// origin is how the summary and the refusal name the source — a path, or
	// the environment.
	origin string
}

// maxSourceEnrollment bounds what root reads out of the person's directory.
// Real material is a few hundred bytes.
const maxSourceEnrollment = 64 << 10

// loadSourceEnrollment reads the material the person staged for this install.
//
// As root it reads a file in a directory the person controls, so it reads it
// warily. Who the person is comes from the directory itself, not from sudo's
// environment: Desk elevates through osascript, which sets no $SUDO_UID. The
// directory must be a real directory, not a symlink, that only its owner can
// write; the file must be a regular file that same account owns, and it is
// opened without following a symlink. A symlink there would have root read,
// and then remove, whatever file the person pointed it at; a FIFO would hang
// the install; a root-owned file hardlinked into the person's directory would
// have root hand over a file the person cannot read. The checks are made on the
// opened descriptor too, so a file swapped between the look and the open is
// refused rather than read.
func loadSourceEnrollment(opts Options, source string) (state.Enrollment, error) {
	if opts.Euid() != 0 {
		return (&state.Store{Dir: source}).LoadEnrollment()
	}

	dir, err := os.Lstat(source)
	if errors.Is(err, fs.ErrNotExist) {
		return state.Enrollment{}, state.ErrNoEnrollment
	}
	if err != nil {
		return state.Enrollment{}, fmt.Errorf("supervise: reading %s: %w", source, err)
	}
	person, err := sourceOwner(source, dir)
	if err != nil {
		return state.Enrollment{}, err
	}

	path := filepath.Join(source, "enrollment.json")
	seen, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return state.Enrollment{}, state.ErrNoEnrollment
	}
	if err != nil {
		return state.Enrollment{}, fmt.Errorf("supervise: reading %s: %w", path, err)
	}
	if err := ownedBy(path, seen, person); err != nil {
		return state.Enrollment{}, err
	}

	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return state.Enrollment{}, fmt.Errorf("supervise: opening %s: %w", path, err)
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil {
		return state.Enrollment{}, fmt.Errorf("supervise: reading %s: %w", path, err)
	}
	if !os.SameFile(seen, opened) {
		return state.Enrollment{}, fmt.Errorf("supervise: refusing %s: it changed while it was being read", path)
	}
	if err := ownedBy(path, opened, person); err != nil {
		return state.Enrollment{}, err
	}

	data, err := io.ReadAll(io.LimitReader(f, maxSourceEnrollment+1))
	if err != nil {
		return state.Enrollment{}, fmt.Errorf("supervise: reading %s: %w", path, err)
	}
	if len(data) > maxSourceEnrollment {
		return state.Enrollment{}, fmt.Errorf("supervise: refusing %s: it is larger than any enrollment", path)
	}
	var enrollment state.Enrollment
	if err := json.Unmarshal(data, &enrollment); err != nil {
		return state.Enrollment{}, fmt.Errorf("supervise: parsing %s: %w", path, err)
	}
	if enrollment.PreauthKey == "" {
		return state.Enrollment{}, state.ErrNoEnrollment
	}
	return enrollment, nil
}

// sourceOwner answers whose the person's state directory is, refusing one root
// should not read from: a symlink, anything but a directory, or a directory
// accounts other than its owner can write.
func sourceOwner(source string, info fs.FileInfo) (uint32, error) {
	if !info.IsDir() {
		return 0, fmt.Errorf("supervise: refusing %s: it is not a directory (%v), and root reads only from the person's own", source, info.Mode().Type())
	}
	if info.Mode().Perm()&0o022 != 0 {
		return 0, fmt.Errorf("supervise: refusing %s: accounts other than its owner can write to it (%v)", source, info.Mode().Perm())
	}
	return ownerOf(source, info)
}

// ownedBy refuses a source file root should not read: anything but a regular
// file, or one owned by an account other than the directory's.
func ownedBy(path string, info fs.FileInfo, person uint32) error {
	if !info.Mode().IsRegular() {
		return fmt.Errorf("supervise: refusing %s: it is not a regular file (%v), and root reads only the file the person wrote", path, info.Mode().Type())
	}
	owner, err := ownerOf(path, info)
	if err != nil {
		return err
	}
	if owner != person {
		return fmt.Errorf("supervise: refusing %s: it is owned by uid %d, not uid %d, whose directory it is in", path, owner, person)
	}
	return nil
}

func ownerOf(path string, info fs.FileInfo) (uint32, error) {
	sys, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, fmt.Errorf("supervise: %s has no owner this machine reports", path)
	}
	return sys.Uid, nil
}

func (o offer) staged(stateDir string) string {
	return fmt.Sprintf("took enrollment %s from %s and staged it in %s", o.enrollment.EnrollmentID, o.origin, stateDir)
}

// enrollmentWindow is how long material read out of the environment is taken to
// live when OVERLAY_EXPIRES_AT does not say: ten minutes, which is the mint's
// own window — the pre-auth key it hands over is single-use and expires ten
// minutes after it was minted.
//
// It is a guess about a fact the mint knows, so it errs the only way that is
// safe. Dating the material longer than the key actually lives would make an
// install keep a corpse in preference to material a person could still go and
// earn; dating it shorter would refuse a trio minted moments ago.
const enrollmentWindow = 10 * time.Minute

// mintedVars are the OVERLAY_* variables a mint's own payload spells, which is
// exactly state.Enrollment's JSON. They are how an install tells "no
// environment" from "an environment that went wrong": any one of them set means
// somebody is running the unattended recipe, so a missing sixth is named rather
// than quietly fallen through to a directory nobody pointed this install at.
//
// The two further variables enroll.ReadEnv requires are deliberately not here:
// they are about this machine rather than about the grant, and an install
// answers them itself. See environmentOffer.
var mintedVars = []string{
	enroll.VarEnrollmentID,
	enroll.VarNonce,
	enroll.VarPreauthKey,
	enroll.VarHeadURL,
	enroll.VarExpiresAt,
	enroll.VarGatewayAddr,
	enroll.VarGatewayCA,
}

// environmentOffer reads the enrollment out of the join environment, and
// answers nil when there is no environment to read.
//
// This is the unattended install: `net_machine_enroll` mints a machine's trio,
// the recipe exports it, and `sudo -E connector install` stages it. Nobody is
// at a browser and nothing was written to a person's directory, so the
// environment is the whole of what this machine has been told.
//
// The lookup enroll.ReadEnv is given is not quite the process's. Two of the
// variables it requires are about this machine rather than about the grant —
// where the identity is kept and what the head calls the node, the pair
// enroll.EnrollmentOptions names for the same reason — and an install knows
// both: the state directory is the one it is installing the daemon against, and
// the hostname is this machine's own. A node reading its own join environment
// has nobody to ask; an install has, and making an operator export them as well
// would be making them say twice what they have already said once.
func environmentOffer(opts Options) (*offer, error) {
	var stated bool
	for _, name := range mintedVars {
		if value, _ := opts.LookupEnv(name); value != "" {
			stated = true
			break
		}
	}
	if !stated {
		return nil, nil
	}

	// An error leaves this empty, which ReadEnv reports as the variable being
	// unset — and OVERLAY_HOSTNAME is then the operator's to supply, which is
	// a better answer than a machine name invented here.
	hostname, _ := os.Hostname()

	env, err := enroll.ReadEnv(func(name string) (string, bool) {
		if value, ok := opts.LookupEnv(name); ok && value != "" {
			return value, true
		}
		switch name {
		case enroll.VarStateDir:
			return opts.StateDir, true
		case enroll.VarHostname:
			return hostname, hostname != ""
		}
		return "", false
	})
	if err != nil {
		return nil, fmt.Errorf("supervise: reading the enrollment out of the environment: %w", err)
	}

	expires, err := expiresAt(opts)
	if err != nil {
		return nil, err
	}

	return &offer{
		enrollment: state.Enrollment{
			EnrollmentID: env.EnrollmentID,
			PreauthKey:   env.PreauthKey,
			Nonce:        env.Nonce,
			HeadURL:      env.HeadURL,
			ExpiresAt:    expires,
			// Split, never sliced: the address and the port are stored beside
			// each other because an overlay address is IPv6 and "fd06:6e7:…:443"
			// is not an address and a port, it is a typo a parser will accept
			// as something else (enroll.gatewayAddrPort).
			GatewayAddr: env.GatewayAddr.Addr().String(),
			GatewayPort: int(env.GatewayAddr.Port()),
			// The PEM itself: OVERLAY_GATEWAY_CA names a file, and ReadEnv has
			// already read it.
			GatewayCA: env.GatewayCA,
		},
		origin: "the OVERLAY_* environment",
	}, nil
}

// expiresAt is when material read out of the environment stops being worth
// staging.
//
// OVERLAY_EXPIRES_AT is the mint's own answer and is passed through unchanged,
// so the daemon reads exactly what the mothership wrote. A value that is not an
// instant is a refusal rather than a reason to fall back on the default: this
// field is what restage and `serve` both decide on, and inventing ten minutes
// for something somebody typed wrong would be inventing a fuse.
func expiresAt(opts Options) (string, error) {
	value, _ := opts.LookupEnv(enroll.VarExpiresAt)
	if value == "" {
		return opts.Now().UTC().Add(enrollmentWindow).Format(time.RFC3339), nil
	}
	if _, err := time.Parse(time.RFC3339, value); err != nil {
		return "", fmt.Errorf(
			"supervise: %s is %q, which is not an RFC 3339 instant — leave it unset and this install "+
				"dates the material %s out, which is the window a mint gives it: %w",
			enroll.VarExpiresAt, value, enrollmentWindow, err)
	}
	return value, nil
}

// replaceIdentity is what an install means for a machine that has already
// exchanged.
//
// Nothing on offer is a plain reinstall of the unit: the node keeps the
// identity it earned and neither directory is touched, which is what a second
// `connector install` has always meant. Fresh material on offer is somebody
// re-enrolling this machine, and that has a cost the install will not pay
// quietly — the node the mothership knows stops existing, and every service
// approved for it goes with it. So it is refused unless `--replace` says so out
// loud, and the refusal names the node that is at stake.
func replaceIdentity(opts Options, daemon *state.Store, node state.Node, fromEnv *offer) ([]string, error) {
	offered, err := offeredEnrollment(opts, fromEnv)
	if err != nil || offered == nil {
		return nil, err
	}

	if !opts.ReplaceIdentity {
		return nil, fmt.Errorf(
			"supervise: %s already holds the identity of node %s, and %s offers a fresh enrollment — "+
				"an install will not choose between two identities: pass --replace to discard the node "+
				"this machine is and join the overlay as the one the new enrollment names",
			opts.StateDir, node.NodeID, offered.origin)
	}

	// The discard is stage's, immediately before it writes: see stage.
	if err := stage(opts, daemon, *offered); err != nil {
		return nil, err
	}
	return []string{
		fmt.Sprintf("discarded node %s, its tsnet state and its status snapshot in %s", node.NodeID, opts.StateDir),
		offered.staged(opts.StateDir),
	}, nil
}

// offeredEnrollment is the fresh material an install would have to discard this
// machine's identity to use, and nil when there is none. It is the one question
// `--replace` exists to answer.
//
// The environment first, then the person's directory — the same precedence the
// rest of the handoff runs on. Expired material is not an offer: a trio whose
// ten minutes are gone cannot join anything, so an install that refused on
// account of one would refuse a reinstall over a file nobody has tidied up.
func offeredEnrollment(opts Options, fromEnv *offer) (*offer, error) {
	if fromEnv != nil {
		return fromEnv, nil
	}

	source, err := sourceStateDir(opts)
	if err != nil {
		// Where the person's directory is cannot be worked out. With an
		// identity already in hand that is not a refusal but a reinstall with
		// nothing on offer: the message about $SUDO_USER would be about a
		// directory nobody is asking after.
		return nil, nil
	}

	enrolling, err := loadSourceEnrollment(opts, source)
	switch {
	case state.IsNoEnrollment(err):
		return nil, nil
	case err != nil:
		return nil, err
	case !unexpired(enrolling, opts.Now()):
		return nil, nil
	}

	offered := offer{enrollment: enrolling, from: source, origin: source}
	if filepath.Clean(source) == filepath.Clean(opts.StateDir) {
		// The person's directory is the daemon's: the material is already
		// where the daemon will look and there is nothing to carry across.
		offered.from = ""
	}
	return &offered, nil
}

// restage decides what to do about material already sitting in the daemon's
// state directory, and is the whole of what a second install means.
//
// A preauth key lives ten minutes. Keeping a staged enrollment unexamined
// makes the documented recovery — `serve` says the material expired, the
// person runs `connector login --server` and `sudo connector install` again —
// silently do nothing: the fresh material stays in the person's directory, the
// dead material stays in the daemon's, and the service never joins. So the
// staged copy is only kept when it is still alive and nothing newer is
// offered; anything else prefers the source's.
func restage(opts Options, daemon *state.Store, staged state.Enrollment) ([]string, error) {
	alive := unexpired(staged, opts.Now())

	source, err := sourceStateDir(opts)
	if err != nil {
		// Where the person's directory is cannot be worked out. That is a
		// refusal when the staged material is dead and there is nothing else
		// to run on, and merely unhelpful when it is alive.
		if alive {
			return nil, nil
		}
		return nil, err
	}

	// The person's state directory can be the daemon's, in which case what is
	// staged is also all that is on offer and there is nothing to carry: a
	// handoff here would clear the file it had just written.
	var offered *offer
	if filepath.Clean(source) != filepath.Clean(opts.StateDir) {
		fresher, err := loadSourceEnrollment(opts, source)
		if err != nil && !state.IsNoEnrollment(err) {
			return nil, err
		}
		if err == nil && unexpired(fresher, opts.Now()) {
			offered = &offer{enrollment: fresher, from: source, origin: source}
		}
	}

	if offered == nil {
		if alive {
			// Nothing fresher on offer, but what is staged has time on it.
			return nil, nil
		}
		return nil, fmt.Errorf(
			"supervise: the staged enrollment in %s expired at %s and %s holds no fresh one — %s",
			opts.StateDir, staged.ExpiresAt, source, joinAgain)
	}

	if err := stage(opts, daemon, *offered); err != nil {
		return nil, err
	}
	return []string{fmt.Sprintf("replaced the enrollment staged in %s with the one from %s", opts.StateDir, source)}, nil
}

// unexpired reports whether e still has time on it at now.
//
// An expires_at this connector cannot read counts as dead, on `serve`'s
// reasoning: the mothership writes an ISO 8601 instant on every enrollment, so
// a value that is not one is material this binary does not understand — and
// keeping it in preference to something readable is keeping the one the daemon
// is certain to refuse.
func unexpired(e state.Enrollment, now time.Time) bool {
	expires, err := time.Parse(time.RFC3339, e.ExpiresAt)
	return err == nil && now.Before(expires)
}

// stage writes the enrollment where the daemon will look and clears it from
// where it came, in that order: material lost between the two steps is a
// re-login, material duplicated is a second copy of two secrets.
//
// An install told to --replace discards the identity first, and only here —
// the one place fresh material is about to land. Both halves go, because the
// node key tsnet persisted is the half that survives a missing node.json, and
// a rejoin on a key the head has forgotten is exactly the refusal that sends
// an operator here (tsnode.ErrRejoinRefused). Doing it in stage rather than
// wherever --replace is read is what keeps an install that ends in a refusal
// from having wiped an identity on its way to saying no.
func stage(opts Options, daemon *state.Store, o offer) error {
	if opts.ReplaceIdentity {
		if err := daemon.ClearIdentity(); err != nil {
			return fmt.Errorf("supervise: discarding the identity in %s: %w", opts.StateDir, err)
		}
	}
	// SaveEnrollment creates the directory 0711 and the file 0600, which is
	// the whole of what the material needs.
	if err := daemon.SaveEnrollment(o.enrollment); err != nil {
		return fmt.Errorf("supervise: staging the enrollment in %s: %w", opts.StateDir, err)
	}
	if o.from == "" {
		return nil
	}
	if err := (&state.Store{Dir: o.from}).ClearEnrollment(); err != nil {
		return fmt.Errorf("supervise: clearing the enrollment from %s: %w", o.from, err)
	}
	return nil
}

// ownByRoot gives a Linux interface daemon's two directories back to root,
// recursively, as ensureServiceUser gives them to the service user.
func ownByRoot(ctx context.Context, opts Options, runner Runner) error {
	if err := runner.Run(ctx, "chown", "-R", "root:root", opts.StateDir, opts.LogDir); err != nil {
		return fmt.Errorf("supervise: giving root the interface daemon's directories: %w", err)
	}
	return nil
}

// ensureServiceUser makes the account the systemd unit names exist, and gives
// it the two directories it must write.
//
// getent is the question. Any answer but success is taken as "not there":
// getent exits 2 for a key it cannot find, and a machine with no getent at all
// cannot prove the user exists either. useradd then either creates it or says
// why it could not, which is a better error than anything invented here.
func ensureServiceUser(ctx context.Context, opts Options, runner Runner) error {
	if err := runner.Run(ctx, "getent", "passwd", serviceUser); err != nil {
		if err := runner.Run(ctx, "useradd", "--system", "--user-group", "--no-create-home", "--shell", "/usr/sbin/nologin", serviceUser); err != nil {
			return fmt.Errorf("supervise: creating the %s service user: %w", serviceUser, err)
		}
	}

	// Recursive: the state directory already holds the enrollment the handoff
	// just put there, and a daemon that cannot read its own material is a
	// daemon that never joins.
	owner := serviceUser + ":" + serviceUser
	if err := runner.Run(ctx, "chown", "-R", owner, opts.StateDir, opts.LogDir); err != nil {
		return fmt.Errorf("supervise: giving %s its directories: %w", serviceUser, err)
	}
	return nil
}

// unloadGrace bounds the wait for launchd to let go of a label it was told to
// boot out. launchd gives a stopping job 20 seconds before it kills it.
const unloadGrace = 30 * time.Second

// awaitUnloaded waits until the supervisor no longer holds the label, and says
// whether it got there within unloadGrace. launchd's bootout returns while the
// job it stopped is still exiting, and a bootstrap in that window fails ("5:
// Input/output error") and leaves the label unloaded, with no daemon and
// nothing to start one (T-450).
func awaitUnloaded(ctx context.Context, runner Runner, loaded []string) bool {
	deadline := time.Now().Add(unloadGrace)
	for probe(ctx, runner, loaded) != notLoaded {
		if time.Now().After(deadline) {
			return false
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(250 * time.Millisecond):
		}
	}
	return true
}

// activateUnit is shared by daemon and per-user installation. An unloaded
// launchd label can race bootout; activation remains the authoritative result.
func activateUnit(ctx context.Context, runner Runner, p platform, label string, changed, restart bool, out io.Writer) ([][]string, bool, error) {
	// An unchanged unit the supervisor already holds needs no bootstrap —
	// unless it holds it as a job it can no longer spawn, which only a
	// bootout and a fresh bootstrap bring back. See platform.loaded.
	activate := p.activate
	wedged := false
	if !changed && len(p.loaded) > 0 {
		switch probe(ctx, runner, p.loaded) {
		case loadedLive:
			activate = nil
		case loadedWedged:
			wedged = true
			fmt.Fprintf(out, "note: the supervisor holds %s but cannot spawn it (job state = spawn failed); unloading it and bootstrapping it afresh\n", label)
		}
	}

	if changed || wedged {
		for _, argv := range p.unload {
			if err := runner.Run(ctx, argv[0], argv[1:]...); err != nil {
				fmt.Fprintf(out, "note: %v\n", err)
			}
		}
		if len(p.unload) > 0 && len(p.loaded) > 0 && !awaitUnloaded(ctx, runner, p.loaded) {
			fmt.Fprintf(out, "note: the supervisor still holds %s %v after unloading it; bootstrapping anyway\n", label, unloadGrace)
		}
	}

	// The restart is part of the activation and not a step after it: material
	// was staged, the unit changed or the binary it names was replaced, so
	// bringing the daemon up is bringing it up on that material, that unit and
	// that binary. See platform.restart.
	if restart || changed {
		activate = append(append([][]string{}, activate...), p.restart...)
	}

	for _, argv := range activate {
		if err := runner.Run(ctx, argv[0], argv[1:]...); err != nil {
			return nil, false, fmt.Errorf("supervise: activating %s: %w", label, err)
		}
	}

	return activate, wedged, nil
}
