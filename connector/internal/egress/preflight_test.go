package egress

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/gentility-ai/gentility/connector/internal/contracttest"
)

func TestPreflightRefusesOutsideItsHintList(t *testing.T) {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	defer ln.Close()

	p := &Preflight{
		Allow: []string{"*.github.com"},
		DialRelay: func(context.Context) (net.Conn, error) {
			t.Fatal("a preflight refusal must not open an overlay connection")
			return nil, nil
		},
	}
	go p.Serve(ln)

	c, _ := net.Dial("tcp", ln.Addr().String())
	defer c.Close()
	_, _ = io.WriteString(c, "CONNECT example.com:443 HTTP/1.1\r\nHost: example.com:443\r\n\r\n")

	status, _ := bufio.NewReader(c).ReadString('\n')
	if !strings.Contains(status, "403") {
		t.Fatalf("status = %q, want 403", status)
	}
}

func TestPreflightForwardsAnAllowedTargetVerbatim(t *testing.T) {
	relay, _ := net.Listen("tcp", "127.0.0.1:0")
	defer relay.Close()

	seen := make(chan string, 1)
	go func() {
		c, err := relay.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		line, _ := bufio.NewReader(c).ReadString('\n')
		seen <- line
		_, _ = io.WriteString(c, "HTTP/1.1 200 Connection Established\r\n\r\n")
	}()

	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	defer ln.Close()

	p := &Preflight{
		Allow: []string{"*.github.com"},
		DialRelay: func(ctx context.Context) (net.Conn, error) {
			return net.Dial("tcp", relay.Addr().String())
		},
	}
	go p.Serve(ln)

	c, _ := net.Dial("tcp", ln.Addr().String())
	defer c.Close()
	_, _ = io.WriteString(c, "CONNECT api.github.com:443 HTTP/1.1\r\nHost: api.github.com:443\r\n\r\n")

	if got := <-seen; !strings.HasPrefix(got, "CONNECT api.github.com:443") {
		t.Fatalf("relay saw %q", got)
	}
}

func TestPreflightWithNoAllowListForwardsEverything(t *testing.T) {
	// An empty preflight list is "no hint", not "deny all": the relay is the
	// authority and a node that has not been told the hint must not invent one.
	relay, _ := net.Listen("tcp", "127.0.0.1:0")
	defer relay.Close()

	reached := make(chan struct{}, 1)
	go func() {
		c, err := relay.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		_, _ = bufio.NewReader(c).ReadString('\n')
		reached <- struct{}{}
		_, _ = io.WriteString(c, "HTTP/1.1 200 Connection Established\r\n\r\n")
	}()

	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	defer ln.Close()

	p := &Preflight{
		DialRelay: func(context.Context) (net.Conn, error) { return net.Dial("tcp", relay.Addr().String()) },
	}
	go p.Serve(ln)

	c, _ := net.Dial("tcp", ln.Addr().String())
	defer c.Close()
	_, _ = io.WriteString(c, "CONNECT anything.example:443 HTTP/1.1\r\nHost: anything.example:443\r\n\r\n")
	<-reached
}

// An IP-literal CONNECT target is forwarded to the relay even when a non-empty
// hostname hint list would refuse every host that is not `*.github.com`. The
// preflight holds no literal allowlist — literals live in the relay's
// LiteralAllow — so it has no basis to refuse one, and turning an
// org-allowlisted literal into a fast 403 here would refuse in the sandbox what
// the relay allows. The DialRelay MUST be reached; a 403 would be the bug.
func TestPreflightForwardsAnIPLiteralTarget(t *testing.T) {
	relay, _ := net.Listen("tcp", "127.0.0.1:0")
	defer relay.Close()

	seen := make(chan string, 1)
	go func() {
		c, err := relay.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		line, _ := bufio.NewReader(c).ReadString('\n')
		seen <- line
		_, _ = io.WriteString(c, "HTTP/1.1 200 Connection Established\r\n\r\n")
	}()

	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	defer ln.Close()

	p := &Preflight{
		// A hostname hint list that admits nothing but *.github.com. A literal
		// must skip it entirely rather than be measured against it.
		Allow:     []string{"*.github.com"},
		DialRelay: func(context.Context) (net.Conn, error) { return net.Dial("tcp", relay.Addr().String()) },
	}
	go p.Serve(ln)

	c, _ := net.Dial("tcp", ln.Addr().String())
	defer c.Close()
	_, _ = io.WriteString(c, "CONNECT 93.184.216.34:443 HTTP/1.1\r\nHost: 93.184.216.34:443\r\n\r\n")

	if got := <-seen; !strings.HasPrefix(got, "CONNECT 93.184.216.34:443") {
		t.Fatalf("relay saw %q, want the literal forwarded verbatim (not a 403)", got)
	}
}

