package state

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestRecordOrigin(t *testing.T) {
	store := &Store{Dir: t.TempDir()}
	before := Node{NodeID: "node", NodeCredential: "credential", RuntimeToken: "runtime", LastRevision: 17, CAPEM: "ca", HeadURL: "https://head.example", Hostname: "host"}
	if err := store.SaveNode(before); err != nil {
		t.Fatal(err)
	}
	origin := "https://mcp.example"
	if err := store.RecordOrigin(origin); err != nil {
		t.Fatal(err)
	}
	before.Origin = origin
	got, err := (&Store{Dir: store.Dir}).LoadNode()
	if err != nil || !reflect.DeepEqual(before, got) {
		t.Fatalf("identity changed: %v", err)
	}
	info, _ := os.Stat(filepath.Join(store.Dir, nodeFile))
	if info.Mode().Perm() != 0600 {
		t.Fatal("state must stay private")
	}
	bytes, _ := os.ReadFile(filepath.Join(store.Dir, nodeFile))
	for _, same := range []string{"", origin} {
		if err := store.RecordOrigin(same); err != nil {
			t.Fatal(err)
		}
	}
	var mismatch *OriginMismatchError
	if !errors.As(store.RecordOrigin("https://other.example"), &mismatch) || mismatch.NodeID != before.NodeID {
		t.Fatal("expected typed mismatch")
	}
	after, _ := os.ReadFile(filepath.Join(store.Dir, nodeFile))
	if string(after) != string(bytes) {
		t.Fatal("no-op or mismatch changed state")
	}
}

func TestRecordOriginWriteFailurePreservesIdentity(t *testing.T) {
	store := &Store{Dir: t.TempDir()}
	if err := store.SaveNode(Node{NodeID: "node", NodeCredential: "credential", LastRevision: 17}); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(filepath.Join(store.Dir, nodeFile))
	failure := errors.New("injected disk failure")
	calls := 0
	store.writeJSONOverride = func(name string, value any, mode os.FileMode) error { calls++; return failure }
	if err := store.RecordOrigin("https://mcp.example"); !errors.Is(err, failure) {
		t.Fatalf("write error lost: %v", err)
	}
	after, _ := os.ReadFile(filepath.Join(store.Dir, nodeFile))
	if calls != 1 || string(before) != string(after) {
		t.Fatal("failed persistence changed identity")
	}
}
