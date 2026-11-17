package egress

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"time"
)

// Preflight is the loopback CONNECT proxy a case node points HTTPS_PROXY at.
//
// It is UX and NOTHING ELSE: the node runs inside the sandbox the
// agent controls, so nothing it decides is a boundary — the value of this list
// is that a coding agent asking for a host its org never allowed gets a clear
// 403 in a second instead of a confusing failure a minute later. The relay
// makes the real decision, and a process inside the sandbox that skips this
// proxy and speaks CONNECT to the relay directly gets exactly the same answer
// and the same recorded fact.
//
// An empty Allow forwards everything: "we were told no hint" is not "deny".
type Preflight struct {
	Allow     []string
	DialRelay func(ctx context.Context) (net.Conn, error)
	// DialTimeout bounds reaching the relay. Kept as a field so the tests can
	// shorten it; production leaves it at the 2s below. A tsnet dial to a peer
	// that has left the overlay stalls rather than refusing, and an unbounded
	// dial makes the client wait out its own ceiling for a refusal that was
	// already certain.
	DialTimeout time.Duration
}

func (p *Preflight) Serve(ln net.Listener) error {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return err
		}
		go p.handle(conn)
	}
}

func (p *Preflight) handle(client net.Conn) {
	defer client.Close()

	br := bufio.NewReader(client)
	req, err := http.ReadRequest(br)
	if err != nil {
		return
	}
	if req.Method != http.MethodConnect {
		writeStatus(client, http.StatusMethodNotAllowed, "only CONNECT")
		return
	}

	host, _, err := net.SplitHostPort(req.Host)
	if err != nil {
		writeStatus(client, http.StatusBadRequest, "bad target")
		return
	}

	// An IP-literal target carries no hostname to match against the hint list,
	// and the preflight holds no literal allowlist — literals live in the relay's
	// LiteralAllow, which is the only place with the basis to admit or refuse
	// one. So a literal is forwarded UNCONDITIONALLY: turning an org-allowlisted
	// literal into a confusing fast 403 here, while the relay would have allowed
	// it, is exactly the sandbox-refuses-what-the-relay-allows drift this proxy
	// must never introduce. permits gates hostnames only.
	_, literalErr := netip.ParseAddr(host)
	isLiteral := literalErr == nil
	if !isLiteral && len(p.Allow) > 0 && !permits(p.Allow, host) {
		writeStatus(client, http.StatusForbidden,
			"host is not in this organization's egress allowlist (preflight)")
		return
	}

	// The budget is the relay's own admit budget (admit.go), deliberately the
	// same number: a relay that cannot be reached within the time it would
	// itself have given the mothership is a relay that is down, and saying so
	// now is better than the client waiting out its own ceiling for it. Cancel
	// as soon as the dial answers — the budget covers reaching the relay, not
	// the connection it hands back.
	timeout := p.DialTimeout
	if timeout == 0 {
		timeout = 2 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	upstream, err := p.DialRelay(ctx)
	cancel()
	if err != nil {
		writeStatus(client, http.StatusBadGateway, "relay unreachable")
		return
	}
	defer upstream.Close()

	// Forwarded verbatim: the relay re-parses the request line itself, and
	// rewriting it here would put a second opinion in the path.
	if err := req.Write(upstream); err != nil {
		return
	}

	go func() { _, _ = io.Copy(upstream, br) }()
	_, _ = io.Copy(client, upstream)
}

// The name matched is the name dialled, so the preflight's fast refusal must be
// the SAME rule the relay's read-path allowlist applies — anything laxer here
// refuses in the sandbox what the relay would allow, or (worse) waves through a
// spelling the relay reads as a different, unallowed host.
const (
	maxName  = 253
	maxLabel = 63
)

// permits mirrors the server's allowlist matching EXACTLY: an
// exact host, or `*.` plus one extra label, but only once both the host and the
// pattern's non-wildcard portion pass a strict ASCII LDH gate, with a single
// trailing root dot stripped and ASCII-only case folding. Two implementations
// of one rule is a smell; they are pinned in step by the shared test vectors in
// contract/connector/allowlist_cases.json, which the server test
// reads too.
//
// Every liberty is the narrowest the server takes, and for the reasons it
// takes them: ASCII-only case folding because Unicode folding is not spelling
// (KELVIN SIGN U+212A folds to `k`, and strings.ToLower would admit
// "Kubernetes.io" against "kubernetes.io" while the resolver dialled the
// raw bytes), and at most one trailing dot because the second is an empty label.
func permits(patterns []string, host string) bool {
	host = normaliseHost(host)
	if !isHostname(host) {
		return false
	}
	for _, pattern := range patterns {
		if matches(normaliseHost(pattern), host) {
			return true
		}
	}
	return false
}

// matches assumes host has already passed isHostname. A wildcard admits exactly
// one extra label (Cut splits at the FIRST dot, so there is no depth to count),
// and its suffix is gated in its own right — an exact pattern can only equal a
// host that already passed the gate, but a wildcard's suffix is never compared
// against a validated whole, so `*.bad_suffix.com` must refuse itself.
func matches(pattern, host string) bool {
	if suffix, ok := strings.CutPrefix(pattern, "*."); ok {
		_, rest, found := strings.Cut(host, ".")
		return found && rest == suffix && isHostname(suffix)
	}
	return pattern == host
}

// normaliseHost strips at most ONE trailing dot (the root dot a resolver-shaped
// name legitimately carries) and case-folds ASCII byte-wise. Byte-wise, not
// rune-wise, so invalid UTF-8 and every non-ASCII byte is left untouched to be
// refused by the LDH gate rather than folded into something that matches.
func normaliseHost(s string) string {
	s = strings.TrimSuffix(s, ".")
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + 32
		}
	}
	return string(b)
}

// isHostname is the strict ASCII LDH gate: at most 253 bytes, labels of 1..63
// bytes, each label [a-z0-9-] with no leading or trailing hyphen. Byte length,
// not rune count — the resolver counts bytes.
func isHostname(name string) bool {
	if len(name) < 1 || len(name) > maxName {
		return false
	}
	for _, label := range strings.Split(name, ".") {
		if !isLabel(label) {
			return false
		}
	}
	return true
}

func isLabel(label string) bool {
	if len(label) < 1 || len(label) > maxLabel {
		return false
	}
	if label[0] == '-' || label[len(label)-1] == '-' {
		return false
	}
	for i := 0; i < len(label); i++ {
		c := label[i]
		if !((c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-') {
			return false
		}
	}
	return true
}
