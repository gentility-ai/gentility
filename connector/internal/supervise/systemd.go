package supervise

import (
	"strconv"
	"strings"

	"github.com/gentility-ai/gentility/connector/internal/exitcodes"
)

// refusals are the exit codes systemd must not restart into: the connector
// said no, and it will say no again two seconds later. They are read from the
// exitcodes package rather than written out here so the unit and the exit
// vocabulary cannot drift apart. Temporary and Restart are deliberately
// absent — both mean "start me again".
var refusals = []int{
	exitcodes.Usage,
	exitcodes.EnrollRefused,
	exitcodes.MaterialExpired,
	exitcodes.CapverTooOld,
}

// RenderSystemd renders the systemd service unit for u.
//
// network-online.target rather than network.target: the connector's first act
// is to reach the mothership, and network.target only promises the stack is
// configured, not routable.
//
// Restart=on-failure with RestartPreventExitStatus is the whole of the restart
// policy: a crash or a Temporary exit comes back after two seconds, the
// binary-swap Restart exit comes back as the new binary, a clean exit stays
// stopped — and a refusal (a revoked node, a stale enrollment, a binary too
// old) stays stopped too, rather than knocking on the mothership's door every
// two seconds forever.
//
// The hardening lines are the ordinary daemon minimum: no privilege
// escalation, a private /tmp, and a read-only system with exactly two
// directories carved back out — the state directory it must write and the log
// directory it writes into.
//
// An interface unit keeps the first two and loosens the rest, because the
// interface is root's work and so is the machine's DNS. It names no User=,
// since only root creates a network interface. It is ProtectSystem=true rather
// than strict or full: /usr and /boot stay read-only, but /etc stays writable
// (full would make /etc read-only too, by systemd.exec(5)), because on a host
// without systemd-resolved the overlay's DNS is a write to /etc/resolv.conf.
// It carves out no ReadWritePaths, because true leaves /var, and the state
// and log directories with it, writable. And it orders itself after
// NetworkManager and systemd-resolved, as tailscaled.service does, so the DNS
// manager it picks at start is talking to a resolver that is already up.
//
// It also runs `cleanup --interface` after every stop (ExecStopPost runs after
// a crash and a kill as well as a clean exit), so a daemon that died without
// closing does not leave the machine's DNS pointing at a resolver that is
// gone until its next start.
func RenderSystemd(u Unit) (string, error) {
	if err := u.validate(); err != nil {
		return "", err
	}

	var b strings.Builder
	b.WriteString("[Unit]\n")
	b.WriteString("Description=Gentility connector (" + u.Label + ")\n")
	if u.Interface {
		b.WriteString("After=network-online.target NetworkManager.service systemd-resolved.service\n")
	} else {
		b.WriteString("After=network-online.target\n")
	}
	b.WriteString("Wants=network-online.target\n")
	b.WriteString("\n")
	b.WriteString("[Service]\n")
	b.WriteString("Type=simple\n")
	b.WriteString("ExecStart=" + strings.Join(quoteArgs(u.args()), " ") + "\n")
	if u.Interface {
		b.WriteString("ExecStopPost=" + strings.Join(quoteArgs([]string{u.BinaryPath, "cleanup", "--interface"}), " ") + "\n")
	}
	b.WriteString("Restart=on-failure\n")
	b.WriteString("RestartPreventExitStatus=" + strings.Join(codes(refusals), " ") + "\n")
	b.WriteString("RestartSec=2\n")
	if !u.Interface {
		b.WriteString("User=" + serviceUser + "\n")
	}
	b.WriteString("NoNewPrivileges=true\n")
	b.WriteString("PrivateTmp=true\n")
	if u.Interface {
		b.WriteString("ProtectSystem=true\n")
	} else {
		b.WriteString("ProtectSystem=strict\n")
		b.WriteString("ReadWritePaths=" + quoteArg(u.StateDir) + " " + quoteArg(u.LogDir) + "\n")
	}
	b.WriteString("\n")
	b.WriteString("[Install]\n")
	b.WriteString("WantedBy=multi-user.target\n")
	return b.String(), nil
}

// quoteArg renders s as one word of a systemd directive.
//
// systemd splits ExecStart and its space-separated path lists on whitespace,
// so a state directory named "/var/lib/gentility node" would otherwise reach
// `serve` as two arguments — Usage, which the unit above prevents restarting,
// so the service stays down and nothing in it says why. A binary path with a
// space in it would name a program that does not exist.
//
// The answer is systemd's own C-style quoting: double quotes around the word,
// with backslashes and double quotes inside it escaped. It is applied only when
// the word actually needs it, so every path a real installation uses — and
// every golden unit — renders exactly as it did before.
func quoteArg(s string) string {
	if !strings.ContainsAny(s, " \t\n\r\v\f\"'\\") {
		return s
	}
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}

// quoteArgs is quoteArg over a command's words.
func quoteArgs(args []string) []string {
	out := make([]string, len(args))
	for i, a := range args {
		out[i] = quoteArg(a)
	}
	return out
}

// codes renders exit codes as the words systemd's directive expects.
func codes(cs []int) []string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = strconv.Itoa(c)
	}
	return out
}
