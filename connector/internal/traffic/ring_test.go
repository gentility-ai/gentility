package traffic

import (
	"fmt"
	"reflect"
	"testing"
)

func ptr(v int64) *int64 { return &v }

func TestTheRingKeepsOneHundredTwentySecondsAndReportsTheGap(t *testing.T) {
	r := NewRing()
	start := t0.Unix()
	for s := int64(0); s < 125; s++ {
		r.Add(start+s, []Delta{{Address: mac, Rx: 1, Tx: 2}}, nil)
	}
	if r.Seconds() != MaxSeconds {
		t.Fatalf("holds %d seconds", r.Seconds())
	}
	b := r.Batch(MaxSeconds)
	if b.DroppedFrom == nil || *b.DroppedFrom != start || *b.DroppedTo != start+4 {
		t.Fatalf("gap %v..%v", b.DroppedFrom, b.DroppedTo)
	}
	if b.Peers[0].From != start+5 || len(b.Peers[0].Rx) != MaxSeconds {
		t.Fatalf("peer %+v", b.Peers[0])
	}
	if !reflect.DeepEqual(b.Covered, [][2]int64{{start + 5, start + 124}}) {
		t.Fatalf("covered %v", b.Covered)
	}
}

func TestAnIdleSecondIsCoveredAndLosingOneIsNoGap(t *testing.T) {
	r := NewRing()
	start := t0.Unix()
	for s := int64(0); s < 125; s++ {
		r.Add(start+s, nil, nil)
	}
	b := r.Batch(MaxSeconds)
	if len(b.Peers) != 0 || b.DroppedFrom != nil {
		t.Fatalf("idle seconds made peers or a gap: %+v", b)
	}
	if r.Seconds() != MaxSeconds || !reflect.DeepEqual(b.Covered, [][2]int64{{start + 5, start + 124}}) {
		t.Fatalf("%d seconds, covered %v", r.Seconds(), b.Covered)
	}
}

func TestTheRingKeepsTheBusiestPeersInASecond(t *testing.T) {
	deltas := make([]Delta, 0, MaxPeersPerSecond+10)
	for i := 0; i < MaxPeersPerSecond+10; i++ {
		deltas = append(deltas, Delta{Address: fmt.Sprintf("fd06:6e7:0:2::%x", i+1), Rx: int64(i)})
	}
	r := NewRing()
	r.Add(t0.Unix(), deltas, nil)
	b := r.Batch(1)
	if len(b.Peers) != MaxPeersPerSecond {
		t.Fatalf("kept %d peers", len(b.Peers))
	}
	for _, p := range b.Peers {
		if p.Rx[0] < 10 {
			t.Fatalf("kept a quiet peer %s over a busier one", p.Peer)
		}
	}
}

func TestABatchGroupsSecondsByPeerWithZerosBetween(t *testing.T) {
	r := NewRing()
	s := t0.Unix()
	r.Add(s, []Delta{{Address: mac, Rx: 1, Tx: 10}}, map[string]int64{mac: s - 10})
	r.Add(s+2, []Delta{{Address: mac, Rx: 3, Tx: 30}, {Address: cedar, Rx: 7, Tx: 70}}, nil)

	b := r.Batch(MaxSeconds)
	want := []Peer{
		{Peer: mac, From: s, Rx: []int64{1, 0, 3}, Tx: []int64{10, 0, 30}, LastHandshake: ptr(s - 10)},
		{Peer: cedar, From: s + 2, Rx: []int64{7}, Tx: []int64{70}},
	}
	if b.TrafficReportVersion != 1 || b.DroppedFrom != nil || !reflect.DeepEqual(b.Peers, want) {
		t.Fatalf("batch %+v", b)
	}
	// s+1 was never sampled, so it is not covered.
	if !reflect.DeepEqual(b.Covered, [][2]int64{{s, s}, {s + 2, s + 2}}) {
		t.Fatalf("covered %v", b.Covered)
	}
}

func TestAckForgetsWhatTheBatchCarried(t *testing.T) {
	r := NewRing()
	s := t0.Unix()
	for i := int64(0); i < 125; i++ {
		r.Add(s+i, []Delta{{Address: mac, Rx: 1}}, nil)
	}
	r.Ack(r.Batch(10))

	next := r.Batch(MaxSeconds)
	if next.DroppedFrom != nil {
		t.Fatalf("the gap was reported twice: %v..%v", *next.DroppedFrom, *next.DroppedTo)
	}
	if next.Peers[0].From != s+15 || r.Seconds() != 110 {
		t.Fatalf("from %d, %d seconds left", next.Peers[0].From, r.Seconds())
	}
}

func TestAnEmptyRingIsAnEmptyBatch(t *testing.T) {
	b := NewRing().Batch(MaxSeconds)
	if b.TrafficReportVersion != 1 || b.Peers == nil || len(b.Peers) != 0 || b.Covered == nil || len(b.Covered) != 0 {
		t.Fatalf("batch %+v", b)
	}
}

// Seconds the ring evicts while a batch carrying them is in flight were
// delivered by that batch: once it is acknowledged, they are no gap.
func TestSecondsEvictedInFlightAreNotReportedDropped(t *testing.T) {
	r := NewRing()
	s := t0.Unix()
	for i := int64(0); i < MaxSeconds; i++ {
		r.Add(s+i, []Delta{{Address: mac, Rx: 1}}, nil)
	}
	inFlight := r.Batch(10)
	for i := int64(MaxSeconds); i < MaxSeconds+15; i++ {
		r.Add(s+i, []Delta{{Address: mac, Rx: 1}}, nil)
	}
	r.Ack(inFlight)

	next := r.Batch(MaxSeconds)
	if next.DroppedFrom == nil || *next.DroppedFrom != s+10 || *next.DroppedTo != s+14 {
		t.Fatalf("gap %v..%v, want only the five seconds never sent", next.DroppedFrom, next.DroppedTo)
	}

	r = NewRing()
	for i := int64(0); i < MaxSeconds; i++ {
		r.Add(s+i, []Delta{{Address: mac, Rx: 1}}, nil)
	}
	whole := r.Batch(MaxSeconds)
	for i := int64(MaxSeconds); i < MaxSeconds+5; i++ {
		r.Add(s+i, []Delta{{Address: mac, Rx: 1}}, nil)
	}
	r.Ack(whole)
	if b := r.Batch(MaxSeconds); b.DroppedFrom != nil {
		t.Fatalf("delivered seconds reported dropped: %v..%v", *b.DroppedFrom, *b.DroppedTo)
	}
}

// A batch from a ring since replaced (the clock stepped back while it was in
// flight) has nothing in this ring to forget.
func TestAnAckFromAnotherRingForgetsNothing(t *testing.T) {
	old := NewRing()
	old.Add(t0.Unix()+100, []Delta{{Address: mac, Rx: 1}}, nil)
	inFlight := old.Batch(MaxSeconds)

	r := NewRing()
	r.Add(t0.Unix(), []Delta{{Address: mac, Rx: 1}}, nil)
	r.Ack(inFlight)
	if r.Seconds() != 1 {
		t.Fatalf("another ring's ack forgot %d second(s)", 1-r.Seconds())
	}
}
