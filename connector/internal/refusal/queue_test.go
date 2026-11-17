package refusal

import "testing"

// drain empties the shared queue so each test starts clean.
func drain() {
	for {
		select {
		case <-attempts:
		default:
			Lost()
			return
		}
	}
}

func TestAFullQueueDropsAndCountsInsteadOfWaiting(t *testing.T) {
	drain()
	for i := 0; i < queueSize+5; i++ {
		Offer(Attempt{Side: Destination, Src: mac, Dst: cedar, Port: 8080})
	}
	if lost := Lost(); lost != 5 {
		t.Fatalf("lost %d; want 5", lost)
	}
	drain()
}
