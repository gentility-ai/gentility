package config

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gentility-ai/gentility/connector/internal/exitcodes"
	"github.com/gentility-ai/gentility/connector/internal/state"
	"github.com/gentility-ai/gentility/connector/internal/version"
)

const credential = "cred-shhh"

func store(t *testing.T) *state.Store {
	t.Helper()
	s := &state.Store{Dir: filepath.Join(t.TempDir(), "gentility")}
	if err := s.SaveNode(state.Node{
		NodeID:         "0198f0a2-0000-7000-8000-000000000001",
		NodeCredential: credential,
		LastRevision:   state.NoRevision,
	}); err != nil {
		t.Fatal(err)
	}
	return s
}

func client(t *testing.T, s *state.Store, handler http.HandlerFunc) *Client {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	node, err := s.LoadNode()
	if err != nil {
		t.Fatal(err)
	}
	return New(server.URL, node, s, server.Client())
}

// document is the configuration response shape, as the server's renderer
// writes it.
func document(revision int64) map[string]any {
	return map[string]any{
		"renderer_version": 1,
		"revision":         revision,
		"capver_floor":     1,
		"node_id":          "0198f0a2-0000-7000-8000-000000000001",
		"grants": []map[string]any{
			{"org": "acme", "address": "fd06:6e7:0:1::a", "label": "n1"},
		},
		"services": []map[string]any{
			{"id": "svc", "kind": "egress", "org": "acme", "port": 8443, "state": "approved"},
		},
		"egress": map[string]any{
			"org": "acme", "via": "fd06:6e7:0:1::b", "port": 8443,
			"preflight_allow": []string{"*.github.com"},
		},
		"gateway": map[string]any{
			"address": "fd06:6e7:0:1::1", "port": 443, "ca_pem": nil,
		},
		"update": map[string]any{
			"latest_version": "0.1.0", "min_version": "0.1.0", "manifest_url": nil,
		},
	}
}

func writeJSON(t *testing.T, w http.ResponseWriter, status int, body any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		t.Errorf("encoding the reply: %v", err)
	}
}

func TestPullSendsTheBearerTheCapverAndNoSinceBeforeTheFirstDocument(t *testing.T) {
	var got *http.Request
	s := store(t)
	c := client(t, s, func(w http.ResponseWriter, r *http.Request) {
		got = r.Clone(context.Background())
		writeJSON(t, w, 200, document(0))
	})

	if _, err := c.Pull(context.Background()); err != nil {
		t.Fatalf("Pull: %v", err)
	}

	if got.URL.Path != "/overlay/self/config" {
		t.Fatalf("path = %q, want /overlay/self/config", got.URL.Path)
	}
	if want := "Bearer " + credential; got.Header.Get("Authorization") != want {
		t.Fatalf("Authorization = %q, want %q", got.Header.Get("Authorization"), want)
	}
	if got.Header.Get("X-Node-Capver") != "1" {
		t.Fatalf("X-Node-Capver = %q, want 1", got.Header.Get("X-Node-Capver"))
	}
	// A node with no document has no cursor, and an absent `since` is what
	// asks for the whole document rather than a claim to hold revision 0.
	if _, present := got.URL.Query()["since"]; present {
		t.Fatalf("since = %q, want it absent before the first document", got.URL.RawQuery)
	}
}

// The cursor is a document this PROCESS applied, so it is the apply that puts
// `since` on the wire and nothing else can.
func TestPullSendsSinceOnceItHoldsADocument(t *testing.T) {
	var query url.Values
	s := store(t)
	var served atomic.Int64
	c := client(t, s, func(w http.ResponseWriter, r *http.Request) {
		if served.Add(1) == 1 {
			writeJSON(t, w, 200, document(5))
			return
		}
		query = r.URL.Query()
		w.WriteHeader(304)
	})

	if _, err := c.Pull(context.Background()); err != nil {
		t.Fatalf("Pull: %v", err)
	}
	c.applied(5)

	if _, err := c.Pull(context.Background()); !errors.Is(err, ErrUnchanged) {
		t.Fatalf("Pull = %v, want ErrUnchanged", err)
	}
	if query.Get("since") != "5" {
		t.Fatalf("since = %q, want 5", query.Get("since"))
	}
}

