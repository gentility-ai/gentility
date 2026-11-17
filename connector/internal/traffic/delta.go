// Package traffic samples this node's per-peer byte counters once a second
// and reports them to the mothership in batches (6net traffic design).
//
// Monitoring, not billing: up to a couple of minutes may be lost when the
// mothership is away longer than the ring holds, and a gap is reported as a
// gap, never as zero.
package traffic

import (
	"time"

	"github.com/gentility-ai/gentility/connector/internal/tsnode"
)

// forgetAfter is how long a peer that stopped appearing is remembered, so a
// peer re-added within it counts its fresh counter as bytes rather than as a
// new baseline.
const forgetAfter = 10 * time.Minute

// MaxBytesPerSecond is the mothership's ceiling on one peer's bytes in one
// second, either way: 2^36, about 550 Gbit/s, past any real link. A batch
// carrying more is refused whole, so a delta past it is dropped here.
const MaxBytesPerSecond = 1 << 36

// Delta is one peer's bytes in one second.
type Delta struct {
	Address string
	Rx, Tx  int64
}

type reading struct {
	rx, tx int64
	seen   time.Time
}

// Deltas turns cumulative counters into per-second deltas.
type Deltas struct {
	last map[string]reading
}

func NewDeltas() *Deltas { return &Deltas{last: map[string]reading{}} }

// Next reads one sample. A peer seen for the first time is a baseline and
// emits nothing: its counter holds bytes from before this process looked. A
// counter that went down — the peer re-added, the engine restarted — counts
// its new value. A peer that moved no bytes this second emits nothing. A
// peer unseen for longer than forgetAfter is forgotten first, so its return is
// a baseline and not its whole absence counted as one second. A second past
// MaxBytesPerSecond either way is a counter's artefact, not traffic: it emits
// nothing, and its reading is still the next second's baseline.
func (d *Deltas) Next(now time.Time, counters []tsnode.PeerCounter) []Delta {
	for address, r := range d.last {
		if now.Sub(r.seen) > forgetAfter {
			delete(d.last, address)
		}
	}
	deltas := make([]Delta, 0, len(counters))
	for _, c := range counters {
		prev, known := d.last[c.Address]
		d.last[c.Address] = reading{rx: c.Rx, tx: c.Tx, seen: now}
		if !known {
			continue
		}
		delta := Delta{Address: c.Address, Rx: step(prev.rx, c.Rx), Tx: step(prev.tx, c.Tx)}
		if delta.Rx == 0 && delta.Tx == 0 || delta.Rx > MaxBytesPerSecond || delta.Tx > MaxBytesPerSecond {
			continue
		}
		deltas = append(deltas, delta)
	}
	return deltas
}

func step(prev, cur int64) int64 {
	if cur < prev {
		return cur
	}
	return cur - prev
}
