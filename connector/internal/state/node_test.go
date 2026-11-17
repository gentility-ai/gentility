package state

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func enrolled() Node {
	return Node{
		NodeID:         "0198f0a2-0000-7000-8000-000000000001",
		NodeCredential: "cred-shhh",
		RuntimeToken:   "rt-shhh",
		LastRevision:   NoRevision,
		GatewayAddr:    "fd06:6e7:0:1::1",
		GatewayPort:    443,
		HeadURL:        "https://head.example",
		Hostname:       "shipyard",
	}
}

// A server comes back from a reboot with the pre-auth key it joined on long
// expired, so what it joined with is not what it rejoins with: the head to
// point tsnet at and the name to come back under have to be on disk.
func TestSaveNodeRoundTripsWhatItTakesToRejoin(t *testing.T) {
	s := &Store{Dir: filepath.Join(t.TempDir(), "gentility")}

	if err := s.SaveNode(enrolled()); err != nil {
		t.Fatal(err)
	}
	got, err := s.LoadNode()
	if err != nil {
		t.Fatal(err)
	}
	if got.HeadURL != "https://head.example" || got.Hostname != "shipyard" {
		t.Fatalf("node came back as head %q hostname %q", got.HeadURL, got.Hostname)
	}

	written, err := os.ReadFile(filepath.Join(s.Dir, nodeFile))
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(written, &raw); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"head_url", "hostname"} {
		if _, ok := raw[key]; !ok {
			t.Fatalf("%s holds no %s: %v", nodeFile, key, raw)
		}
	}
}

// Neither is a secret, and a log line naming only the node id would leave an
// operator unable to see which head a node came back to.
func TestNodeStringShowsTheHeadAndTheHostname(t *testing.T) {
	rendered := enrolled().String()

	if !strings.Contains(rendered, "https://head.example") {
		t.Fatalf("rendered state does not name the head: %s", rendered)
	}
	if !strings.Contains(rendered, "shipyard") {
		t.Fatalf("rendered state does not name the hostname: %s", rendered)
	}
}

func TestSaveNodeRoundTrips(t *testing.T) {
	s := &Store{Dir: filepath.Join(t.TempDir(), "gentility")}
	want := enrolled()

	if err := s.SaveNode(want); err != nil {
		t.Fatalf("SaveNode: %v", err)
	}
	got, err := s.LoadNode()
	if err != nil {
		t.Fatalf("LoadNode: %v", err)
	}
	if got != want {
		t.Fatalf("LoadNode() = %+v, want %+v", got, want)
	}
}

func TestSaveNodeWritesOwnerOnlyFileInOwnerOnlyDir(t *testing.T) {
	s := &Store{Dir: filepath.Join(t.TempDir(), "gentility")}

	if err := s.SaveNode(enrolled()); err != nil {
		t.Fatalf("SaveNode: %v", err)
	}

	fi, err := os.Stat(filepath.Join(s.Dir, nodeFile))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if got := fi.Mode().Perm(); got != 0o600 {
		t.Fatalf("%s mode = %o, want 600", nodeFile, got)
	}

	di, err := os.Stat(s.Dir)
	if err != nil {
		t.Fatalf("stat dir: %v", err)
	}
	if got := di.Mode().Perm(); got != 0o711 {
		t.Fatalf("dir mode = %o, want 711", got)
	}
}

func TestLoadNodeMissingIsErrNotEnrolled(t *testing.T) {
	s := &Store{Dir: filepath.Join(t.TempDir(), "gentility")}

	if _, err := s.LoadNode(); !IsNotEnrolled(err) {
		t.Fatalf("want ErrNotEnrolled, got %v", err)
	}
}

// A file that parses but names no credential is a node that cannot present
// itself, which is the same answer as no file at all.
func TestLoadNodeWithoutCredentialIsErrNotEnrolled(t *testing.T) {
	s := &Store{Dir: filepath.Join(t.TempDir(), "gentility")}
	if err := s.SaveNode(Node{NodeID: "n", LastRevision: NoRevision}); err != nil {
		t.Fatal(err)
	}

	if _, err := s.LoadNode(); !IsNotEnrolled(err) {
		t.Fatalf("want ErrNotEnrolled, got %v", err)
	}
}