func TestPull304ReturnsUnchangedAndLeavesTheRevisionWhereItWas(t *testing.T) {
	s := store(t)
	c := client(t, s, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(304) })
	c.applied(5)

	if _, err := c.Pull(context.Background()); !errors.Is(err, ErrUnchanged) {
		t.Fatalf("Pull = %v, want ErrUnchanged", err)
	}
	if c.Revision() != 5 {
		t.Fatalf("Revision() = %d, want 5", c.Revision())
	}
}

// A config document is applied into memory — grants, listeners, the relay — so
// a restarted process holds none of it. Asking for something NEWER than the
// cursor on disk would be answered 304 forever: no grants, never Ready, and a
// heartbeat loop waiting on a readiness that could not arrive.
func TestARestartedNodeAsksForTheWholeDocumentAndAppliesItAgain(t *testing.T) {
	s := store(t)
	if err := s.AdvanceRevision(5); err != nil {
		t.Fatal(err)
	}

	// The FIRST request's query, and only that one: the loop pulls again as
	// soon as it has applied, and by then the cursor is legitimately 5.
	var mu sync.Mutex
	var query url.Values
	var served int

	applied := make(chan *Document, 1)
	c := client(t, s, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		served++
		if served == 1 {
			query = r.URL.Query()
		}
		mu.Unlock()

		writeJSON(t, w, 200, document(5))
	})
	c.MinPoll = time.Millisecond

	if c.Revision() != state.NoRevision {
		t.Fatalf("Revision() = %d on a fresh process, want NoRevision", c.Revision())
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Run(ctx, func(doc *Document) error {
		select {
		case applied <- doc:
		default:
		}
		return nil
	})

	select {
	case doc := <-applied:
		if doc.Revision != 5 {
			t.Fatalf("applied revision %d, want the 5 it already held", doc.Revision)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a restarted node never applied the document it was holding")
	}

	waitFor(t, c.Ready, "Ready() never became true after a restart")

	mu.Lock()
	first := query
	mu.Unlock()

	if first.Has("since") {
		t.Fatalf("a restarted node sent since=%q; it holds no document in memory", first.Get("since"))
	}
}

// The mothership's answer for a node the renderer has not reached yet
// (the overlay router). It is not a failure and it is not a 304: the
// node has no document at all, and the render is already queued.
func TestPull409IsNotRenderedAndIsRetryable(t *testing.T) {
	s := store(t)
	c := client(t, s, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, 409, map[string]string{"error": "not_rendered"})
	})

	_, err := c.Pull(context.Background())
	if !errors.Is(err, ErrNotRendered) {
		t.Fatalf("Pull = %v, want ErrNotRendered", err)
	}
	if _, fatal := exitcodes.Of(err); fatal {
		t.Fatalf("ErrNotRendered must be retryable")
	}
}

func TestPull200DecodesTheDocumentAndLeavesTheCursorForApply(t *testing.T) {
	s := store(t)
	c := client(t, s, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, 200, document(3))
	})

	doc, err := c.Pull(context.Background())
	if err != nil {
		t.Fatalf("Pull: %v", err)
	}
	if doc.Revision != 3 || doc.NodeID == "" || len(doc.Grants) != 1 {
		t.Fatalf("decoded %+v", doc)
	}
	if doc.Egress == nil || doc.Egress.Via != "fd06:6e7:0:1::b" {
		t.Fatalf("egress = %+v", doc.Egress)
	}
	if doc.Gateway.Address != "fd06:6e7:0:1::1" || doc.Gateway.Port != 443 {
		t.Fatalf("gateway = %+v", doc.Gateway)
	}
	// A pull that has not been applied has changed nothing: the cursor moves
	// when the node has actually taken the document on.
	if c.Revision() != state.NoRevision {
		t.Fatalf("Revision() = %d, want NoRevision until the document is applied", c.Revision())
	}
	if c.Ready() {
		t.Fatalf("Ready() is true before any document was applied")
	}
}

