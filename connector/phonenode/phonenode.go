// Package phonenode is the phone's 6net node and its line to a Mac's door,
// bound into the iOS app with gomobile.
//
// The node half is the connector's own join, exchange, config channel and
// presence, run in userspace inside the app: enroll.Join → Mothership →
// Exchange → config.Client.Run → report.Reporter. The door half dials a
// Mac's door over that node and speaks WebSocket text frames, so Swift never
// needs a socket into tsnet and nothing listens on loopback.
//
// Everything exported takes strings, ints and the two callback interfaces,
// and answers a JSON string, because that is what gomobile binds cleanly. A
// failure is {"error": sentence, "code": word}: the app switches on the word
// and may show the sentence. No secret — pre-auth key, nonce, credential —
// is ever in either, or in a log line.
//
// For the iOS app:
//
//   - joining from Join or Rejoin straight after Down means the join Down
//     called off, or a node that ended on its own, is still letting go of dir
//     (tsnet keeps its key there, and two engines must not share it): try
//     again shortly.
//   - On scenePhase .active, Close every door and open it again rather than
//     wait on one that slept with the app: a door that died meanwhile is
//     only found out by its pings, up to about 40 s later, as door_silent.
//   - A pre-auth key that expired or was used before the first Join reads
//     join_failed, not expired: tsnet does not say which. The app knows from
//     the material's expires_at whether to show the pairing as expired.
//   - Status answers state "up" with code status_unknown when the engine did
//     not say how it stands; ask again. Its addresses and grants are always
//     arrays.
package phonenode

import (
	"encoding/json"
	"fmt"
	"sync"
	"time"
)

// Logger receives one line at a time, in order, from one goroutine of
// phonenode's own — never the thread that called into phonenode, and never
// while phonenode holds a lock. A Logger that falls thousands of lines
// behind loses lines, and hears "phonenode: N log lines dropped" where they
// were.
type Logger interface {
	Log(line string)
}

// The words a failure's "code" is one of.
const (
	codeMalformed     = "malformed"    // the input cannot be used as given
	codeUnreadable    = "unreadable"   // the identity is there but cannot be read yet: try later
	codeNotEnrolled   = "not_enrolled" // nothing to come back on: pair again
	codeRevoked       = "revoked"      // the mothership no longer holds this node: pair again
	codeExpired       = "expired"      // the join material is past its ten minutes: pair again
	codeAlreadyUp     = "already_up"
	codeJoining       = "joining"   // a join is under way, or the last node is still closing: try again
	codeCancelled     = "cancelled" // Down was called while the join was under way
	codeJoinFailed    = "join_failed"
	codeNodeDown      = "node_down"
	codeStatusUnknown = "status_unknown" // the node is up but did not say how it stands: ask again
	codeNoGrant       = "no_grant"       // this node holds no address in the door's organisation
	codeTimedOut      = "timed_out"      // the door did not answer in time
	codeDropped       = "dropped"        // the door hung up before HTTP: its lobby did not name this phone
	codeRefused       = "refused"        // the door answered HTTP and refused
	codeUnreachable   = "unreachable"    // any other dial failure
	codeNoDoor        = "no_door"        // no open door has that id
	codeSendFailed    = "send_failed"
)

func answer(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf(`{"error":%q,"code":"internal"}`, err.Error())
	}
	return string(b)
}

func failure(code string, err error) string {
	return answer(map[string]string{"error": err.Error(), "code": code})
}

// logDepth is how many lines wait for a Logger still busy with an earlier
// one. Past it a line is dropped and counted, and the Logger is told how
// many before the next line it hears.
const logDepth = 4096

// logs carries every line to its Logger.
var logs = newLogQueue(logDepth)

// logger stamps each line, so the app's log reads in order across goroutines.
func logger(log Logger) func(string, ...any) { return logs.logger(log) }

// logQueue hands lines to their Loggers from one goroutine of its own, in
// the order they were logged. A line is logged wherever phonenode happens to
// be — inside Down, under a lock, in tsnet's engine — and the app's Logger
// hops to the main thread; called in place, a Log from a Down the main thread
// called would wait on the main thread, and every caller would wait on the
// app. Logging only queues the line, and never waits.
type logQueue struct {
	lines chan logLine

	mu      sync.Mutex
	dropped int // lines lost since the last one queued
}

type logLine struct {
	to      Logger
	at      time.Time
	text    string
	dropped int // lines lost just before this one
}

func newLogQueue(depth int) *logQueue {
	q := &logQueue{lines: make(chan logLine, depth)}
	go q.drain()
	return q
}

func (q *logQueue) logger(log Logger) func(string, ...any) {
	if log == nil {
		return func(string, ...any) {}
	}
	return func(format string, args ...any) {
		q.put(logLine{to: log, at: time.Now(), text: fmt.Sprintf(format, args...)})
	}
}

func (q *logQueue) put(line logLine) {
	q.mu.Lock()
	defer q.mu.Unlock()
	line.dropped = q.dropped
	select {
	case q.lines <- line:
		q.dropped = 0
	default:
		q.dropped++
	}
}

func (q *logQueue) drain() {
	for line := range q.lines {
		stamp := line.at.UTC().Format("15:04:05.000 ")
		if line.dropped > 0 {
			line.to.Log(stamp + fmt.Sprintf("phonenode: %d log lines dropped", line.dropped))
		}
		line.to.Log(stamp + line.text)
	}
}