// The write is a temp file renamed over the target: a second save leaves the
// new value and nothing half-written beside it.
func TestSaveNodeLeavesNothingBesideTheFile(t *testing.T) {
	s := &Store{Dir: filepath.Join(t.TempDir(), "gentility")}

	if err := s.SaveNode(enrolled()); err != nil {
		t.Fatal(err)
	}
	second := enrolled()
	second.LastRevision = 7
	if err := s.SaveNode(second); err != nil {
		t.Fatal(err)
	}

	entries, err := os.ReadDir(s.Dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != nodeFile {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("state dir holds %v, want just %s", names, nodeFile)
	}

	got, err := s.LoadNode()
	if err != nil {
		t.Fatal(err)
	}
	if got.LastRevision != 7 {
		t.Fatalf("LastRevision = %d, want 7", got.LastRevision)
	}
}

func TestNodeStringRedactsBothSecrets(t *testing.T) {
	node := enrolled()

	for _, rendered := range []string{
		fmt.Sprintf("%v", node),
		fmt.Sprintf("%v", &node),
		fmt.Sprintf("%s", node),
		node.String(),
	} {
		if strings.Contains(rendered, node.NodeCredential) {
			t.Fatalf("rendered state leaks the node credential: %s", rendered)
		}
		if strings.Contains(rendered, node.RuntimeToken) {
			t.Fatalf("rendered state leaks the runtime token: %s", rendered)
		}
		if !strings.Contains(rendered, node.NodeID) {
			t.Fatalf("rendered state does not name the node: %s", rendered)
		}
	}
}

func TestConfigRevisionPersists(t *testing.T) {
	s := &Store{Dir: filepath.Join(t.TempDir(), "gentility")}
	node := enrolled()
	if err := s.SaveNode(node); err != nil {
		t.Fatal(err)
	}

	if err := s.AdvanceRevision(0); err != nil {
		t.Fatalf("AdvanceRevision: %v", err)
	}
	got, err := s.LoadNode()
	if err != nil {
		t.Fatal(err)
	}
	if got.LastRevision != 0 {
		t.Fatalf("LastRevision = %d, want 0 — revision 0 is a real document", got.LastRevision)
	}
	if got.NodeCredential != node.NodeCredential {
		t.Fatalf("AdvanceRevision lost the credential")
	}
}

// A file written before this field existed, or one a caller left out, is a
// node that has applied no document — never one that has applied revision 0.
func TestLoadNodeWithoutRevisionIsNoRevision(t *testing.T) {
	s := &Store{Dir: filepath.Join(t.TempDir(), "gentility")}
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	written, err := json.Marshal(map[string]string{"node_id": "n", "node_credential": "c"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.Dir, nodeFile), written, 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := s.LoadNode()
	if err != nil {
		t.Fatal(err)
	}
	if got.LastRevision != NoRevision {
		t.Fatalf("LastRevision = %d, want NoRevision (%d)", got.LastRevision, NoRevision)
	}
}

// The CA the gateway's certificate chains to arrives in a config document and
// is read back on the next start — a node that failed to write it down would
// come back reaching for the plaintext hop its first join was allowed.
func TestRecordGatewayPersistsTheCAAndKeepsTheSecrets(t *testing.T) {
	s := &Store{Dir: filepath.Join(t.TempDir(), "gentility")}
	node := enrolled()
	if err := s.SaveNode(node); err != nil {
		t.Fatal(err)
	}

	if err := s.RecordGateway("fd06:6e7:0:1::1", 443, "-----BEGIN CERTIFICATE-----"); err != nil {
		t.Fatalf("RecordGateway: %v", err)
	}

	got, err := s.LoadNode()
	if err != nil {
		t.Fatal(err)
	}
	if got.CAPEM != "-----BEGIN CERTIFICATE-----" {
		t.Fatalf("CAPEM = %q", got.CAPEM)
	}
	if got.GatewayAddr != "fd06:6e7:0:1::1" || got.GatewayPort != 443 {
		t.Fatalf("gateway = [%s]:%d", got.GatewayAddr, got.GatewayPort)
	}
	if got.NodeCredential != node.NodeCredential {
		t.Fatal("RecordGateway lost the credential")
	}
}

// A document arrives whenever anything in the organization changes; the
// gateway is the part of it that almost never does.
func TestRecordGatewayWritesNothingWhenNothingMoved(t *testing.T) {
	s := &Store{Dir: filepath.Join(t.TempDir(), "gentility")}
	if err := s.SaveNode(enrolled()); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordGateway("fd06:6e7:0:1::1", 443, ""); err != nil {
		t.Fatal(err)
	}

	before, err := os.Stat(filepath.Join(s.Dir, nodeFile))
	if err != nil {
		t.Fatal(err)
	}

	if err := s.RecordGateway("fd06:6e7:0:1::1", 443, ""); err != nil {
		t.Fatal(err)
	}

	after, err := os.Stat(filepath.Join(s.Dir, nodeFile))
	if err != nil {
		t.Fatal(err)
	}
	if !after.ModTime().Equal(before.ModTime()) {
		t.Fatal("the state file was rewritten for a gateway that had not moved")
	}
}

func TestAdvanceRevisionOnAnUnenrolledNodeIsErrNotEnrolled(t *testing.T) {
	s := &Store{Dir: filepath.Join(t.TempDir(), "gentility")}

	if err := s.AdvanceRevision(3); !IsNotEnrolled(err) {
		t.Fatalf("want ErrNotEnrolled, got %v", err)
	}
}

// An identity has two halves stored by two owners, and `install --replace`
// discards both or neither: node.json left behind is a credential the
// mothership has revoked, and the tsnet directory left behind is a node key the
// head answers by asking for an interactive login nobody will ever give.
func TestClearIdentityRemovesBothHalvesAndTheStatusSnapshot(t *testing.T) {
	s := &Store{Dir: filepath.Join(t.TempDir(), "gentility")}
	if err := s.SaveNode(enrolled()); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveStatus(Status{WrittenAt: time.Now(), Revision: 7}); err != nil {
		t.Fatal(err)
	}
	tsnet := filepath.Join(s.Dir, TSNetDir)
	if err := os.MkdirAll(tsnet, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tsnet, "tailscaled.state"), []byte(`{"_machinekey":"…"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	// Everything else in the directory is somebody else's: the bearer belongs
	// to the person at this terminal, and material staged a moment ago is what
	// the node is about to join on.
	if err := s.SaveAuth(Auth{BaseURL: "https://mcp.example", AccessToken: "bearer"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveEnrollment(Enrollment{EnrollmentID: "enr_01", PreauthKey: "tskey-auth-fresh"}); err != nil {
		t.Fatal(err)
	}

	if err := s.ClearIdentity(); err != nil {
		t.Fatalf("ClearIdentity: %v", err)
	}

	if _, err := s.LoadNode(); !IsNotEnrolled(err) {
		t.Fatalf("LoadNode after ClearIdentity = %v, want ErrNotEnrolled", err)
	}
	if _, err := os.Stat(tsnet); !os.IsNotExist(err) {
		t.Fatalf("the tsnet state survived ClearIdentity: %v", err)
	}
	if _, err := s.LoadStatus(); !IsNoStatus(err) {
		t.Fatalf("LoadStatus after ClearIdentity = %v, want ErrNoStatus", err)
	}
	if _, err := s.LoadAuth(); err != nil {
		t.Errorf("ClearIdentity took the bearer with it: %v", err)
	}
	if _, err := s.LoadEnrollment(); err != nil {
		t.Errorf("ClearIdentity took the staged enrollment with it: %v", err)
	}
	if _, err := os.Stat(s.Dir); err != nil {
		t.Errorf("ClearIdentity removed the state directory itself: %v", err)
	}
}

// Forgetting what is already forgotten is not an error: an install that
// refuses halfway through is one an operator runs again.
func TestClearIdentityOnADirectoryWithNoIdentityIsNotAnError(t *testing.T) {
	s := &Store{Dir: filepath.Join(t.TempDir(), "gentility")}
	if err := s.ClearIdentity(); err != nil {
		t.Fatalf("ClearIdentity on an empty directory: %v", err)
	}
	if err := s.ClearIdentity(); err != nil {
		t.Fatalf("second ClearIdentity: %v", err)
	}
}

// HoldsMachine is whether there is anything here that leave has to forget:
// an identity, tailscale's node key, or an enrollment staged to join on. A
// bearer is the person's and is not one of them.
func TestHoldsMachineSeesEachPieceAlone(t *testing.T) {
	put := map[string]func(*Store) error{
		"node.json":       func(s *Store) error { return s.SaveNode(enrolled()) },
		"enrollment.json": func(s *Store) error { return s.SaveEnrollment(Enrollment{EnrollmentID: "enr_01", PreauthKey: "k"}) },
		TSNetDir:          func(s *Store) error { return os.MkdirAll(filepath.Join(s.Dir, TSNetDir), 0o700) },
	}
	for name, write := range put {
		s := &Store{Dir: filepath.Join(t.TempDir(), "gentility")}
		if s.HoldsMachine() {
			t.Fatalf("an empty directory holds a machine")
		}
		if err := write(s); err != nil {
			t.Fatal(err)
		}
		if !s.HoldsMachine() {
			t.Fatalf("%s alone is not seen", name)
		}
	}

	s := &Store{Dir: t.TempDir()}
	if err := s.SaveAuth(Auth{BaseURL: "https://mcp.example", AccessToken: "bearer"}); err != nil {
		t.Fatal(err)
	}
	if s.HoldsMachine() {
		t.Fatalf("a bearer alone holds a machine")
	}
}
