// Package refusal is what a 6net node saw refused, and how it says so.
//
// Two places see a refusal, both in internal/tsnode, the one package that
// knows tailscale. A destination's packet filter drops an
// inbound SYN no rule admits (an ipnext filter hook); a sender's engine drops
// an outbound SYN no peer owns, because the sender holds no grant at all
// (genscale's OnNoPeer engine hook). Both hand an Attempt over with Offer,
// without waiting — the packet path never blocks — and the Reporter folds
// them into edges and posts them to the mothership once a minute
// (POST /overlay/self/refusals; the refusal report endpoint on the other
// end). A third, a peer's TSMP "rejected" notice, is this node's own SYN
// refused somewhere else: it is logged here and reported by that peer.
package refusal

import (
	"cmp"
	"math"
	"net/netip"
	"slices"
	"time"
)

// Side is which end saw the refusal.
type Side string

const (
	// Destination: this node's filter dropped an inbound SYN.
	Destination Side = "destination"
	// Source: this node had no peer for an outbound SYN.
	Source Side = "source"
	// Rejected: a peer's filter dropped this node's SYN and said so. Logged,
	// never reported: the peer reports it.
	Rejected Side = "rejected"
)

// MaxEdges is the most edges one report carries, and the mothership's limit.
const MaxEdges = 256

// maxCount is the most a count or the overflow may carry: the mothership
// refuses a whole batch with a count past it, so counts saturate here.
const maxCount = math.MaxInt32

// Attempt is one refused SYN, as a hook saw it.
type Attempt struct {
	Side     Side
	Src, Dst netip.Addr
	Port     uint16
}

// Edge is what refusals are counted by.
type Edge struct {
	Side        Side       `json:"side"`
	Source      netip.Addr `json:"source"`
	Destination netip.Addr `json:"destination"`
	Protocol    string     `json:"protocol"`
	Port        uint16     `json:"port"`
}

// Seen is one edge's attempts since the last report.
type Seen struct {
	Edge
	Count       int       `json:"count"`
	FirstSeenAt time.Time `json:"first_seen_at"`
	LastSeenAt  time.Time `json:"last_seen_at"`
}

// Batch is one report's body.
type Batch struct {
	Edges    []Seen `json:"edges"`
	Overflow int    `json:"overflow"`
}

// pending is the edges not yet reported. Owned by the Reporter's goroutine
// alone, so it has no lock.
type pending struct {
	edges    map[Edge]*Seen
	overflow int
}

func newPending() *pending { return &pending{edges: map[Edge]*Seen{}} }

func (p *pending) empty() bool { return len(p.edges) == 0 && p.overflow == 0 }

// add counts one attempt at `at`, stamped to the second in UTC so a full
// batch stays small on the wire. A new edge past MaxEdges is overflow.
func (p *pending) add(a Attempt, at time.Time) {
	at = at.UTC().Truncate(time.Second)
	e := Edge{Side: a.Side, Source: a.Src, Destination: a.Dst, Protocol: "tcp", Port: a.Port}
	p.seen(Seen{Edge: e, Count: 1, FirstSeenAt: at, LastSeenAt: at})
}

// merge puts a batch that could not be sent back, under what came since.
func (p *pending) merge(b Batch) {
	for _, s := range b.Edges {
		p.seen(s)
	}
	p.overflow = plus(p.overflow, b.Overflow)
}

func (p *pending) seen(s Seen) {
	if held, ok := p.edges[s.Edge]; ok {
		held.Count = plus(held.Count, s.Count)
		if s.FirstSeenAt.Before(held.FirstSeenAt) {
			held.FirstSeenAt = s.FirstSeenAt
		}
		if s.LastSeenAt.After(held.LastSeenAt) {
			held.LastSeenAt = s.LastSeenAt
		}
		return
	}
	if len(p.edges) >= MaxEdges {
		p.overflow = plus(p.overflow, s.Count)
		return
	}
	copied := s
	copied.Count = plus(0, s.Count)
	p.edges[s.Edge] = &copied
}

// take empties the buffer into a batch, oldest edge first.
func (p *pending) take() Batch {
	b := Batch{Edges: make([]Seen, 0, len(p.edges)), Overflow: p.overflow}
	for _, s := range p.edges {
		b.Edges = append(b.Edges, *s)
	}
	slices.SortFunc(b.Edges, func(x, y Seen) int {
		return cmp.Or(
			x.FirstSeenAt.Compare(y.FirstSeenAt),
			cmp.Compare(x.Port, y.Port),
			cmp.Compare(x.Side, y.Side),
			x.Source.Compare(y.Source),
			x.Destination.Compare(y.Destination),
		)
	})
	p.edges, p.overflow = map[Edge]*Seen{}, 0
	return b
}

// plus adds two counts, saturating at maxCount.
func plus(a, b int) int { return min(a+b, maxCount) }