// A relay that has left the overlay does not refuse the dial: a tsnet dial to a
// departed peer stalls. Fail-closed still holds — nothing is relayed and no
// admit fact is recorded — but a client that is never answered waits out its
// own ceiling for a refusal that was certain within the dial budget. The
// refusal it gets is the same one a relay it cannot reach already produced.
func TestPreflightRefusesWhenTheRelayDialStalls(t *testing.T) {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	defer ln.Close()

	p := &Preflight{
		DialTimeout: 50 * time.Millisecond,
		DialRelay: func(ctx context.Context) (net.Conn, error) {
			// Stalls the way the departed peer does, but with a ceiling of its
			// own so a preflight that ignores the budget fails this test rather
			// than hanging it.
			select {
			case <-ctx.Done():
			case <-time.After(2 * time.Second):
			}
			return nil, errors.New("the relay is not there")
		},
	}
	go p.Serve(ln)

	c, _ := net.Dial("tcp", ln.Addr().String())
	defer c.Close()
	_, _ = io.WriteString(c, "CONNECT anything.example:443 HTTP/1.1\r\nHost: anything.example:443\r\n\r\n")

	asked := time.Now()
	status, err := bufio.NewReader(c).ReadString('\n')
	waited := time.Since(asked)
	if err != nil {
		t.Fatalf("reading the refusal: %v", err)
	}
	if waited > time.Second {
		t.Fatalf("the client waited %s for its answer; the dial budget was 50ms", waited)
	}
	if !strings.Contains(status, "502") {
		t.Fatalf("status = %q, want the 502 an unreachable relay already answers", status)
	}
}

// sharedCases pins the client and server matchers to the same fixtures. Its
// literal rows carry a per-language classification because the two deliberately
// diverge on the odd `inet` spellings: the server over-includes, Go's netip
// discards what it cannot parse, and that fail-safe direction is pinned here.
type sharedCases struct {
	Permits []struct {
		Note      string   `json:"note"`
		Patterns  []string `json:"patterns"`
		Host      string   `json:"host"`
		Permitted bool     `json:"permitted"`
	} `json:"permits"`
	Literal []struct {
		Note    string `json:"note"`
		Pattern string `json:"pattern"`
		Elixir  bool   `json:"elixir"`
		Go      bool   `json:"go"`
	} `json:"literal"`
}

func loadSharedCases(t *testing.T) sharedCases {
	t.Helper()
	raw := contracttest.Read(t, "connector/allowlist_cases.json")
	var cases sharedCases
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatalf("decoding shared cases: %v", err)
	}
	return cases
}

func TestPermitsAgreesWithSharedVectors(t *testing.T) {
	for _, tc := range loadSharedCases(t).Permits {
		if got := permits(tc.Patterns, tc.Host); got != tc.Permitted {
			t.Errorf("permits(%q, %q) = %v, want %v (%s)", tc.Patterns, tc.Host, got, tc.Permitted, tc.Note)
		}
	}
}

// TestLiteralClassificationAgreesWithSharedVectors pins the Go half of the
// literal split: netip.ParseAddr is what CheckTarget uses to tell an IP-literal
// CONNECT target from a hostname, so the `go` column is exactly its verdict.
func TestLiteralClassificationAgreesWithSharedVectors(t *testing.T) {
	for _, tc := range loadSharedCases(t).Literal {
		_, err := netip.ParseAddr(tc.Pattern)
		if got := err == nil; got != tc.Go {
			t.Errorf("netip.ParseAddr(%q) parses = %v, want %v (%s)", tc.Pattern, got, tc.Go, tc.Note)
		}
	}
}
