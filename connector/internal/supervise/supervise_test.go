package supervise

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gentility-ai/gentility/connector/internal/exitcodes"
)

// installed is the unit the golden files were rendered from. Every path is
// absolute, as a real installation's would be.
func installed() Unit {
	return Unit{
		BinaryPath: "/usr/local/bin/connector",
		StateDir:   "/var/lib/gentility",
		Label:      "ai.gentility.connector",
		LogDir:     "/var/log/gentility",
	}
}

func golden(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	return string(b)
}

func TestLaunchdPlistMatchesGolden(t *testing.T) {
	got, err := RenderLaunchd(installed())
	if err != nil {
		t.Fatalf("RenderLaunchd: %v", err)
	}
	if want := golden(t, "ai.gentility.connector.plist"); got != want {
		t.Fatalf("RenderLaunchd() =\n%s\nwant\n%s", got, want)
	}
}

func TestSystemdUnitMatchesGolden(t *testing.T) {
	got, err := RenderSystemd(installed())
	if err != nil {
		t.Fatalf("RenderSystemd: %v", err)
	}
	if want := golden(t, "gentility-connector.service"); got != want {
		t.Fatalf("RenderSystemd() =\n%s\nwant\n%s", got, want)
	}
}

// interfaceUnit is the same installation run with a real network interface. The
// goldens it renders differ from installed()'s only where the interface's need
// for root and a writable /etc says they must.
func interfaceUnit() Unit {
	u := installed()
	u.Interface = true
	return u
}

func TestLaunchdInterfacePlistMatchesGolden(t *testing.T) {
	got, err := RenderLaunchd(interfaceUnit())
	if err != nil {
		t.Fatalf("RenderLaunchd: %v", err)
	}
	if want := golden(t, "interface.plist"); got != want {
		t.Fatalf("RenderLaunchd() =\n%s\nwant\n%s", got, want)
	}
}

func TestSystemdInterfaceUnitMatchesGolden(t *testing.T) {
	got, err := RenderSystemd(interfaceUnit())
	if err != nil {
		t.Fatalf("RenderSystemd: %v", err)
	}
	if want := golden(t, "interface.service"); got != want {
		t.Fatalf("RenderSystemd() =\n%s\nwant\n%s", got, want)
	}
}

// A refusal is not a reason to start again. exitcodes says so in prose; the
// unit has to say so in a line systemd reads, and the numbers in that line
// have to be the ones the binary actually exits with — so they are asserted
// against the constants, not against literals a later edit could drift from.
func TestSystemdPreventsRestartOnRefusals(t *testing.T) {
	got, err := RenderSystemd(installed())
	if err != nil {
		t.Fatalf("RenderSystemd: %v", err)
	}

	if strings.Contains(got, "Restart=always") {
		t.Errorf("RenderSystemd() still has Restart=always:\n%s", got)
	}
	if !strings.Contains(got, "\nRestart=on-failure\n") {
		t.Errorf("RenderSystemd() has no Restart=on-failure:\n%s", got)
	}

	want := fmt.Sprintf("\nRestartPreventExitStatus=%d %d %d %d\n",
		exitcodes.Usage, exitcodes.EnrollRefused, exitcodes.MaterialExpired, exitcodes.CapverTooOld)
	if !strings.Contains(got, want) {
		t.Errorf("RenderSystemd() has no %q:\n%s", strings.TrimSpace(want), got)
	}

	// Temporary and Restart mean "start me again"; preventing them would
	// strand a node that asked to be restarted.
	for _, code := range []int{exitcodes.Temporary, exitcodes.Restart} {
		if strings.Contains(want, fmt.Sprintf(" %d", code)) {
			t.Errorf("RestartPreventExitStatus prevents %d, which means start me again", code)
		}
	}
}

// launchd cannot be told which exit codes to give up on. What it can be told
// is not to restart a clean exit, and not to restart faster than every 30s.
func TestLaunchdThrottlesAndStopsOnCleanExit(t *testing.T) {
	got, err := RenderLaunchd(installed())
	if err != nil {
		t.Fatalf("RenderLaunchd: %v", err)
	}

	if strings.Contains(got, "<key>KeepAlive</key>\n\t<true/>") {
		t.Errorf("RenderLaunchd() still has an unconditional KeepAlive:\n%s", got)
	}
	if want := "<key>KeepAlive</key>\n\t<dict>\n\t\t<key>SuccessfulExit</key>\n\t\t<false/>\n\t</dict>"; !strings.Contains(got, want) {
		t.Errorf("RenderLaunchd() has no SuccessfulExit=false KeepAlive dict:\n%s", got)
	}
	if want := "<key>ThrottleInterval</key>\n\t<integer>30</integer>"; !strings.Contains(got, want) {
		t.Errorf("RenderLaunchd() has no 30s ThrottleInterval:\n%s", got)
	}
}