// A null egress is a refusal and not a default (Render.config_for/6): the
// organization has selected no relay, and the node must not invent one.
func TestPullReadsANullEgressAsNoEgress(t *testing.T) {
	s := store(t)
	body := document(0)
	body["egress"] = nil
	c := client(t, s, func(w http.ResponseWriter, _ *http.Request) { writeJSON(t, w, 200, body) })

	doc, err := c.Pull(context.Background())
	if err != nil {
		t.Fatalf("Pull: %v", err)
	}
	if doc.Egress != nil {
		t.Fatalf("Egress = %+v, want nil", doc.Egress)
	}
}

func TestPullRejectsUnknownRendererVersion(t *testing.T) {
	s := store(t)
	body := document(0)
	body["renderer_version"] = 2
	c := client(t, s, func(w http.ResponseWriter, _ *http.Request) { writeJSON(t, w, 200, body) })

	doc, err := c.Pull(context.Background())
	var unknown *UnknownRendererError
	if !errors.As(err, &unknown) {
		t.Fatalf("Pull = %v, want *UnknownRendererError", err)
	}
	if unknown.Version != 2 {
		t.Fatalf("Version = %d, want 2", unknown.Version)
	}
	if doc != nil {
		t.Fatalf("a document this binary cannot read was returned anyway: %+v", doc)
	}
	if code, fatal := exitcodes.Of(err); !fatal || code != exitcodes.CapverTooOld {
		t.Fatalf("exit code = (%d, %v), want (%d, true)", code, fatal, exitcodes.CapverTooOld)
	}
}

func TestPull426ReturnsCapverTooOldWithTheFloor(t *testing.T) {
	s := store(t)
	c := client(t, s, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, 426, map[string]any{"error": "capver_below_floor", "floor": 4})
	})

	_, err := c.Pull(context.Background())
	var old *CapverTooOldError
	if !errors.As(err, &old) {
		t.Fatalf("Pull = %v, want *CapverTooOldError", err)
	}
	if old.Floor != 4 || old.Declared != version.NodeCapver {
		t.Fatalf("CapverTooOldError = %+v", old)
	}
	if code, fatal := exitcodes.Of(err); !fatal || code != exitcodes.CapverTooOld {
		t.Fatalf("exit code = (%d, %v), want (%d, true)", code, fatal, exitcodes.CapverTooOld)
	}
}

func TestPull401IsFatalNotRetried(t *testing.T) {
	s := store(t)
	var calls atomic.Int64
	c := client(t, s, func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		writeJSON(t, w, 401, map[string]string{"error": "bad_credential"})
	})

	err := c.Run(context.Background(), func(*Document) error { return nil })
	code, fatal := exitcodes.Of(err)
	if !fatal || code != exitcodes.EnrollRefused {
		t.Fatalf("Run = %v (code %d, fatal %v), want a fatal EnrollRefused", err, code, fatal)
	}
	if calls.Load() != 1 {
		t.Fatalf("the loop pulled %d times; a refused credential is not retried", calls.Load())
	}
	if !strings.Contains(err.Error(), "bad_credential") {
		t.Fatalf("the refusal does not name what the mothership said: %v", err)
	}
}

// The other 401. `no_overlay_peer`, `ambiguous_peer` and every verifier
// refusal are the GATEWAY's — a header it stopped setting, a signature that
// went stale — and no amount of enrolling again fixes one. A node that treated
// them as its own would take itself off the network over somebody else's
// misconfiguration.
func TestPull401AboutThePeerIsRetryable(t *testing.T) {
	for _, reason := range []string{"no_overlay_peer", "ambiguous_peer", "peer_unverified", "a_verifier_s4_adds"} {
		t.Run(reason, func(t *testing.T) {
			s := store(t)
			c := client(t, s, func(w http.ResponseWriter, _ *http.Request) {
				writeJSON(t, w, 401, map[string]string{"error": reason})
			})

			_, err := c.Pull(context.Background())
			if err == nil {
				t.Fatal("want an error")
			}
			if _, fatal := exitcodes.Of(err); fatal {
				t.Fatalf("a 401 %s is the gateway's, and must be retryable: %v", reason, err)
			}
		})
	}
}

