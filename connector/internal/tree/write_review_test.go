package tree

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func assertNoCopies(t *testing.T, s *Syncer, suffix string) {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(s.options.Root, "acme/notes/*"+suffix))
	if err != nil || len(matches) != 0 {
		t.Fatalf("unexpected copies: %v %v", matches, err)
	}
}

func TestSuccessfulSavesAndRemovalLeaveNoConflicts(t *testing.T) {
	f, server := newWriteFixture(t)
	s := openTest(t, server)
	onceOK(t, s)
	for i := 0; i < 4; i++ {
		localWrite(t, s, "acme/notes/test.md", fmt.Sprint("save", i))
		onceOK(t, s)
	}
	f.mu.Lock()
	delete(f.bodies, "acme/notes/test.md")
	delete(f.revisions, "acme/notes/test.md")
	f.mu.Unlock()
	onceOK(t, s)
	assertNoCopies(t, s, ".conflict.md")
	if len(s.checkpoint.Conflicts) != 0 {
		t.Fatal(s.checkpoint.Conflicts)
	}
}

func TestSuccessorDuringPUTUsesReceiptRevision(t *testing.T) {
	f, server := newWriteFixture(t)
	s := openTest(t, server)
	onceOK(t, s)
	localWrite(t, s, "acme/notes/test.md", "first")
	f.mu.Lock()
	f.beforePut = func() { localWrite(t, s, "acme/notes/test.md", "second") }
	f.mu.Unlock()
	onceOK(t, s)
	p, ok := s.journal.pending["acme/notes/test.md"]
	if !ok || p.Revision != "2" || string(p.Body) != "second" {
		t.Fatalf("successor: %+v", p)
	}
	onceOK(t, s)
	if f.bodies["acme/notes/test.md"] != "second" || f.puts != 2 {
		t.Fatal(f.bodies, f.puts)
	}
	assertNoCopies(t, s, ".rejected.md")
	assertNoCopies(t, s, ".conflict.md")
}

func TestLostNoteResponseAdoptsMatchingManifest(t *testing.T) {
	for _, create := range []bool{false, true} {
		t.Run(fmt.Sprint(create), func(t *testing.T) {
			f, server := newWriteFixture(t)
			s := openTest(t, server)
			onceOK(t, s)
			name := "acme/notes/test.md"
			if create {
				name = "acme/notes/new.md"
			}
			localWrite(t, s, name, "accepted")
			f.loseResponse = true
			if err := s.Once(context.Background()); err == nil {
				t.Fatal("expected lost response")
			}
			onceOK(t, s)
			if f.puts != 1 || len(s.journal.pending) != 0 {
				t.Fatal(f.puts, s.journal.pending)
			}
			assertNoCopies(t, s, ".rejected.md")
			assertNoCopies(t, s, ".conflict.md")
		})
	}
}

func TestStraySymlinkAndFIFODoNotStopSync(t *testing.T) {
	_, server := newWriteFixture(t)
	s := openTest(t, server)
	onceOK(t, s)
	if err := os.Symlink("/nonexistent", filepath.Join(s.options.Root, "acme/notes/stray.md")); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mkfifo(filepath.Join(s.options.Root, "acme/notes/pipe"), 0600); err != nil {
		t.Fatal(err)
	}
	onceOK(t, s)
	// A managed path still refuses a symlink.
	name := filepath.Join(s.options.Root, "acme/notes/test.md")
	if err := os.Remove(name); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/nonexistent", name); err != nil {
		t.Fatal(err)
	}
	if err := s.Once(context.Background()); err == nil {
		t.Fatal("managed symlink accepted")
	}
}

func TestMissingOtherDirectoryDoesNotBlockCreate(t *testing.T) {
	f, server := newWriteFixture(t)
	f.bodies["acme/notes/sub/source.md"] = "source"
	f.revisions["acme/notes/sub/source.md"] = "1"
	s := openTest(t, server)
	onceOK(t, s)
	if err := os.Remove(filepath.Join(s.options.Root, "acme/notes/sub/source.md")); err != nil {
		t.Fatal(err)
	}
	localWrite(t, s, "acme/notes/new.md", "new")
	onceOK(t, s)
	if f.bodies["acme/notes/new.md"] != "new" {
		t.Fatal("unrelated deletion blocked create")
	}
}

