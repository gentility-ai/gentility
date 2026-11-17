package refusal

import (
	"encoding/json"
	"net/netip"
	"strings"
	"testing"
	"time"
)

var (
	mac   = netip.MustParseAddr("fd06:6e7:0:2::4")
	cedar = netip.MustParseAddr("fd06:6e7:0:2::5")
	t0    = time.Date(2026, 10, 11, 8, 0, 0, 0, time.UTC)
)

func TestAnEdgeIsCountedOnceWithItsFirstAndLastSighting(t *testing.T) {
	p := newPending()
	for i := 0; i < 6; i++ { // a SYN and its five retransmits
		p.add(Attempt{Side: Destination, Src: mac, Dst: cedar, Port: 8080}, t0.Add(time.Duration(i)*time.Second))
	}
	b := p.take()
	if len(b.Edges) != 1 {
		t.Fatalf("edges %v; want one", b.Edges)
	}
	e := b.Edges[0]
	if e.Count != 6 || !e.FirstSeenAt.Equal(t0) || !e.LastSeenAt.Equal(t0.Add(5*time.Second)) || e.Protocol != "tcp" {
		t.Fatalf("got %+v", e)
	}
	if !p.empty() {
		t.Fatal("take did not empty the buffer")
	}
}

func TestSidesAndPortsAreDistinctEdges(t *testing.T) {
	p := newPending()
	p.add(Attempt{Side: Destination, Src: mac, Dst: cedar, Port: 8080}, t0)
	p.add(Attempt{Side: Destination, Src: mac, Dst: cedar, Port: 22}, t0)
	p.add(Attempt{Side: Source, Src: mac, Dst: cedar, Port: 8080}, t0)
	if n := len(p.take().Edges); n != 3 {
		t.Fatalf("%d edges; want 3", n)
	}
}

func TestPast256EdgesTheRestAreCountedAsOverflow(t *testing.T) {
	p := newPending()
	for port := 1; port <= MaxEdges+10; port++ {
		p.add(Attempt{Side: Destination, Src: mac, Dst: cedar, Port: uint16(port)}, t0)
	}
	// A known edge still counts when the buffer is full.
	p.add(Attempt{Side: Destination, Src: mac, Dst: cedar, Port: 1}, t0)
	b := p.take()
	if len(b.Edges) != MaxEdges || b.Overflow != 10 {
		t.Fatalf("edges %d overflow %d", len(b.Edges), b.Overflow)
	}
	for _, s := range b.Edges {
		if s.Port == 1 && s.Count != 2 {
			t.Fatalf("port 1 counted %d; want 2", s.Count)
		}
	}
}

func TestAFailedBatchMergesIntoTheNext(t *testing.T) {
	p := newPending()
	p.add(Attempt{Side: Destination, Src: mac, Dst: cedar, Port: 8080}, t0)
	failed := p.take()
	failed.Overflow = 3
	p.add(Attempt{Side: Destination, Src: mac, Dst: cedar, Port: 8080}, t0.Add(time.Minute))
	p.merge(failed)
	b := p.take()
	if len(b.Edges) != 1 || b.Edges[0].Count != 2 || !b.Edges[0].FirstSeenAt.Equal(t0) ||
		!b.Edges[0].LastSeenAt.Equal(t0.Add(time.Minute)) || b.Overflow != 3 {
		t.Fatalf("got %+v", b)
	}
}

func TestAFullBatchIsSmallOnTheWire(t *testing.T) {
	p := newPending()
	for port := 1; port <= MaxEdges; port++ {
		p.add(Attempt{Side: Destination, Src: mac, Dst: cedar, Port: uint16(60000 + port%5000)}, t0.Add(123456789*time.Nanosecond))
	}
	body, err := json.Marshal(p.take())
	if err != nil {
		t.Fatal(err)
	}
	if len(body) >= 64*1024 {
		t.Fatalf("%d bytes; the mothership takes 64 KiB", len(body))
	}
	if want := `"source":"fd06:6e7:0:2::4"`; !strings.Contains(string(body), want) {
		t.Fatalf("addresses are not canonical: %s", body[:200])
	}
	if want := `"first_seen_at":"2026-10-11T08:00:00Z"`; !strings.Contains(string(body), want) {
		t.Fatalf("stamps are not whole seconds in UTC: %s", body[:200])
	}
}

// The longest address 6net allocates is a 16-bit org subnet and a 32-bit host
// index; with every count at its ceiling a full batch still fits.
func TestTheLongestFullBatchFits(t *testing.T) {
	src := netip.MustParseAddr("fd06:6e7:0:fffe::ffff:fffe")
	dst := netip.MustParseAddr("fd06:6e7:0:fffe::ffff:ffff")
	stamp := time.Date(2026, 10, 11, 8, 0, 0, 0, time.UTC)
	b := Batch{Overflow: maxCount}
	for i := 0; i < MaxEdges; i++ {
		e := Edge{Side: Destination, Source: src, Destination: dst, Protocol: "tcp", Port: 65535}
		b.Edges = append(b.Edges, Seen{Edge: e, Count: maxCount, FirstSeenAt: stamp, LastSeenAt: stamp})
	}
	body, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	if len(body) >= 64*1024 {
		t.Fatalf("%d bytes; the mothership takes 64 KiB", len(body))
	}
}

func TestCountsSaturateAtTheMothershipsCeiling(t *testing.T) {
	p := newPending()
	p.add(Attempt{Side: Destination, Src: mac, Dst: cedar, Port: 8080}, t0)
	flooded := Seen{Edge: Edge{Side: Destination, Source: mac, Destination: cedar, Protocol: "tcp", Port: 8080},
		Count: maxCount, FirstSeenAt: t0, LastSeenAt: t0}
	p.merge(Batch{Edges: []Seen{flooded}, Overflow: maxCount})
	p.merge(Batch{Overflow: 1})
	b := p.take()
	if b.Edges[0].Count != maxCount || b.Overflow != maxCount {
		t.Fatalf("count %d overflow %d; want both %d", b.Edges[0].Count, b.Overflow, maxCount)
	}
}

func TestTakeOrdersTiesBySideThenAddresses(t *testing.T) {
	p := newPending()
	p.add(Attempt{Side: Source, Src: cedar, Dst: mac, Port: 22}, t0)
	p.add(Attempt{Side: Destination, Src: cedar, Dst: mac, Port: 22}, t0)
	p.add(Attempt{Side: Destination, Src: mac, Dst: cedar, Port: 22}, t0)
	got := p.take().Edges
	if got[0].Side != Destination || got[0].Source != mac || got[1].Source != cedar || got[2].Side != Source {
		t.Fatalf("order %+v", got)
	}
}
