package traffic

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gentility-ai/gentility/connector/internal/tsnode"
)

type mothership struct {
	mu      sync.Mutex
	status  int
	next    int64
	raw     string
	conns   int
	batches chan Batch
	headers chan http.Header
}

func newMothership(status int, next int64) *mothership {
	return &mothership{status: status, next: next, batches: make(chan Batch, 16), headers: make(chan http.Header, 16)}
}

func (m *mothership) set(status int, next int64) {
	m.mu.Lock()
	m.status, m.next, m.raw = status, next, ""
	m.mu.Unlock()
}

// answer has the mothership answer status with body as it stands.
func (m *mothership) answer(status int, body string) {
	m.mu.Lock()
	m.status, m.raw = status, body
	m.mu.Unlock()
}

// connections is how many TCP connections the reporter has opened.
func (m *mothership) connections() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.conns
}

func (m *mothership) connState(_ net.Conn, state http.ConnState) {
	if state == http.StateNew {
		m.mu.Lock()
		m.conns++
		m.mu.Unlock()
	}
}

func (m *mothership) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var b Batch
	zr, err := gzip.NewReader(r.Body)
	if err == nil {
		err = json.NewDecoder(zr).Decode(&b)
	}
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	m.headers <- r.Header.Clone()
	m.batches <- b
	if r.URL.Path != Path {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	m.mu.Lock()
	status, next, raw := m.status, m.next, m.raw
	m.mu.Unlock()
	if raw != "" {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(raw))
		return
	}
	if status != http.StatusOK {
		// As the mothership does: every refusal carries a JSON error.
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"error":"refused"}`))
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]int64{"next_interval_ms": next})
}

// rig drives a Reporter by hand: the test sets the counters and the clock,
// takes samples, and decides when a send is due. Every channel is
// unbuffered except asked, so a send the test makes completes only once the
// loop has taken it — and the loop finishes one event before it takes the
// next. A sample also waits for the loop's read, so the test cannot set the
// next counters before the loop has read these.
type rig struct {
	m        *mothership
	ticks    chan time.Time
	read     chan struct{}
	due      chan time.Time
	asked    chan time.Duration
	mu       sync.Mutex
	readings []tsnode.PeerCounter
	now      time.Time
	ready    bool
	broken   bool
}

func start(t *testing.T, m *mothership, configure func(*Reporter)) *rig {
	t.Helper()
	srv := httptest.NewUnstartedServer(m)
	srv.Config.ConnState = m.connState
	srv.Start()
	g := &rig{m: m, ticks: make(chan time.Time), read: make(chan struct{}), due: make(chan time.Time), asked: make(chan time.Duration, 16), now: t0, ready: true}
	r := &Reporter{
		BaseURL: srv.URL, Credential: "node-secret", HTTP: srv.Client(),
		Read: func(context.Context) ([]tsnode.PeerCounter, error) {
			defer func() { g.read <- struct{}{} }()
			g.mu.Lock()
			defer g.mu.Unlock()
			if g.broken {
				return nil, errors.New("the engine is not up")
			}
			return append([]tsnode.PeerCounter(nil), g.readings...), nil
		},
		Ready: func() bool {
			g.mu.Lock()
			defer g.mu.Unlock()
			return g.ready
		},
		SampleTicks: g.ticks,
		After:       func(d time.Duration) <-chan time.Time { g.asked <- d; return g.due },
		Now: func() time.Time {
			g.mu.Lock()
			defer g.mu.Unlock()
			return g.now
		},
	}
	if configure != nil {
		configure(r)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = r.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done; srv.Close() })
	return g
}

// sample sets the counters and the clock, then has the loop take one sample
// and waits until it has read them.
func (g *rig) sample(at time.Time, readings ...tsnode.PeerCounter) {
	g.mu.Lock()
	g.now, g.readings = at, readings
	g.mu.Unlock()
	g.ticks <- at
	<-g.read
}

// interval is the wait the loop asked for before its next send.
func (g *rig) interval(t *testing.T) time.Duration {
	t.Helper()
	select {
	case d := <-g.asked:
		return d
	case <-time.After(5 * time.Second):
		t.Fatal("the reporter never scheduled a send")
		return 0
	}
}

// send makes the pending send due and returns what the mothership received.
func (g *rig) send(t *testing.T) Batch {
	t.Helper()
	g.due <- time.Time{}
	select {
	case b := <-g.m.batches:
		return b
	case <-time.After(5 * time.Second):
		t.Fatal("nothing was posted")
		return Batch{}
	}
}

func TestAnIdleNodeStillPostsAnEmptyBatch(t *testing.T) {
	g := start(t, newMothership(http.StatusOK, 10_000), nil)
	if d := g.interval(t); d != DefaultInterval {
		t.Fatalf("first send after %s", d)
	}
	b := g.send(t)
	if b.TrafficReportVersion != 1 || len(b.Peers) != 0 || len(b.Covered) != 0 {
		t.Fatalf("batch %+v", b)
	}
}

func TestTheNextSendFollowsTheAnswerWithinItsBounds(t *testing.T) {
	m := newMothership(http.StatusOK, 2_000)
	g := start(t, m, nil)
	g.interval(t)
	g.send(t)
	if d := g.interval(t); d != 2*time.Second {
		t.Fatalf("asked 2 s, waited %s", d)
	}
	m.set(http.StatusOK, 200)
	g.send(t)
	if d := g.interval(t); d != MinInterval {
		t.Fatalf("asked 200 ms, waited %s", d)
	}
	m.set(http.StatusOK, 600_000)
	g.send(t)
	if d := g.interval(t); d != MaxInterval {
		t.Fatalf("asked 10 min, waited %s", d)
	}
}

func TestSecondsGoOutAsPerPeerDeltasGzippedWithTheCredential(t *testing.T) {
	g := start(t, newMothership(http.StatusOK, 10_000), nil)
	g.interval(t)
	handshake := t0.Add(-time.Minute)
	at := func(rx, tx int64) tsnode.PeerCounter {
		return tsnode.PeerCounter{Address: cedar, Rx: rx, Tx: tx, LastHandshake: handshake}
	}
	g.sample(t0, at(100, 1000))
	g.sample(t0.Add(time.Second), at(150, 1600))
	g.sample(t0.Add(2*time.Second), at(150, 1600))
	g.sample(t0.Add(3*time.Second), at(160, 1700))

	b := g.send(t)
	h := <-g.m.headers
	if h.Get("Content-Encoding") != "gzip" || h.Get("Content-Type") != "application/json" ||
		h.Get("Authorization") != "Bearer node-secret" || h.Get("X-Node-Capver") == "" {
		t.Fatalf("headers %v", h)
	}
	want := Peer{Peer: cedar, From: t0.Unix() + 1, Rx: []int64{50, 0, 10}, Tx: []int64{600, 0, 100}, LastHandshake: ptr(handshake.Unix())}
	if len(b.Peers) != 1 || !reflect.DeepEqual(b.Peers[0], want) {
		t.Fatalf("peers %+v", b.Peers)
	}
	// The first reading is the baseline: t0 itself is not covered.
	if !reflect.DeepEqual(b.Covered, [][2]int64{{t0.Unix() + 1, t0.Unix() + 3}}) {
		t.Fatalf("covered %v", b.Covered)
	}
}

func TestAnIdleNodeCoversTheSecondsItRead(t *testing.T) {
	g := start(t, newMothership(http.StatusOK, 10_000), nil)
	g.interval(t)
	for i := 0; i < 3; i++ {
		g.sample(t0.Add(time.Duration(i)*time.Second), counter(cedar, 5, 5))
	}
	b := g.send(t)
	if len(b.Peers) != 0 || !reflect.DeepEqual(b.Covered, [][2]int64{{t0.Unix() + 1, t0.Unix() + 2}}) {
		t.Fatalf("idle batch %+v", b)
	}
}

func TestASecondThatCouldNotBeReadIsNotCovered(t *testing.T) {
	g := start(t, newMothership(http.StatusOK, 10_000), nil)
	g.interval(t)
	g.sample(t0, counter(cedar, 0, 0))
	g.sample(t0.Add(time.Second), counter(cedar, 0, 0))
	g.mu.Lock()
	g.broken = true
	g.mu.Unlock()
	g.sample(t0.Add(2 * time.Second))
	g.mu.Lock()
	g.broken = false
	g.mu.Unlock()
	g.sample(t0.Add(3*time.Second), counter(cedar, 0, 0))

	b := g.send(t)
	want := [][2]int64{{t0.Unix() + 1, t0.Unix() + 1}, {t0.Unix() + 3, t0.Unix() + 3}}
	if !reflect.DeepEqual(b.Covered, want) {
		t.Fatalf("covered %v, want %v", b.Covered, want)
	}
}

// A 429 is the mothership's rate floor: the batch was not taken, so it is
// kept and sent again at the interval, like any failure.
func TestAFailedSendKeepsItsSecondsForTheNext(t *testing.T) {
	for _, status := range []int{http.StatusInternalServerError, http.StatusTooManyRequests} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			m := newMothership(status, 0)
			g := start(t, m, nil)
			g.interval(t)
			g.sample(t0, counter(cedar, 0, 0))
			g.sample(t0.Add(time.Second), counter(cedar, 10, 10))
			g.send(t)
			if d := g.interval(t); d != DefaultInterval {
				t.Fatalf("after a %d, waited %s", status, d)
			}

			m.set(http.StatusOK, 10_000)
			g.sample(t0.Add(2*time.Second), counter(cedar, 30, 30))
			b := g.send(t)
			if len(b.Peers) != 1 || b.Peers[0].From != t0.Unix()+1 || len(b.Peers[0].Rx) != 2 {
				t.Fatalf("the second refused with a %d was not resent: %+v", status, b.Peers)
			}

			g.interval(t)
			if b := g.send(t); len(b.Peers) != 0 {
				t.Fatalf("an acknowledged second was sent again: %+v", b.Peers)
			}
		})
	}
}

func TestA404BacksOffTenMinutesAndKeepsTheSeconds(t *testing.T) {
	m := newMothership(http.StatusNotFound, 0)
	g := start(t, m, nil)
	g.interval(t)
	g.sample(t0, counter(cedar, 0, 0))
	g.sample(t0.Add(time.Second), counter(cedar, 10, 10))
	g.send(t)
	if d := g.interval(t); d != NotHereInterval {
		t.Fatalf("after a 404, waited %s", d)
	}
	m.set(http.StatusOK, 10_000)
	if b := g.send(t); len(b.Peers) != 1 {
		t.Fatalf("the seconds were not kept through the 404: %+v", b.Peers)
	}
}

func TestAnOversizedBatchGoesOldestFirstInPieces(t *testing.T) {
	// Eight seconds of 100 kB a second encode past 260 bytes; one second
	// encodes well under it, so a piece holds several seconds, not one.
	g := start(t, newMothership(http.StatusOK, 10_000), func(r *Reporter) { r.MaxBody = 260 })
	g.interval(t)
	g.sample(t0, counter(cedar, 0, 0))
	for i := int64(1); i <= 8; i++ {
		g.sample(t0.Add(time.Duration(i)*time.Second), counter(cedar, i*100_000, i*100_000))
	}

	first := g.send(t)
	g.interval(t)
	second := g.send(t)

	sent := int64(len(first.Peers[0].Rx))
	if first.Peers[0].From != t0.Unix()+1 || sent <= 1 || sent >= 8 {
		t.Fatalf("first piece %+v", first.Peers[0])
	}
	if !reflect.DeepEqual(first.Covered, [][2]int64{{t0.Unix() + 1, t0.Unix() + sent}}) {
		t.Fatalf("the first piece covers %v, not the oldest %d seconds", first.Covered, sent)
	}
	if second.Peers[0].From != first.Peers[0].From+sent ||
		!reflect.DeepEqual(second.Covered, [][2]int64{{t0.Unix() + sent + 1, t0.Unix() + 8}}) {
		t.Fatalf("second piece %+v, covered %v, does not follow the first", second.Peers[0], second.Covered)
	}
}

func TestNothingIsSentBeforeTheNodeIsReady(t *testing.T) {
	g := start(t, newMothership(http.StatusOK, 10_000), nil)
	g.mu.Lock()
	g.ready = false
	g.mu.Unlock()

	g.interval(t)
	g.due <- time.Time{}
	if d := g.interval(t); d != readyPoll {
		t.Fatalf("not ready, waited %s", d)
	}
	select {
	case b := <-g.m.batches:
		t.Fatalf("posted %+v before the node was ready", b)
	default:
	}
}

func TestA404WaitsOnceAndAFailureAfterItUsesTheNormalInterval(t *testing.T) {
	m := newMothership(http.StatusNotFound, 0)
	g := start(t, m, nil)
	g.interval(t)
	g.send(t)
	if d := g.interval(t); d != NotHereInterval {
		t.Fatalf("after a 404, waited %s", d)
	}
	m.set(http.StatusBadGateway, 0)
	g.send(t)
	if d := g.interval(t); d != DefaultInterval {
		t.Fatalf("after a 404 and then a 502, waited %s", d)
	}
}

func TestAClockThatStepsBackStillFilesAscendingSeconds(t *testing.T) {
	g := start(t, newMothership(http.StatusOK, 10_000), nil)
	g.interval(t)
	g.sample(t0, counter(cedar, 0, 0))
	g.sample(t0.Add(time.Second), counter(cedar, 1, 1))
	g.sample(t0.Add(2*time.Second), counter(cedar, 3, 3))
	g.sample(t0, counter(cedar, 6, 6))
	g.sample(t0, counter(cedar, 10, 10))

	b := g.send(t)
	want := Peer{Peer: cedar, From: t0.Unix() + 1, Rx: []int64{1, 2, 3, 4}, Tx: []int64{1, 2, 3, 4}}
	if len(b.Peers) != 1 || !reflect.DeepEqual(b.Peers[0], want) {
		t.Fatalf("peers %+v", b.Peers)
	}
	if !reflect.DeepEqual(b.Covered, [][2]int64{{t0.Unix() + 1, t0.Unix() + 4}}) {
		t.Fatalf("covered %v", b.Covered)
	}
}

func TestABatchTheMothershipWillNeverTakeIsDropped(t *testing.T) {
	for _, status := range []int{http.StatusRequestEntityTooLarge, http.StatusUnprocessableEntity} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			m := newMothership(status, 0)
			g := start(t, m, nil)
			g.interval(t)
			g.sample(t0, counter(cedar, 0, 0))
			g.sample(t0.Add(time.Second), counter(cedar, 10, 10))
			if b := g.send(t); len(b.Peers) != 1 {
				t.Fatalf("sent %+v", b.Peers)
			}
			if d := g.interval(t); d != DefaultInterval {
				t.Fatalf("after a %d, waited %s", status, d)
			}
			m.set(http.StatusOK, 10_000)
			if b := g.send(t); len(b.Peers) != 0 || len(b.Covered) != 0 {
				t.Fatalf("a refused batch was sent again: %+v", b)
			}
		})
	}
}

func TestATakenBatchWithAnUnreadableAnswerKeepsTheInterval(t *testing.T) {
	m := newMothership(http.StatusOK, 2_000)
	g := start(t, m, nil)
	g.interval(t)
	g.send(t)
	if d := g.interval(t); d != 2*time.Second {
		t.Fatalf("asked 2 s, waited %s", d)
	}

	m.answer(http.StatusOK, "<html>a proxy's page</html>")
	g.sample(t0, counter(cedar, 0, 0))
	g.sample(t0.Add(time.Second), counter(cedar, 10, 10))
	if b := g.send(t); len(b.Peers) != 1 {
		t.Fatalf("sent %+v", b.Peers)
	}
	if d := g.interval(t); d != 2*time.Second {
		t.Fatalf("after an unreadable answer, waited %s", d)
	}
	m.set(http.StatusOK, 10_000)
	if b := g.send(t); len(b.Peers) != 0 {
		t.Fatalf("a taken batch was sent again: %+v", b.Peers)
	}
}

// Every answer is read to its end, so one connection carries every send.
func TestEverySendReusesTheConnection(t *testing.T) {
	m := newMothership(http.StatusInternalServerError, 0)
	g := start(t, m, nil)
	g.interval(t)
	g.send(t)
	g.interval(t)
	g.send(t)
	m.set(http.StatusOK, 10_000)
	g.interval(t)
	g.send(t)
	g.interval(t)
	g.send(t)
	g.interval(t)
	if n := m.connections(); n != 1 {
		t.Fatalf("four sends opened %d connections", n)
	}
}

// logs collects what the reporter logs.
type logs struct {
	mu    sync.Mutex
	lines []string
}

func (l *logs) log(format string, args ...any) {
	l.mu.Lock()
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
	l.mu.Unlock()
}

func (l *logs) containing(text string) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []string
	for _, line := range l.lines {
		if strings.Contains(line, text) {
			out = append(out, line)
		}
	}
	return out
}

// A clock stepped back would otherwise leave every later label as far ahead
// of the wall clock, for good — past the mothership's slack for the future,
// where it drops them. The reporter starts again from the wall clock.
func TestAClockThatStepsFarBackReanchorsTheSeconds(t *testing.T) {
	var l logs
	g := start(t, newMothership(http.StatusOK, 10_000), func(r *Reporter) { r.Log = l.log })
	g.interval(t)
	g.sample(t0, counter(cedar, 0, 0))
	g.sample(t0.Add(time.Second), counter(cedar, 10, 10))
	g.sample(t0.Add(2*time.Second), counter(cedar, 20, 20))

	back := t0.Add(-100 * time.Second)
	g.sample(back, counter(cedar, 30, 30))
	g.sample(back.Add(time.Second), counter(cedar, 45, 45))
	g.sample(back.Add(2*time.Second), counter(cedar, 65, 65))

	b := g.send(t)
	want := Peer{Peer: cedar, From: back.Unix() + 1, Rx: []int64{15, 20}, Tx: []int64{15, 20}}
	if len(b.Peers) != 1 || !reflect.DeepEqual(b.Peers[0], want) {
		t.Fatalf("peers %+v", b.Peers)
	}
	if !reflect.DeepEqual(b.Covered, [][2]int64{{back.Unix() + 1, back.Unix() + 2}}) {
		t.Fatalf("covered %v, want the seconds after the step, on the wall clock", b.Covered)
	}
	if lines := l.containing("stepped back"); len(lines) != 1 {
		t.Fatalf("logged %v", l.lines)
	}
}

// A tick that comes late puts a label a second or two ahead of the wall
// clock; that is no step.
func TestLateTicksDoNotReanchor(t *testing.T) {
	var l logs
	g := start(t, newMothership(http.StatusOK, 10_000), func(r *Reporter) { r.Log = l.log })
	g.interval(t)
	g.sample(t0, counter(cedar, 0, 0))
	g.sample(t0.Add(time.Second), counter(cedar, 1, 1))
	g.sample(t0.Add(time.Second), counter(cedar, 2, 2))
	g.sample(t0.Add(time.Second), counter(cedar, 3, 3))
	g.sample(t0.Add(2*time.Second), counter(cedar, 4, 4))
	g.sample(t0.Add(3*time.Second), counter(cedar, 5, 5))

	b := g.send(t)
	if len(b.Peers) != 1 || b.Peers[0].From != t0.Unix()+1 || len(b.Peers[0].Rx) != 5 {
		t.Fatalf("peers %+v", b.Peers)
	}
	if !reflect.DeepEqual(b.Covered, [][2]int64{{t0.Unix() + 1, t0.Unix() + 5}}) {
		t.Fatalf("covered %v", b.Covered)
	}
	if lines := l.containing("stepped back"); len(lines) != 0 {
		t.Fatalf("re-anchored on late ticks: %v", lines)
	}
}
