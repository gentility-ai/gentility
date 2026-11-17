package refusal

import "sync/atomic"

// queueSize is how many attempts may wait for the Reporter. A flood past it
// is counted, not queued: the packet path never waits.
const queueSize = 1024

// attempts is process-wide because the hooks are: an ipnext extension is
// registered for every backend in the process, and a daemon runs one.
var (
	attempts = make(chan Attempt, queueSize)
	lost     atomic.Int64
)

// Offer is how a hook (internal/tsnode, on the packet path) hands an attempt
// over. It never waits: past a full queue the attempt is counted as lost.
func Offer(a Attempt) {
	select {
	case attempts <- a:
	default:
		lost.Add(1)
	}
}

// Attempts is what the hooks hand over, for the Reporter.
func Attempts() <-chan Attempt { return attempts }

// Lost is how many attempts were dropped because the queue was full since
// the last call.
func Lost() int { return int(lost.Swap(0)) }
