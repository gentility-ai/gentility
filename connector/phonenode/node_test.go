package phonenode

import (
	"context"
	"encoding/json"
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gentility-ai/gentility/connector/internal/config"
	"github.com/gentility-ai/gentility/connector/internal/enroll"
	"github.com/gentility-ai/gentility/connector/internal/exitcodes"
	"github.com/gentility-ai/gentility/connector/internal/state"
	"github.com/gentility-ai/gentility/connector/internal/tsnode"
)

// enrollment is join material as the claim poll hands it over, with secrets
// a test can look for in what comes back.
const enrollment = `{"status":"allowed","enrollment":{"enrollment_id":"e1","preauth_key":"pk-secret-xyz",` +
	`"nonce":"nonce-secret-xyz","head_url":"https://head.invalid","expires_at":"2026-10-24T12:10:00Z"}}`

func decode(t *testing.T, raw string) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatalf("not a JSON answer: %q: %v", raw, err)
	}
	return out
}

// down leaves the package as a test found it: no node, no join, no doors.
func down(t *testing.T) {
	t.Helper()
	t.Cleanup(func() { Down() })
}

// holdJoins makes every join wait in place of the netmap until it is called
// off, and says when one has started waiting.
func holdJoins(t *testing.T) <-chan struct{} {
	t.Helper()
	entered := make(chan struct{}, 4)
	bringUp = func(ctx context.Context, _ enroll.Env, _ func(string, ...any)) (*tsnode.Node, []netip.Addr, error) {
		entered <- struct{}{}
		<-ctx.Done()
		return nil, nil, ctx.Err()
	}
	t.Cleanup(func() { bringUp = enroll.Join })
	return entered
}

func TestRejoinWithNoStateDirSaysNotEnrolled(t *testing.T) {
	down(t)
	dir := filepath.Join(t.TempDir(), "deleted")

	got := decode(t, Rejoin(dir, "iphone", nil))
	if got["code"] != "not_enrolled" {
		t.Fatalf("Rejoin = %v, want code not_enrolled", got)
	}
	if state := decode(t, Status())["state"]; state != "down" {
		t.Fatalf("Status after a refused rejoin = %v, want down", state)
	}
}

// The identity survived but tsnet's node key did not: there is nothing to
// present to the head, and saying so must not cost a dial (tsnode's guard
// runs before tsnet starts).
func TestRejoinWithNoTsnetStateSaysNotEnrolled(t *testing.T) {
	down(t)
	dir := t.TempDir()
	if err := (&state.Store{Dir: dir}).SaveNode(state.Node{
		NodeID:         "n1",
		NodeCredential: "cred-secret-xyz",
		HeadURL:        "https://head.invalid",
		Hostname:       "iphone",
	}); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	got := decode(t, Rejoin(dir, "iphone", nil))
	if got["code"] != "not_enrolled" {
		t.Fatalf("Rejoin = %v, want code not_enrolled", got)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("the refusal waited on something; it should be decided on disk")
	}
	if strings.Contains(got["error"].(string), "cred-secret-xyz") {
		t.Fatal("the refusal carried the node credential")
	}
}

// Before the first unlock after a restart, iOS keeps a file under
// complete-until-first-authentication unreadable. That is not a phone that
// was never paired, and the app wipes a pairing on not_enrolled.
func TestRejoinThatCannotReadItsIdentitySaysUnreadable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a 0000 file")
	}
	down(t)
	dir := t.TempDir()
	if err := (&state.Store{Dir: dir}).SaveNode(state.Node{NodeID: "n1", NodeCredential: "c", HeadURL: "h", Hostname: "iphone"}); err != nil {
		t.Fatal(err)
	}
	identity := filepath.Join(dir, "node.json")
	if err := os.Chmod(identity, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(identity, 0o600) })

	if got := decode(t, Rejoin(dir, "iphone", nil)); got["code"] != "unreadable" {
		t.Fatalf("Rejoin = %v, want code unreadable", got)
	}
}