func TestWatcherLimitsAndClosedChannels(t *testing.T) {
	_, server := newWriteFixture(t)
	s := openTest(t, server)
	onceOK(t, s)
	w, err := newTreeWatcher()
	if err != nil {
		t.Fatal(err)
	}
	defer w.close()
	if err := w.refresh(s.root, s.options.Root, s.checkpoint.Manifest); err != nil {
		t.Fatal(err)
	}
	if _, ok := w.dirs["acme/boards/general"]; ok {
		t.Fatal("watching create-only board")
	}
	for i := 0; i < maxWatchedFiles; i++ {
		localWrite(t, s, fmt.Sprintf("acme/notes/file-%d.tmp", i), "x")
	}
	if err := w.refresh(s.root, s.options.Root, s.checkpoint.Manifest); err != nil {
		t.Fatal(err)
	}
	if len(w.dirs) != 0 {
		t.Fatal("watch budget exceeded")
	}
	w.native.Close()
	if err := w.wait(context.Background(), time.Second); err == nil {
		t.Fatal("closed watcher returned success")
	}
}

func TestCrashBeforeFirstPostReachesServer(t *testing.T) {
	f, server := newWriteFixture(t)
	s := openTest(t, server)
	onceOK(t, s)
	name := "acme/boards/general/draft.md"
	localWrite(t, s, name, "# Title\n\nBody")
	if err := s.captureLocal(); err != nil {
		t.Fatal(err)
	}
	p := s.journal.pending[name]
	key := p.PostKey
	p.Sending = true
	p.SentAt = time.Now().Add(-postCommitGrace - time.Second)
	p.AmbiguousAt = p.SentAt
	if err := s.journal.save(p); err != nil {
		t.Fatal(err)
	}
	options := s.options
	s.Close()
	var err error
	s, err = Open(options)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	onceOK(t, s)
	if f.posts != 1 || f.puts != 1 {
		t.Fatal(f.posts, f.puts)
	}
	found := false
	for _, body := range f.bodies {
		found = found || strings.Contains(body, key)
	}
	if !found {
		t.Fatal("recovery replaced durable key")
	}
}

func TestRejectionDoesNotLeaveConflict(t *testing.T) {
	f, server := newWriteFixture(t)
	s := openTest(t, server)
	onceOK(t, s)
	localWrite(t, s, "acme/notes/test.md", "stale edit")
	f.status = 412
	onceOK(t, s)
	assertNoCopies(t, s, ".conflict.md")
	copies, _ := filepath.Glob(filepath.Join(s.options.Root, "acme/notes/*.rejected.md"))
	if len(copies) != 1 {
		t.Fatal(copies)
	}
}
func TestTerminalLocalFailureKeepsPendingWrite(t *testing.T) {
	_, server := newWriteFixture(t)
	s := openTest(t, server)
	onceOK(t, s)
	localWrite(t, s, "acme/notes/test.md", "pending")
	if err := s.captureLocal(); err != nil {
		t.Fatal(err)
	}
	source := s.options.Source
	s.options.Source = func(ctx context.Context) (Source, error) {
		v, err := source(ctx)
		v.Identity = "another-person"
		return v, err
	}
	if err := s.Once(context.Background()); !Terminal(err) {
		t.Fatalf("got %v", err)
	}
	if p := s.journal.pending["acme/notes/test.md"]; p.Rejection != "" || string(p.Body) != "pending" {
		t.Fatal(p)
	}
	assertNoCopies(t, s, ".rejected.md")
}
func TestRejectAllPreservesAcceptedReceipt(t *testing.T) {
	_, server := newWriteFixture(t)
	s := openTest(t, server)
	onceOK(t, s)
	name := "acme/notes/test.md"
	localWrite(t, s, name, "accepted")
	if err := s.captureLocal(); err != nil {
		t.Fatal(err)
	}
	p := s.journal.pending[name]
	p.Receipt = &writeReceipt{Path: name, Revision: "2"}
	if err := s.journal.save(p); err != nil {
		t.Fatal(err)
	}
	if err := s.rejectAll("access_revoked"); err != nil {
		t.Fatal(err)
	}
	assertNoCopies(t, s, ".rejected.md")
}
func TestNotesInheritanceUsesRootShape(t *testing.T) {
	for _, root := range []string{"acme/notes", "acme/me/notes", "acme/projects/project/notes", "acme/notes/nested/notes", "acme/boards/notes"} {
		m := Manifest{Directories: []Directory{{Path: root, Write: json.RawMessage(`{"create":true,"edit":true}`)}}}
		want := root == "acme/notes" || root == "acme/me/notes" || root == "acme/projects/project/notes"
		if got := m.policy(root + "/nested/file.md").Create; got != want {
			t.Fatalf("%s got %v", root, got)
		}
	}
}

