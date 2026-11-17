package phonenode

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/gentility-ai/gentility/connector/internal/tsnode"
)

func TestPeerPathRefusesWhatIsNotA6netAddress(t *testing.T) {
	for _, address := range []string{"", "not-an-address", "192.168.1.5", "fe80::1%en0"} {
		if got := PeerPath(address); !strings.Contains(got, `"malformed"`) {
			t.Fatalf("PeerPath(%q) = %s, want malformed", address, got)
		}
	}
}

func TestPeerPathSaysNodeDownAndJoining(t *testing.T) {
	mu.Lock()
	current, joining = nil, nil
	mu.Unlock()
	if got := PeerPath("fd06:6e7:0:2::4"); !strings.Contains(got, `"node_down"`) {
		t.Fatalf("with no node: %s", got)
	}
	_, cancel := context.WithCancel(context.Background())
	mu.Lock()
	joining = cancel
	mu.Unlock()
	t.Cleanup(func() {
		mu.Lock()
		joining = nil
		mu.Unlock()
		cancel()
	})
	if got := PeerPath("fd06:6e7:0:2::4"); !strings.Contains(got, `"joining"`) {
		t.Fatalf("while joining: %s", got)
	}
}

func TestPeerPathAnswersThePeersPathViaAndRelay(t *testing.T) {
	s := fakeSession(t, func(context.Context) (tsnode.Report, error) {
		return tsnode.Report{PeerPaths: []tsnode.PeerPath{
			{Address: "fd06:6e7:0:2::4", Path: "direct", Via: "[fd8c:1::5]:41641"},
			{Address: "fd06:6e7:0:2::5", Path: "derp", Relay: "syd"},
			{Address: "fd06:6e7:0:2::6", Path: "peer_relay"},
		}}, nil
	})
	makeCurrent(s)
	t.Cleanup(func() { makeCurrent(nil) })

	cases := []struct{ address, want, absent string }{
		{"fd06:6e7:0:2::4", `"path":"direct"`, `"relay"`},
		{"fd06:6e7:0:2::4", `"via":"[fd8c:1::5]:41641"`, ""},
		{"fd06:6e7:0:2::5", `"relay":"syd"`, `"via"`},
		{"fd06:6e7:0:2::6", `"path":"peer_relay"`, `"via"`},
		{"fd06:6e7:0:2::7", `"path":"unknown"`, `"via"`},
	}
	for _, tc := range cases {
		got := PeerPath(tc.address)
		if !strings.Contains(got, tc.want) {
			t.Errorf("PeerPath(%s) = %s, want %s", tc.address, got, tc.want)
		}
		if tc.absent != "" && strings.Contains(got, tc.absent) {
			t.Errorf("PeerPath(%s) = %s, must not carry %s", tc.address, got, tc.absent)
		}
	}
}

func TestPeerPathSaysWhenTheNodeCannotReport(t *testing.T) {
	s := fakeSession(t, func(context.Context) (tsnode.Report, error) {
		return tsnode.Report{}, errors.New("backend busy")
	})
	makeCurrent(s)
	t.Cleanup(func() { makeCurrent(nil) })
	if got := PeerPath("fd06:6e7:0:2::4"); !strings.Contains(got, `"status_unknown"`) {
		t.Fatalf("got %s, want status_unknown", got)
	}
}