func TestJoinRefusesMaterialItCannotUseWithoutEchoingIt(t *testing.T) {
	down(t)
	for name, raw := range map[string]string{
		"not JSON":       `pk-secret-xyz`,
		"no preauth key": `{"enrollment":{}}`,
		"no nonce":       `{"enrollment":{"enrollment_id":"e1","preauth_key":"pk-secret-xyz","head_url":"https://head.invalid"}}`,
		"bare, no nonce": `{"enrollment_id":"e1","preauth_key":"pk-secret-xyz","head_url":"https://head.invalid"}`,
	} {
		t.Run(name, func(t *testing.T) {
			got := decode(t, Join(t.TempDir(), "iphone", raw, nil))
			if got["code"] != "malformed" {
				t.Fatalf("Join = %v, want code malformed", got)
			}
			if strings.Contains(got["error"].(string), "pk-secret-xyz") {
				t.Fatal("the refusal carried the pre-auth key")
			}
		})
	}
}

// The netmap wait is six or seven seconds on a phone. Status and Down are
// what the app calls in that time, and neither may wait for it.
func TestStatusAndDownAnswerWhileAJoinIsUnderWay(t *testing.T) {
	down(t)
	entered := holdJoins(t)
	result := make(chan string, 1)
	go func() { result <- Join(t.TempDir(), "iphone", enrollment, nil) }()
	<-entered

	start := time.Now()
	if got := decode(t, Status()); got["state"] != "joining" {
		t.Fatalf("Status during a join = %v, want joining", got)
	}
	if got := decode(t, Join(t.TempDir(), "iphone", enrollment, nil)); got["code"] != "joining" {
		t.Fatalf("a second Join = %v, want code joining", got)
	}
	if time.Since(start) > time.Second {
		t.Fatal("Status or Join waited on the join under way")
	}

	if got := decode(t, Down()); got["ok"] != true {
		t.Fatalf("Down = %v", got)
	}
	select {
	case raw := <-result:
		if got := decode(t, raw); got["code"] != "cancelled" {
			t.Fatalf("the called-off Join = %v, want code cancelled", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Down did not call the join off")
	}
	if got := decode(t, Status()); got["state"] != "down" {
		t.Fatalf("Status after Down = %v, want down", got)
	}
}

func TestDownWithNothingUpIsFine(t *testing.T) {
	if got := decode(t, Down()); got["ok"] != true {
		t.Fatalf("Down = %v", got)
	}
}

func TestClassifySaysWhatTheAppDoesNext(t *testing.T) {
	for _, c := range []struct {
		err  error
		want string
	}{
		{tsnode.ErrNoIdentity, "not_enrolled"},
		{exitcodes.Wrap(exitcodes.EnrollRefused, tsnode.ErrRejoinRefused), "revoked"},
		{exitcodes.Wrap(exitcodes.EnrollRefused, errors.New("credential refused")), "revoked"},
		{exitcodes.Wrap(exitcodes.MaterialExpired, errors.New("pre-auth key expired")), "expired"},
		{errors.New("head down"), "join_failed"},
	} {
		if got := classify(c.err); got != c.want {
			t.Errorf("classify(%v) = %q, want %q", c.err, got, c.want)
		}
	}
}

// fakeSession is a node that is up with no engine under it: report answers
// for its status (nil: a node with nothing yet), and its shutdown is at once
// unless a test sets closeNode.
func fakeSession(t *testing.T, report func(context.Context) (tsnode.Report, error)) *session {
	t.Helper()
	if report == nil {
		report = func(context.Context) (tsnode.Report, error) { return tsnode.Report{}, nil }
	}
	_, cancel := context.WithCancel(context.Background())
	return &session{
		report:       report,
		closeNode:    func() {},
		configs:      config.New("https://head.invalid", state.Node{}, &state.Store{Dir: t.TempDir()}, nil),
		cancel:       cancel,
		logf:         func(string, ...any) {},
		nodeID:       "n1",
		channelEnded: make(chan error, 1),
	}
}

func makeCurrent(s *session) {
	mu.Lock()
	current = s
	mu.Unlock()
}

// heldShutdown makes s's shutdown wait until the returned release is called
// (the test's end calls it too), and closes closing once it has begun.
func heldShutdown(t *testing.T, s *session) (closing <-chan struct{}, release func()) {
	t.Helper()
	shut, begun := make(chan struct{}), make(chan struct{})
	s.closeNode = func() { close(begun); <-shut }
	release = sync.OnceFunc(func() { close(shut) })
	t.Cleanup(release)
	return begun, release
}

// A node's shutdown takes as long as tsnet takes. A join that has to shut
// its own node down — Down came in late, or the channel ended as it came up
// — must not hold Status while it does.
func TestStatusAnswersWhileAJoinShutsItsNodeDown(t *testing.T) {
	for name, c := range map[string]struct {
		late bool // Down after the node is up, before it is current
		want string
	}{
		"called off late":                 {late: true, want: "cancelled"},
		"its channel ended as it came up": {want: "join_failed"},
	} {
		t.Run(name, func(t *testing.T) {
			down(t)
			brought := make(chan struct{})
			var closing <-chan struct{}
			var release func()
			bringSession = func(ctx context.Context, _ context.CancelFunc, _ enroll.Env, _ state.Node, _ bool, _ func(string, ...any)) (*session, map[string]any, error) {
				s := fakeSession(t, nil)
				closing, release = heldShutdown(t, s)
				close(brought)
				if c.late {
					<-ctx.Done()
				} else {
					s.channelEnded <- errors.New("credential refused")
				}
				return s, map[string]any{"ok": true}, nil
			}
			t.Cleanup(func() { bringSession = bring })

			joined := make(chan string, 1)
			go func() { joined <- Join(t.TempDir(), "iphone", enrollment, nil) }()
			<-brought
			if c.late {
				Down()
			}
			<-closing

			returnsPromptly(t, "Status", func() {
				if got := decode(t, Status()); got["state"] != "down" {
					t.Errorf("Status while the node closes = %v, want down", got)
				}
			})
			returnsPromptly(t, "a second Join", func() {
				if got := decode(t, Join(t.TempDir(), "iphone", enrollment, nil)); got["code"] != "joining" {
					t.Errorf("a Join while the last node closes = %v, want code joining", got)
				}
			})
			release()
			if got := decode(t, <-joined); got["code"] != c.want {
				t.Fatalf("the Join = %v, want code %s", got, c.want)
			}
		})
	}
}

// tsnet keeps its node key in the state dir. Two engines on it at once —
// the old one still closing, a Rejoin's starting — is two nodes with one
// key. Until the old one is down, a join is told to try again.
func TestANodeStillClosingHoldsOffTheNextJoin(t *testing.T) {
	for name, end := range map[string]func(*session){
		"Down":                      func(*session) { Down() },
		"its config channel ending": func(s *session) { endSession(s, errors.New("credential refused")) },
	} {
		t.Run(name, func(t *testing.T) {
			down(t)
			entered := holdJoins(t)
			s := fakeSession(t, nil)
			closing, release := heldShutdown(t, s)
			makeCurrent(s)
			ended := make(chan struct{})
			go func() { end(s); close(ended) }()
			<-closing

			joined := make(chan string, 1)
			go func() { joined <- Join(t.TempDir(), "iphone", enrollment, nil) }()
			select {
			case raw := <-joined:
				if got := decode(t, raw); got["code"] != "joining" {
					t.Fatalf("a Join while the last node closes = %v, want code joining", got)
				}
			case <-entered:
				t.Fatal("a second engine started on the state dir while the first was closing")
			case <-time.After(5 * time.Second):
				t.Fatal("the Join neither started nor answered")
			}
			returnsPromptly(t, "Status", func() { Status() })

			release()
			<-ended
			go func() { joined <- Join(t.TempDir(), "iphone", enrollment, nil) }()
			select {
			case <-entered:
			case raw := <-joined:
				t.Fatalf("a Join once the node had closed = %s, want it under way", raw)
			}
			Down()
			<-joined
		})
	}
}

// The app decodes Status into arrays; a null is a decoding failure.
func TestStatusOfANodeWithNothingYetSaysEmptyLists(t *testing.T) {
	down(t)
	makeCurrent(fakeSession(t, nil))
	got := decode(t, Status())
	for _, key := range []string{"addresses", "grants"} {
		if _, ok := got[key].([]any); !ok {
			t.Errorf("Status %s = %#v, want a JSON array", key, got[key])
		}
	}
}

func TestStatusThatCannotReadTheNodeSaysSoInAWord(t *testing.T) {
	down(t)
	makeCurrent(fakeSession(t, func(context.Context) (tsnode.Report, error) {
		return tsnode.Report{}, errors.New("tsnode: status: local client: gone")
	}))
	got := decode(t, Status())
	if got["state"] != "up" || got["code"] != "status_unknown" || got["error"] == "" {
		t.Fatalf("Status = %v, want state up, code status_unknown and the error", got)
	}
	for _, key := range []string{"addresses", "grants"} {
		if _, ok := got[key].([]any); !ok {
			t.Errorf("Status %s = %#v, want a JSON array", key, got[key])
		}
	}
}
