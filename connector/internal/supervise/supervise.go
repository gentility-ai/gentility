// Package supervise renders the supervisor units that keep `connector serve`
// running — across a reboot, a crash, and a binary swap.
//
// A server connector is not a command someone runs; it is a process the
// machine owes the organization. So it belongs to the machine's own
// supervisor — launchd on macOS, systemd on Linux — which starts it at boot
// and starts it again when it goes away for a reason worth retrying. The node
// itself never re-execs: it swaps its binary, exits with exitcodes.Restart,
// and the supervisor brings the new binary up. That code is owned by the
// exitcodes package, which reserved it for exactly this; nothing here defines
// an exit code — it only reads them, to know which exits are refusals a
// restart would only repeat.
//
// Everything in this package is a pure renderer: a Unit in, the text of a
// unit file out, no I/O and no exec. Writing that text to
// /Library/LaunchDaemons or /etc/systemd/system and running
// `launchctl bootstrap` or `systemctl enable --now` is the impure half, and
// lives in Install/Uninstall (to come). Keeping the two apart is what lets
// the shape of the units be checked against golden files on any machine,
// including one that has neither supervisor.
package supervise

import (
	"fmt"
	"path/filepath"
)

// Unit is what a supervisor needs to know about one connector: the binary to
// run, the state directory to run it against, the name to file it under, and
// the directory its output goes to.
type Unit struct {
	// BinaryPath is the absolute path of the connector binary.
	BinaryPath string
	// StateDir is the absolute path passed to `serve --state-dir`.
	StateDir string
	// Label names the service — the launchd Label, and the identifier the
	// systemd description and the log file names carry.
	Label string
	// LogDir is the absolute directory the connector's stdout and stderr go
	// to, and which it is allowed to write to under systemd's hardening.
	LogDir string
	// Interface runs the connector with a real network interface — `serve
	// --interface` — which is a person's machine joining the overlay as itself
	// rather than a server reaching it from userspace. Creating an interface
	// and pointing the machine's DNS at the overlay is root's work, so the unit
	// gives up the hardening that would forbid it.
	Interface bool
}

// validate refuses a Unit that would render a unit file the supervisor
// accepts but the operator did not mean: a relative binary path starts
// whatever is in the supervisor's working directory, and an empty directory
// or label renders a path like "/out.log".
func (u Unit) validate() error {
	for _, f := range []struct {
		name, value string
		abs         bool
	}{
		{"BinaryPath", u.BinaryPath, true},
		{"StateDir", u.StateDir, true},
		{"LogDir", u.LogDir, true},
		{"Label", u.Label, false},
	} {
		if f.value == "" {
			return fmt.Errorf("supervise: %s is empty", f.name)
		}
		if f.abs && !filepath.IsAbs(f.value) {
			return fmt.Errorf("supervise: %s must be an absolute path, got %q", f.name, f.value)
		}
	}
	return nil
}

// args is the command the supervisor runs, as its separate words.
func (u Unit) args() []string {
	args := []string{u.BinaryPath, "serve", "--state-dir", u.StateDir}
	if u.Interface {
		args = append(args, "--interface")
	}
	return args
}

// stdoutPath and stderrPath are where the connector's output lands. systemd
// keeps its own journal; launchd does not, so it needs the files.
func (u Unit) stdoutPath() string { return filepath.Join(u.LogDir, u.Label+".out.log") }
func (u Unit) stderrPath() string { return filepath.Join(u.LogDir, u.Label+".err.log") }
