package phonenode

import (
	"fmt"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// goroutineID is the running goroutine's number, from its stack's first
// line: "goroutine 42 [running]:".
func goroutineID() string {
	buf := make([]byte, 64)
	buf = buf[:runtime.Stack(buf, false)]
	return strings.Fields(string(buf))[1]
}

// lineLogger is a Logger that keeps each line and the goroutine that handed
// it over, and can be made to hold every Log until block is closed — Swift
// dispatching to a busy main thread.
type lineLogger struct {
	mu    sync.Mutex
	lines []string
	from  map[string]bool
	block chan struct{}
}

func (l *lineLogger) Log(line string) {
	if l.block != nil {
		<-l.block
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.from == nil {
		l.from = map[string]bool{}
	}
	l.lines = append(l.lines, line)
	l.from[goroutineID()] = true
}

func (l *lineLogger) heard() (lines []string, from map[string]bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	from = map[string]bool{}
	for id := range l.from {
		from[id] = true
	}
	return slices.Clone(l.lines), from
}

// returnsPromptly fails the test if f, run on a goroutine of its own, has not
// returned within a second.
func returnsPromptly(t *testing.T, what string, f func()) {
	t.Helper()
	done := make(chan struct{})
	go func() { f(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatalf("%s waited on something it should not", what)
	}
}

// The app's Logger hops to the main actor. Called on the caller's thread, a
// Log from inside Down — itself called on the main thread — would wait on
// itself; called from many goroutines at once, the app's log would interleave.
func TestLogLinesArriveInOrderFromOneGoroutineThatIsNotTheCallers(t *testing.T) {
	log := &lineLogger{}
	logf := logger(log)
	for i := range 200 {
		logf("line %d", i)
	}
	eventually(t, "200 lines heard", func() bool { lines, _ := log.heard(); return len(lines) == 200 })

	lines, from := log.heard()
	for i, line := range lines {
		if want := fmt.Sprintf(" line %d", i); !strings.HasSuffix(line, want) {
			t.Fatalf("line %d = %q, want …%q", i, line, want)
		}
	}
	if from[goroutineID()] {
		t.Fatal("Log was called on the goroutine that logged")
	}
	if len(from) != 1 {
		t.Fatalf("Log was called from %d goroutines, want one", len(from))
	}
}

// A Logger that falls behind loses lines rather than hold up the node, and
// hears how many it lost where it lost them.
func TestALoggerThatFallsBehindIsToldHowManyLinesItLost(t *testing.T) {
	q := newLogQueue(4)
	log := &lineLogger{block: make(chan struct{})}
	logf := q.logger(log)

	logf("in hand")
	eventually(t, "the first line in the Logger's hands", func() bool { return len(q.lines) == 0 })
	for i := range 4 + 3 { // four queued, three past the queue
		logf("queued %d", i)
	}
	close(log.block)
	eventually(t, "the queue drained", func() bool { lines, _ := log.heard(); return len(lines) == 5 })
	logf("after")
	eventually(t, "the line after the loss", func() bool { lines, _ := log.heard(); return len(lines) == 7 })

	lines, _ := log.heard()
	want := []string{"in hand", "queued 0", "queued 1", "queued 2", "queued 3", "phonenode: 3 log lines dropped", "after"}
	for i, line := range lines {
		if !strings.HasSuffix(line, " "+want[i]) {
			t.Fatalf("line %d = %q, want …%q (all: %q)", i, line, want[i], lines)
		}
	}
}
