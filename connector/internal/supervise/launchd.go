package supervise

import (
	"encoding/xml"
	"strings"
)

// RenderLaunchd renders the launchd daemon plist for u.
//
// KeepAlive is SuccessfulExit=false: launchd restarts the job on any non-zero
// exit — which keeps the binary-swap restart working without the node asking
// anyone's permission — and leaves a clean exit alone.
//
// This is weaker than the systemd unit, and the gap is a documented platform
// limitation, not an oversight. launchd's KeepAlive has no exit-code
// allowlist: SuccessfulExit and Crashed are the whole vocabulary, so there is
// no way to say "and never come back from a 3". A refused node on darwin is
// therefore restarted anyway; ThrottleInterval is the only brake available,
// and it holds the loop to one attempt every 30 seconds rather than launchd's
// default 10. Linux is the real server platform — the systemd unit stops a
// refusal outright — and darwin is for a developer's machine, where the loop
// is visible and short-lived.
//
// RunAtLoad brings it up at boot. ProcessType Interactive runs the connector
// in the class apps run in, because a person is waiting on every packet it
// carries. Background ran it at priority 4, the throttled class: under a
// loaded Mac it went minutes without a core, and ipnlocal's watchdog
// panicked on locks held by goroutines that were runnable but never ran
// (T-450). Unset would mean Standard, priority 20, still below the agents and
// builds it competes with.
func RenderLaunchd(u Unit) (string, error) {
	if err := u.validate(); err != nil {
		return "", err
	}

	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	b.WriteString(`<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">` + "\n")
	b.WriteString(`<plist version="1.0">` + "\n")
	b.WriteString("<dict>\n")
	b.WriteString("\t<key>Label</key>\n")
	b.WriteString("\t<string>" + escapeXML(u.Label) + "</string>\n")
	b.WriteString("\t<key>ProgramArguments</key>\n")
	b.WriteString("\t<array>\n")
	for _, arg := range u.args() {
		b.WriteString("\t\t<string>" + escapeXML(arg) + "</string>\n")
	}
	b.WriteString("\t</array>\n")
	b.WriteString("\t<key>RunAtLoad</key>\n")
	b.WriteString("\t<true/>\n")
	b.WriteString("\t<key>KeepAlive</key>\n")
	b.WriteString("\t<dict>\n")
	b.WriteString("\t\t<key>SuccessfulExit</key>\n")
	b.WriteString("\t\t<false/>\n")
	b.WriteString("\t</dict>\n")
	b.WriteString("\t<key>ThrottleInterval</key>\n")
	b.WriteString("\t<integer>30</integer>\n")
	b.WriteString("\t<key>ProcessType</key>\n")
	b.WriteString("\t<string>Interactive</string>\n")
	b.WriteString("\t<key>StandardOutPath</key>\n")
	b.WriteString("\t<string>" + escapeXML(u.stdoutPath()) + "</string>\n")
	b.WriteString("\t<key>StandardErrorPath</key>\n")
	b.WriteString("\t<string>" + escapeXML(u.stderrPath()) + "</string>\n")
	b.WriteString("</dict>\n")
	b.WriteString("</plist>\n")
	return b.String(), nil
}

// escapeXML makes a path safe to sit between plist tags. A path with an
// ampersand in it is legal on both platforms and would otherwise render a
// plist launchd refuses to parse.
func escapeXML(s string) string {
	var b strings.Builder
	// xml.EscapeText only fails when the writer does, and strings.Builder
	// never does.
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}
