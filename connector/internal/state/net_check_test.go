package state

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gentility-ai/gentility/connector/internal/tsnode"
)

func TestNetworkCheckEndpointsAreOwnerOnly(t *testing.T) {
	s := &Store{Dir: t.TempDir()}
	check := NetCheck{WrittenAt: time.Now(), Peers: []tsnode.PeerDiagnostic{{PeerPath: tsnode.PeerPath{Address: "fd06:6e7:0:1::b", Path: "direct", Via: "192.0.2.4:5678"}}}}
	if err := s.SaveNetCheck(check); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(s.Dir, netCheckFile))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("private diagnostics mode = %o", info.Mode().Perm())
	}
	got, err := s.LoadNetCheck()
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Peers) != 1 || got.Peers[0].Via != check.Peers[0].Via {
		t.Fatalf("lost endpoints: %+v", got)
	}
	if err := s.ClearIdentity(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.LoadNetCheck(); !os.IsNotExist(err) {
		t.Fatalf("private endpoints survived leave: %v", err)
	}
}
