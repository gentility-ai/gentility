package tree

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLocalPhoenixWrites uses only a disposable organization supplied by a
// local proof driver. It proves real HTTP behavior, not overlay TLS or services.
func TestLocalPhoenixWrites(t *testing.T) {
	base := os.Getenv("TREE_WRITE_PROOF_URL")
	if base == "" {
		t.Skip("local Phoenix write proof only")
	}
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	org := os.Getenv("TREE_PROOF_ORG")
	source := Source{BaseURL: base, Bearer: os.Getenv("TREE_PROOF_BEARER"), Identity: "local-write-proof", Organization: org, HTTP: &http.Client{Transport: &peerTransport{peer: os.Getenv("TREE_PROOF_PEER")}}}
	s, err := Open(Options{Root: root, StateDir: root + "-state", Principal: "person", Source: func(context.Context) (Source, error) { return source, nil }})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	defer os.RemoveAll(root + "-state")
	sync := func() {
		t.Helper()
		if err := s.Once(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	read := func(name string) string {
		t.Helper()
		b, e := os.ReadFile(filepath.Join(root, name))
		if e != nil {
			t.Fatal(e)
		}
		return string(b)
	}
	sync()
	name := org + "/notes/live-write.md"
	write(name, "created locally\n")
	sync()
	if read(name) != "created locally\n" {
		t.Fatal("create did not converge")
	}
	write(name, "edited locally\n")
	sync()
	if read(name) != "edited locally\n" {
		t.Fatal("edit did not converge")
	}
	// Advance the server without refreshing the connector's revision.
	previous := entryMap(s.checkpoint.Manifest)[name]
	if _, err := put(context.Background(), source, pendingWrite{Path: name, Revision: previous.Revision, Body: []byte("server wins\n")}); err != nil {
		t.Fatal(err)
	}
	write(name, "stale local text\n")
	sync()
	if read(name) != "server wins\n" {
		t.Fatal("stale copy not restored")
	}
	rejected, err := filepath.Glob(filepath.Join(root, org, "notes", "*.rejected.md"))
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, p := range rejected {
		b, _ := os.ReadFile(p)
		found = found || strings.Contains(string(b), "stale local text")
	}
	if !found {
		t.Fatal("rejected local text lost")
	}
	if err := os.Remove(filepath.Join(root, name)); err != nil {
		t.Fatal(err)
	}
	sync()
	if read(name) != "server wins\n" {
		t.Fatal("delete not restored")
	}
	post := org + "/boards/general/draft.md"
	write(post, "# Local write proof\n\nA disposable proof post.\n")
	sync()
	if _, err := os.Stat(filepath.Join(root, post)); !os.IsNotExist(err) {
		t.Fatal("draft not canonicalized")
	}
	posts := 0
	for _, entry := range s.checkpoint.Manifest.Files {
		if entry.Kind == "post" && strings.Contains(read(entry.Path), "A disposable proof post.") {
			posts++
		}
	}
	if posts != 1 {
		t.Fatalf("expected one canonical proof post, got %d", posts)
	}
	if !strings.Contains(read(".sync-status"), "person") {
		t.Fatal("missing principal status")
	}
	t.Log("real Phoenix create, edit, stale rejection, deletion restore, canonical post and sync status passed")
}
