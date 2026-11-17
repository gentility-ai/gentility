package refusal

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type mothership struct {
	mu      sync.Mutex
	status  int
	batches []Batch
	headers []http.Header
}

func (m *mothership) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var b Batch
	_ = json.NewDecoder(r.Body).Decode(&b)
	m.batches = append(m.batches, b)
	m.headers = append(m.headers, r.Header.Clone())
	if r.URL.Path != Path {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	w.WriteHeader(m.status)
}

func harness(t *testing.T, status int) (*mothership, chan Attempt, chan time.Time, *[]string, func()) {
	// Both channels are unbuffered, so a send completes only once the loop
	// has taken it: a second tick proves the first tick's report is done.
	m := &mothership{status: status}
	srv := httptest.NewServer(m)
	in, ticks := make(chan Attempt), make(chan time.Time)
	var lines []string
	var mu sync.Mutex
	r := &Reporter{
		BaseURL: srv.URL, Credential: "node-secret", HTTP: srv.Client(),
		Attempts: in, Ticks: ticks, Now: func() time.Time { return t0 },
		Log: func(f string, a ...any) { mu.Lock(); lines = append(lines, fmt.Sprintf(f, a...)); mu.Unlock() },
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { r.Run(ctx); close(done) }()
	return m, in, ticks, &lines, func() { cancel(); <-done; srv.Close() }
}

func TestNothingIsSentUntilThereIsSomethingToSay(t *testing.T) {
	m, _, ticks, _, stop := harness(t, http.StatusNoContent)
	ticks <- t0
	ticks <- t0 // a second tick proves the first was handled
	stop()
	if len(m.batches) != 0 {
		t.Fatalf("sent %d empty batches", len(m.batches))
	}
}

func TestATickSendsTheBatchWithTheNodesCredential(t *testing.T) {
	m, in, ticks, lines, stop := harness(t, http.StatusNoContent)
	in <- Attempt{Side: Destination, Src: mac, Dst: cedar, Port: 8080}
	in <- Attempt{Side: Destination, Src: mac, Dst: cedar, Port: 8080}
	ticks <- t0
	ticks <- t0
	stop()
	if len(m.batches) != 1 || m.batches[0].Edges[0].Count != 2 {
		t.Fatalf("batches %+v", m.batches)
	}
	h := m.headers[0]
	if h.Get("Authorization") != "Bearer node-secret" || h.Get("X-Node-Capver") == "" || h.Get("Content-Type") != "application/json" {
		t.Fatalf("headers %v", h)
	}
	for _, line := range *lines {
		if strings.Contains(line, "node-secret") {
			t.Fatalf("the credential was logged: %q", line)
		}
	}
	if !anyContains(*lines, "reported 1 refused") || !anyContains(*lines, "fd06:6e7:0:2::4 -> fd06:6e7:0:2::5:8080") {
		t.Fatalf("log %q", *lines)
	}
}

func TestAFailedReportIsKeptAndMergedIntoTheNext(t *testing.T) {
	m, in, ticks, _, stop := harness(t, http.StatusServiceUnavailable)
	in <- Attempt{Side: Destination, Src: mac, Dst: cedar, Port: 8080}
	ticks <- t0
	// in is unbuffered: this send completes only once the first report is done.
	in <- Attempt{Side: Destination, Src: mac, Dst: cedar, Port: 8080}
	m.mu.Lock()
	m.status = http.StatusNoContent
	m.mu.Unlock()
	ticks <- t0
	ticks <- t0
	stop()
	if len(m.batches) != 2 || m.batches[1].Edges[0].Count != 2 {
		t.Fatalf("batches %+v", m.batches)
	}
}

func TestABatchTheMothershipWillNeverTakeIsDropped(t *testing.T) {
	for _, status := range []int{http.StatusNotFound, http.StatusUnprocessableEntity} {
		m, in, ticks, lines, stop := harness(t, status)
		in <- Attempt{Side: Destination, Src: mac, Dst: cedar, Port: 8080}
		ticks <- t0
		ticks <- t0
		stop()
		if len(m.batches) != 1 {
			t.Fatalf("%d: batches %+v", status, m.batches)
		}
		if !anyContains(*lines, "dropped") {
			t.Fatalf("%d: log %q", status, *lines)
		}
	}
}

func TestAPeersRejectionIsLoggedOnceAMinuteAndNeverSent(t *testing.T) {
	m, in, ticks, lines, stop := harness(t, http.StatusNoContent)
	for i := 0; i < 5; i++ {
		in <- Attempt{Side: Rejected, Src: mac, Dst: cedar, Port: 8080}
	}
	ticks <- t0
	ticks <- t0
	stop()
	if len(m.batches) != 0 {
		t.Fatalf("a rejection was reported: %+v", m.batches)
	}
	n := 0
	for _, line := range *lines {
		if strings.Contains(line, "refused by fd06:6e7:0:2::5's rules: fd06:6e7:0:2::5:8080") {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("logged %d times; want once: %q", n, *lines)
	}
}

func anyContains(lines []string, s string) bool {
	for _, l := range lines {
		if strings.Contains(l, s) {
			return true
		}
	}
	return false
}

func TestARejectionIsLoggedAgainAfterAMinute(t *testing.T) {
	var mu sync.Mutex
	now, n := t0, 0
	in, ticks := make(chan Attempt), make(chan time.Time)
	r := &Reporter{
		Attempts: in, Ticks: ticks,
		Now: func() time.Time { mu.Lock(); defer mu.Unlock(); return now },
		Log: func(string, ...any) { mu.Lock(); n++; mu.Unlock() },
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { r.Run(ctx); close(done) }()
	advance := func(d time.Duration) { mu.Lock(); now = now.Add(d); mu.Unlock() }

	in <- Attempt{Side: Rejected, Src: mac, Dst: cedar, Port: 8080}
	advance(59 * time.Second)
	ticks <- now
	in <- Attempt{Side: Rejected, Src: mac, Dst: cedar, Port: 8080}
	advance(time.Second)
	ticks <- now
	in <- Attempt{Side: Rejected, Src: mac, Dst: cedar, Port: 8080}
	cancel()
	<-done
	if n != 2 {
		t.Fatalf("logged %d times; want twice", n)
	}
}