func TestLateEditAndRevertReplaceFinishedReceipt(t *testing.T) {
	for _, revert := range []bool{false, true} {
		t.Run(fmt.Sprint(revert), func(t *testing.T) {
			f, server := newWriteFixture(t)
			s := openTest(t, server)
			onceOK(t, s)
			name := "acme/notes/test.md"
			localWrite(t, s, name, "submitted")
			successor := "late edit"
			if revert {
				successor = "server"
				f.beforePut = func() { localWrite(t, s, name, successor) }
			} else {
				f.bodies["acme/notes/aaa.md"] = "another download"
				f.revisions["acme/notes/aaa.md"] = "1"
				f.beforeBlob = func() { localWrite(t, s, name, successor) }
			}
			onceOK(t, s)
			p, ok := s.journal.pending[name]
			if !ok || string(p.Body) != successor || p.Revision != "2" || p.Receipt != nil {
				t.Fatalf("successor=%+v", p)
			}
			// Deferred cleanup must not remove the replacement record on disk either.
			opts := s.options
			s.Close()
			var err error
			s, err = Open(opts)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			onceOK(t, s)
			if f.bodies[name] != successor || f.puts != 2 {
				t.Fatal(f.bodies, f.puts)
			}
			assertNoCopies(t, s, ".rejected.md")
		})
	}
}

func TestCrashMidMaterialisationRetainsOwnership(t *testing.T) {
	f, _ := newWriteFixture(t)
	count := 0
	interrupt := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if interrupt && strings.Contains(r.URL.Path, "/blob/") {
			count++
			if count == 6 {
				w.WriteHeader(503)
				return
			}
		}
		f.serve(w, r)
	}))
	defer server.Close()
	s := openTest(t, server)
	onceOK(t, s)
	for i := 0; i < 6; i++ {
		name := fmt.Sprintf("acme/notes/new-%d.md", i)
		if i%2 == 0 {
			name = fmt.Sprintf("acme/boards/general/new-%d.md", i)
		}
		f.bodies[name] = fmt.Sprintf("new body %d", i)
		f.revisions[name] = "1"
	}
	source, err := s.options.Source(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	interrupt = true
	// Call the install stage directly: interruption discards its in-memory state
	// without the normal end-of-round checkpoint, just like process death.
	if err := s.applyWritable(context.Background(), source, f.manifest(), ""); err == nil {
		t.Fatal("expected interruption")
	}
	if count != 6 {
		t.Fatal(count)
	}
	opts := s.options
	s.Close()
	interrupt = false
	s, err = Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	onceOK(t, s)
	if f.puts != 0 {
		t.Fatalf("replayed installed files as creates: %d", f.puts)
	}
	if len(s.checkpoint.Conflicts) != 0 {
		t.Fatal(s.checkpoint.Conflicts)
	}
	for name, want := range f.bodies {
		body, err := s.root.read(name, maxBlob)
		if err != nil || string(body) != want {
			t.Fatalf("%s: %s %v", name, body, err)
		}
	}
}

func TestRejectionUsesOriginalFilename(t *testing.T) {
	_, server := newWriteFixture(t)
	s := openTest(t, server)
	onceOK(t, s)
	localWrite(t, s, "acme/notes/new.md", "keep me")
	if err := s.rejectLocal("acme/notes/new.md", "readonly_target"); err != nil {
		t.Fatal(err)
	}
	body, err := s.root.read("acme/notes/new.rejected.md", maxBlob)
	if err != nil || !strings.Contains(string(body), "keep me") {
		t.Fatal(string(body), err)
	}
	if strings.Contains(strings.Join(s.checkpoint.Conflicts, "\n"), ".conflict") {
		t.Fatal(s.checkpoint.Conflicts)
	}
}

func TestUnknownCollisionIsNotEnqueuedWhenRootIsEditable(t *testing.T) {
	f, server := newWriteFixture(t)
	s := openTest(t, server)
	onceOK(t, s)
	// An unknown baseline is a zero Entry. Even an editable root must never
	// turn that zero Entry into permission to upload an unmanaged collision.
	for i := range s.checkpoint.Manifest.Directories {
		if s.checkpoint.Manifest.Directories[i].Path == "" {
			s.checkpoint.Manifest.Directories[i].Write = json.RawMessage(`{"create":true,"edit":true}`)
		}
	}
	name := "acme/notes/collision.md"
	localWrite(t, s, name, "person's file")
	f.bodies[name] = "server's file"
	f.revisions[name] = "1"
	source, err := s.options.Source(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.applyWritable(context.Background(), source, f.manifest(), ""); err != nil {
		t.Fatal(err)
	}
	if len(s.journal.pending) != 0 {
		t.Fatal("unknown file enqueued", s.journal.pending)
	}
	body, err := s.root.read(name, maxBlob)
	if err != nil || string(body) != "server's file" {
		t.Fatal(string(body), err)
	}
	copies, _ := filepath.Glob(filepath.Join(s.options.Root, "acme/notes/*.conflict.md"))
	if len(copies) != 1 {
		t.Fatal(copies)
	}
	body, err = os.ReadFile(copies[0])
	if err != nil || string(body) != "person's file" {
		t.Fatal(string(body), err)
	}
}