func TestPull401AboutTheCredentialIsFatal(t *testing.T) {
	for _, reason := range []string{"no_credential", "bad_credential", "node_revoked"} {
		t.Run(reason, func(t *testing.T) {
			s := store(t)
			c := client(t, s, func(w http.ResponseWriter, _ *http.Request) {
				writeJSON(t, w, 401, map[string]string{"error": reason})
			})

			_, err := c.Pull(context.Background())
			if code, fatal := exitcodes.Of(err); !fatal || code != exitcodes.EnrollRefused {
				t.Fatalf("exit code = (%d, %v), want a fatal EnrollRefused; err = %v", code, fatal, err)
			}
		})
	}
}

// Every request this client sends carries the capver header, so a 400 is
// something between it and the listener rewriting one — which retrying sends
// again, unchanged.
func TestPull400IsFatal(t *testing.T) {
	for _, reason := range []string{"capver_required", "capver_malformed"} {
		t.Run(reason, func(t *testing.T) {
			s := store(t)
			c := client(t, s, func(w http.ResponseWriter, _ *http.Request) {
				writeJSON(t, w, 400, map[string]string{"error": reason})
			})

			_, err := c.Pull(context.Background())
			if code, fatal := exitcodes.Of(err); !fatal || code != exitcodes.Failure {
				t.Fatalf("exit code = (%d, %v), want a fatal Failure; err = %v", code, fatal, err)
			}
		})
	}
}

// A mothership that answers 200 to a `since` naming its own current revision
// is not one this loop should spin against.
func TestRunDoesNotReapplyTheDocumentItAlreadyHolds(t *testing.T) {
	s := store(t)
	var pulls atomic.Int64
	var applies atomic.Int64

	c := client(t, s, func(w http.ResponseWriter, _ *http.Request) {
		pulls.Add(1)
		writeJSON(t, w, 200, document(3))
	})
	c.MinPoll = time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_ = c.Run(ctx, func(*Document) error {
		applies.Add(1)
		return nil
	})

	if pulls.Load() < 3 {
		t.Fatalf("the loop pulled %d times; it is meant to keep polling", pulls.Load())
	}
	if applies.Load() != 1 {
		t.Fatalf("applied %d times for one revision, want 1", applies.Load())
	}
}

func TestPull403IsFatal(t *testing.T) {
	s := store(t)
	c := client(t, s, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, 403, map[string]string{"error": "unknown_peer"})
	})

	_, err := c.Pull(context.Background())
	if code, fatal := exitcodes.Of(err); !fatal || code != exitcodes.EnrollRefused {
		t.Fatalf("exit code = (%d, %v), want a fatal EnrollRefused; err = %v", code, fatal, err)
	}
}

func TestPull500IsRetryable(t *testing.T) {
	s := store(t)
	c := client(t, s, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, 500, map[string]string{"error": "internal_error"})
	})

	_, err := c.Pull(context.Background())
	if err == nil {
		t.Fatal("want an error")
	}
	if _, fatal := exitcodes.Of(err); fatal {
		t.Fatalf("a 500 must be retryable, got fatal: %v", err)
	}
}

func TestBackoffIsExponentialWithJitterAndCap(t *testing.T) {
	lowest := &backoff{jitter: func() float64 { return 0 }}
	highest := &backoff{jitter: func() float64 { return 1 }}

	// 1s, 2s, 4s … capped at 60s, and every value inside [d/2, d].
	for _, want := range []time.Duration{
		time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second,
		32 * time.Second, 60 * time.Second, 60 * time.Second, 60 * time.Second,
	} {
		low, high := lowest.next(), highest.next()
		if low != want/2 {
			t.Fatalf("floor = %v, want %v (half of %v)", low, want/2, want)
		}
		if high != want {
			t.Fatalf("ceiling = %v, want %v", high, want)
		}
	}
}