// systemd splits ExecStart and its space-separated path lists on whitespace,
// so an unquoted directory with a space in it becomes two arguments: `serve`
// exits Usage, which the unit prevents restarting, and the service stays down
// for a reason nothing in it explains.
func TestSystemdQuotesADirectoryWithASpaceInIt(t *testing.T) {
	u := installed()
	u.StateDir = "/var/lib/gentility node"

	got, err := RenderSystemd(u)
	if err != nil {
		t.Fatalf("RenderSystemd: %v", err)
	}

	want := "\nExecStart=/usr/local/bin/connector serve --state-dir \"/var/lib/gentility node\"\n"
	if !strings.Contains(got, want) {
		t.Errorf("RenderSystemd() has no %q:\n%s", strings.TrimSpace(want), got)
	}
	want = "\nReadWritePaths=\"/var/lib/gentility node\" /var/log/gentility\n"
	if !strings.Contains(got, want) {
		t.Errorf("RenderSystemd() has no %q:\n%s", strings.TrimSpace(want), got)
	}
}

// A binary path with a space in it is the first word of ExecStart, and an
// unquoted one names a program that does not exist.
func TestSystemdQuotesABinaryPathWithASpaceInIt(t *testing.T) {
	u := installed()
	u.BinaryPath = "/opt/gentility tools/connector"

	got, err := RenderSystemd(u)
	if err != nil {
		t.Fatalf("RenderSystemd: %v", err)
	}
	if want := "\nExecStart=\"/opt/gentility tools/connector\" serve "; !strings.Contains(got, want) {
		t.Errorf("RenderSystemd() has no %q:\n%s", strings.TrimSpace(want), got)
	}
}

// The quoting is systemd's C-style one, and it only speaks when spoken to:
// every path a real installation uses comes through untouched, which is what
// keeps the golden units byte-identical.
func TestQuoteArgSpeaksOnlyWhenItMustAndEscapesWhenItDoes(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"/var/lib/gentility", "/var/lib/gentility"},
		{"/usr/local/bin/connector", "/usr/local/bin/connector"},
		{"", ""},
		{"/var/lib/gentility node", `"/var/lib/gentility node"`},
		{"/var/lib/a\tb", "\"/var/lib/a\tb\""},
		{`/var/lib/say "hi"`, `"/var/lib/say \"hi\""`},
		{`/var/lib/back\slash`, `"/var/lib/back\\slash"`},
		{`/var/lib/it's`, `"/var/lib/it's"`},
	} {
		if got := quoteArg(c.in); got != c.want {
			t.Errorf("quoteArg(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// launchd takes its arguments as a list of separate strings, so a space in a
// path needs no quoting there — only the XML escaping it already does. Asserted
// so that the platforms' different answers to the same problem stay honest.
func TestLaunchdCarriesASpacedPathAsOneArgument(t *testing.T) {
	u := installed()
	u.StateDir = "/var/lib/gentility node"

	got, err := RenderLaunchd(u)
	if err != nil {
		t.Fatalf("RenderLaunchd: %v", err)
	}
	if want := "<string>/var/lib/gentility node</string>"; !strings.Contains(got, want) {
		t.Errorf("RenderLaunchd() has no %q:\n%s", want, got)
	}
}

// A supervisor unit pointing at a relative path is a unit that starts the
// wrong binary after a `cd` — or nothing at all.
func TestRenderRefusesARelativeBinaryPath(t *testing.T) {
	u := installed()
	u.BinaryPath = "connector"

	if _, err := RenderLaunchd(u); err == nil {
		t.Fatal("RenderLaunchd(relative binary) = nil error, want a refusal")
	} else if !strings.Contains(err.Error(), "BinaryPath") {
		t.Fatalf("RenderLaunchd error = %v, want it to name BinaryPath", err)
	}

	if _, err := RenderSystemd(u); err == nil {
		t.Fatal("RenderSystemd(relative binary) = nil error, want a refusal")
	} else if !strings.Contains(err.Error(), "BinaryPath") {
		t.Fatalf("RenderSystemd error = %v, want it to name BinaryPath", err)
	}
}

func TestRenderRefusesAnEmptyField(t *testing.T) {
	for _, tc := range []struct {
		name  string
		unit  func(Unit) Unit
		wants string
	}{
		{"state dir", func(u Unit) Unit { u.StateDir = ""; return u }, "StateDir"},
		{"label", func(u Unit) Unit { u.Label = ""; return u }, "Label"},
		{"log dir", func(u Unit) Unit { u.LogDir = ""; return u }, "LogDir"},
		{"binary path", func(u Unit) Unit { u.BinaryPath = ""; return u }, "BinaryPath"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			u := tc.unit(installed())

			if _, err := RenderLaunchd(u); err == nil {
				t.Fatal("RenderLaunchd = nil error, want a refusal")
			} else if !strings.Contains(err.Error(), tc.wants) {
				t.Fatalf("RenderLaunchd error = %v, want it to name %s", err, tc.wants)
			}

			if _, err := RenderSystemd(u); err == nil {
				t.Fatal("RenderSystemd = nil error, want a refusal")
			} else if !strings.Contains(err.Error(), tc.wants) {
				t.Fatalf("RenderSystemd error = %v, want it to name %s", err, tc.wants)
			}
		})
	}
}
