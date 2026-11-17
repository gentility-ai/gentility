package traffic

import (
	"cmp"
	"maps"
	"slices"
)

const (
	// MaxSeconds is how long the ring holds a second the mothership has not
	// taken. The mothership's own dedupe window is fifteen minutes, so a
	// resend of anything still here counts once.
	MaxSeconds = 120
	// MaxPeersPerSecond is the peer_paths cap: past it, the busiest peers are
	// kept.
	MaxPeersPerSecond = 256
)

// Peer is one peer's seconds on the wire: Rx[i] and Tx[i] are the bytes in
// second From+i, zero for a second inside the span that moved nothing.
type Peer struct {
	Peer          string  `json:"peer"`
	From          int64   `json:"from"`
	Rx            []int64 `json:"rx"`
	Tx            []int64 `json:"tx"`
	LastHandshake *int64  `json:"last_handshake"`
}

// Batch is one POST's body (traffic_report_version 1). Covered is the
// inclusive runs of seconds it carries — every second the sampler read, with
// bytes or idle — so the mothership can tell an idle link (0) from a silent
// one (a gap). A dropped second is never covered.
type Batch struct {
	TrafficReportVersion int        `json:"traffic_report_version"`
	Peers                []Peer     `json:"peers"`
	Covered              [][2]int64 `json:"covered"`
	DroppedFrom          *int64     `json:"dropped_from"`
	DroppedTo            *int64     `json:"dropped_to"`

	// through is the newest second this batch carries, 0 when it carries none.
	through int64
	// ring is the ring it was taken from: only that ring's Ack forgets it.
	ring *Ring
}

type cell struct{ rx, tx int64 }

// Ring holds every sampled second — its deltas, or none for an idle one —
// until the mothership has it. Not safe for concurrent use: the reporter's
// loop is its only user.
type Ring struct {
	seconds     map[int64]map[string]cell
	handshakes  map[string]int64
	dropped     bool
	droppedFrom int64
	droppedTo   int64
}

func NewRing() *Ring {
	return &Ring{seconds: map[int64]map[string]cell{}, handshakes: map[string]int64{}}
}

// Seconds is how many sampled seconds the ring holds, idle ones included.
func (r *Ring) Seconds() int { return len(r.seconds) }

// Add files a sampled second — its deltas, possibly none — and the peers'
// latest handshakes, then drops any second MaxSeconds or more older than this
// one. A dropped second that carried bytes is recorded as a gap. Seconds are
// the caller's to keep distinct and ascending: a second filed twice keeps the
// later.
func (r *Ring) Add(second int64, deltas []Delta, handshakes map[string]int64) {
	if len(deltas) > MaxPeersPerSecond {
		deltas = slices.Clone(deltas)
		slices.SortFunc(deltas, func(a, b Delta) int { return cmp.Compare(b.Rx+b.Tx, a.Rx+a.Tx) })
		deltas = deltas[:MaxPeersPerSecond]
	}
	row := make(map[string]cell, len(deltas))
	for _, d := range deltas {
		row[d.Address] = cell{rx: d.Rx, tx: d.Tx}
	}
	r.seconds[second] = row
	for address, at := range handshakes {
		r.handshakes[address] = at
	}
	for s := range r.seconds {
		if s <= second-MaxSeconds {
			r.drop(s)
		}
	}
}

func (r *Ring) drop(second int64) {
	row := r.seconds[second]
	delete(r.seconds, second)
	if len(row) == 0 {
		// An idle second lost costs only its coverage: it reads as a gap.
		return
	}
	switch {
	case !r.dropped:
		r.dropped, r.droppedFrom, r.droppedTo = true, second, second
	case second < r.droppedFrom:
		r.droppedFrom = second
	case second > r.droppedTo:
		r.droppedTo = second
	}
}

// Batch is the oldest limit sampled seconds: their bytes grouped by peer,
// the runs of seconds they cover, and any gap.
func (r *Ring) Batch(limit int) Batch {
	b := Batch{TrafficReportVersion: 1, Peers: []Peer{}, Covered: [][2]int64{}, ring: r}
	if r.dropped {
		from, to := r.droppedFrom, r.droppedTo
		b.DroppedFrom, b.DroppedTo = &from, &to
	}

	seconds := slices.Sorted(maps.Keys(r.seconds))
	if len(seconds) > limit {
		seconds = seconds[:limit]
	}
	if len(seconds) == 0 {
		return b
	}
	b.through = seconds[len(seconds)-1]
	b.Covered = runs(seconds)

	spans := map[string][2]int64{}
	for _, s := range seconds {
		for address := range r.seconds[s] {
			span, seen := spans[address]
			if !seen {
				span[0] = s
			}
			span[1] = s
			spans[address] = span
		}
	}
	for _, address := range slices.Sorted(maps.Keys(spans)) {
		first, last := spans[address][0], spans[address][1]
		peer := Peer{Peer: address, From: first, Rx: make([]int64, last-first+1), Tx: make([]int64, last-first+1)}
		for s := first; s <= last; s++ {
			if c, ok := r.seconds[s][address]; ok {
				peer.Rx[s-first], peer.Tx[s-first] = c.rx, c.tx
			}
		}
		if at, ok := r.handshakes[address]; ok {
			peer.LastHandshake = &at
		}
		b.Peers = append(b.Peers, peer)
	}
	return b
}

// runs folds sorted seconds into inclusive [first, last] runs.
func runs(seconds []int64) [][2]int64 {
	out := [][2]int64{}
	for _, s := range seconds {
		if n := len(out); n > 0 && out[n-1][1] == s-1 {
			out[n-1][1] = s
			continue
		}
		out = append(out, [2]int64{s, s})
	}
	return out
}

// Ack forgets what b carried, once the mothership has it or never will take
// it: its seconds, and the gap up to the later of its newest second and the
// end of the gap it reported. A second evicted while b was in flight was
// carried by b, so it is no gap; a gap that grew past b keeps its newer part.
// A batch from another ring — one replaced while b was in flight — forgets
// nothing here.
func (r *Ring) Ack(b Batch) {
	if b.ring != r {
		return
	}
	upTo := b.through
	if b.DroppedTo != nil {
		upTo = max(upTo, *b.DroppedTo)
	}
	for s := range r.seconds {
		if s <= upTo {
			delete(r.seconds, s)
		}
	}
	if r.dropped && r.droppedFrom <= upTo {
		if r.droppedTo <= upTo {
			r.dropped = false
		} else {
			r.droppedFrom = upTo + 1
		}
	}
}
