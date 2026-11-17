package traffic

import (
	"testing"
	"time"

	"github.com/gentility-ai/gentility/connector/internal/tsnode"
)

var t0 = time.Unix(1_793_433_600, 0)

const (
	mac   = "fd06:6e7:0:2::4"
	cedar = "fd06:6e7:0:2::5"
)

func counter(address string, rx, tx int64) tsnode.PeerCounter {
	return tsnode.PeerCounter{Address: address, Rx: rx, Tx: tx}
}

func TestAFirstReadingIsABaseline(t *testing.T) {
	d := NewDeltas()
	if got := d.Next(t0, []tsnode.PeerCounter{counter(mac, 500, 900)}); len(got) != 0 {
		t.Fatalf("a first reading emitted %v", got)
	}
	got := d.Next(t0.Add(time.Second), []tsnode.PeerCounter{counter(mac, 700, 950)})
	if len(got) != 1 || got[0] != (Delta{Address: mac, Rx: 200, Tx: 50}) {
		t.Fatalf("delta %v", got)
	}
}

func TestACounterThatWentDownCountsItsNewValue(t *testing.T) {
	d := NewDeltas()
	d.Next(t0, []tsnode.PeerCounter{counter(mac, 5000, 5000)})
	got := d.Next(t0.Add(time.Second), []tsnode.PeerCounter{counter(mac, 40, 0)})
	if len(got) != 1 || got[0] != (Delta{Address: mac, Rx: 40, Tx: 0}) {
		t.Fatalf("delta %v", got)
	}
}

func TestAnIdlePeerEmitsNothing(t *testing.T) {
	d := NewDeltas()
	d.Next(t0, []tsnode.PeerCounter{counter(mac, 10, 10)})
	if got := d.Next(t0.Add(time.Second), []tsnode.PeerCounter{counter(mac, 10, 10)}); len(got) != 0 {
		t.Fatalf("an idle second emitted %v", got)
	}
}

func TestAPeerThatLeftAndCameBackIsStillKnown(t *testing.T) {
	d := NewDeltas()
	d.Next(t0, []tsnode.PeerCounter{counter(mac, 5000, 5000)})
	d.Next(t0.Add(time.Second), nil)
	got := d.Next(t0.Add(2*time.Second), []tsnode.PeerCounter{counter(mac, 40, 4)})
	if len(got) != 1 || got[0] != (Delta{Address: mac, Rx: 40, Tx: 4}) {
		t.Fatalf("a re-added peer's bytes were lost: %v", got)
	}
}

func TestAPeerForgottenIsABaselineAgain(t *testing.T) {
	d := NewDeltas()
	d.Next(t0, []tsnode.PeerCounter{counter(mac, 100, 100)})
	later := t0.Add(forgetAfter + time.Minute)
	if got := d.Next(later, []tsnode.PeerCounter{counter(mac, 5_000_000, 5_000_000)}); len(got) != 0 {
		t.Fatalf("a long-unseen peer's whole absence read as one second: %v", got)
	}
	got := d.Next(later.Add(time.Second), []tsnode.PeerCounter{counter(mac, 5_000_100, 5_000_000)})
	if len(got) != 1 || got[0] != (Delta{Address: mac, Rx: 100}) {
		t.Fatalf("delta %v", got)
	}
}

func TestASecondPastTheCeilingIsDroppedNotSent(t *testing.T) {
	d := NewDeltas()
	d.Next(t0, []tsnode.PeerCounter{counter(mac, MaxBytesPerSecond+5000, 0), counter(cedar, 0, 0)})
	// A counter that went down counts its new value, and a peer's delta can
	// span a long pause: either, past the mothership's ceiling, would have the
	// whole batch refused.
	got := d.Next(t0.Add(time.Second), []tsnode.PeerCounter{
		counter(mac, MaxBytesPerSecond+1, 0),
		counter(cedar, 10, MaxBytesPerSecond+1),
	})
	if len(got) != 0 {
		t.Fatalf("a second past the ceiling was sent: %v", got)
	}
	// The reading is still the baseline: the next second counts from it.
	got = d.Next(t0.Add(2*time.Second), []tsnode.PeerCounter{
		counter(mac, MaxBytesPerSecond+1+70, 0),
		counter(cedar, 10, MaxBytesPerSecond+1+7),
	})
	want := []Delta{{Address: mac, Rx: 70}, {Address: cedar, Tx: 7}}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("delta %v, want %v", got, want)
	}
}

func TestASecondAtTheCeilingIsSent(t *testing.T) {
	d := NewDeltas()
	d.Next(t0, []tsnode.PeerCounter{counter(mac, 0, 0)})
	got := d.Next(t0.Add(time.Second), []tsnode.PeerCounter{counter(mac, MaxBytesPerSecond, 0)})
	if len(got) != 1 || got[0] != (Delta{Address: mac, Rx: MaxBytesPerSecond}) {
		t.Fatalf("delta %v", got)
	}
}
