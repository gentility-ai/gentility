package config

import (
	"math/rand/v2"
	"time"
)

const (
	// baseDelay is the first wait after a retryable failure, and the unit the
	// doubling counts in.
	baseDelay = time.Second

	// maxDelay is the ceiling. A minute is the same order as the long poll's
	// own window, so a node that has lost the mothership asks about as often
	// as one that has it.
	maxDelay = 60 * time.Second
)

// backoff is the wait between retries: 1s doubling to 60s, jittered into
// [d/2, d].
//
// The jitter is not decoration. A mothership that restarts has every node that
// was mid-poll come back at once, and an unjittered backoff makes each of those
// herds arrive on the same second forever. Half the window is enough to spread
// them and keeps the delay recognisable in a log — a node that waited 47
// seconds is plainly in its sixty-second band.
type backoff struct {
	attempt int

	// base is the first band's width. Zero means baseDelay; a test that is
	// proving the loop iterates sets it small rather than spending a second
	// per iteration doing so.
	base time.Duration

	// jitter is the seam the tests drive: 0 for the floor of each band, 1 for
	// its ceiling. Nil means rand.Float64.
	jitter func() float64
}

// next returns the delay to wait now, and moves to the next band.
func (b *backoff) next() time.Duration {
	unit := b.base
	if unit <= 0 {
		unit = baseDelay
	}

	delay := unit << b.attempt
	if delay > maxDelay || delay <= 0 {
		delay = maxDelay
	} else {
		b.attempt++
	}

	roll := rand.Float64
	if b.jitter != nil {
		roll = b.jitter
	}

	half := delay / 2
	return half + time.Duration(roll()*float64(half))
}

// reset puts the next delay back to the base. A pull that got an answer — a
// document, a 304 or a 409 — is the mothership being reachable, which is the
// only thing this was counting.
func (b *backoff) reset() { b.attempt = 0 }