func TestBackoffResetsAfterASuccessfulPull(t *testing.T) {
	b := &backoff{jitter: func() float64 { return 1 }}
	b.next()
	b.next()
	if got := b.next(); got != 4*time.Second {
		t.Fatalf("third delay = %v, want 4s", got)
	}

	b.reset()
	if got := b.next(); got != time.Second {
		t.Fatalf("delay after reset = %v, want 1s", got)
	}
}

func TestServesNothingUntilTheFirstDocumentIsApplied(t *testing.T) {
	s := store(t)
	applied := make(chan *Document, 1)
	c := client(t, s, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, 200, document(0))
	})

	if c.Ready() {
		t.Fatal("Ready() is true before the loop has run")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Run(ctx, func(doc *Document) error {
		select {
		case applied <- doc:
		default:
		}
		return nil
	})

	select {
	case <-applied:
	case <-time.After(5 * time.Second):
		t.Fatal("the loop never applied a document")
	}

	waitFor(t, c.Ready, "Ready() never became true after a document was applied")
	if c.Revision() != 0 {
		t.Fatalf("Revision() = %d, want 0", c.Revision())
	}

	stored, err := s.LoadNode()
	if err != nil {
		t.Fatal(err)
	}
	if stored.LastRevision != 0 {
		t.Fatalf("stored LastRevision = %d, want 0 — an applied revision survives a restart", stored.LastRevision)
	}
}

// The cursor is what says "I already have this". Moving it past a document the
// node could not take on would mean never being offered that document again.
func TestRunDoesNotAdvanceTheCursorWhenApplyFails(t *testing.T) {
	s := store(t)
	var calls atomic.Int64
	c := client(t, s, func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		writeJSON(t, w, 200, document(2))
	})
	c.MinPoll = time.Millisecond
	// The retry band, shrunk: this test is proving the loop comes back, not
	// how long it waits before it does.
	c.retry = backoff{base: time.Millisecond}

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	_ = c.Run(ctx, func(*Document) error { return errors.New("the netstack refused the grants") })

	if calls.Load() < 2 {
		t.Fatalf("the loop pulled %d times; a failed apply must be retried", calls.Load())
	}
	if c.Revision() != state.NoRevision {
		t.Fatalf("Revision() = %d, want NoRevision", c.Revision())
	}
	if c.Ready() {
		t.Fatal("Ready() is true after every apply failed")
	}
}

func TestRunStopsOnContextCancel(t *testing.T) {
	s := store(t)
	c := client(t, s, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(304) })
	c.MinPoll = time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := c.Run(ctx, func(*Document) error { return nil }); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run = %v, want context.Canceled", err)
	}
}

func waitFor(t *testing.T, ok func() bool, message string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal(message)
}

func TestOriginMismatchDoesNotAcknowledgeRevision(t *testing.T) {
	s := store(t)
	if err := s.RecordOrigin("https://original.example"); err != nil {
		t.Fatal(err)
	}
	c := client(t, s, func(w http.ResponseWriter, r *http.Request) {
		body := document(7)
		body["origin"] = "https://different.example"
		writeJSON(t, w, http.StatusOK, body)
	})
	_, err := c.once(context.Background(), func(d *Document) error { return d.Adopt(nil, s) })
	var mismatch *state.OriginMismatchError
	if !errors.As(err, &mismatch) {
		t.Fatalf("expected typed mismatch: %v", err)
	}
	if c.Ready() || c.Revision() != state.NoRevision {
		t.Fatal("refused revision acknowledged")
	}
	saved, _ := s.LoadNode()
	if saved.LastRevision != state.NoRevision || saved.GatewayAddr != "" {
		t.Fatal("refused revision persisted")
	}
}
