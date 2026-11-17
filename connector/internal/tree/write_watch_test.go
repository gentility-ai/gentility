package tree

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestNativeWatcherDebouncesAtomicSavesAndReplacedDirectories(t *testing.T) {
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
	for _, replace := range []bool{false, true} {
		if replace {
			dir := filepath.Join(s.options.Root, "acme/notes")
			if err := os.Rename(dir, dir+".old"); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(dir, 0700); err != nil {
				t.Fatal(err)
			}
			if err := w.refresh(s.root, s.options.Root, s.checkpoint.Manifest); err != nil {
				t.Fatal(err)
			}
		}
		started := time.Now()
		// Two editor saves must coalesce into one wake after the last save.
		timer := time.AfterFunc(100*time.Millisecond, func() {
			_ = os.WriteFile(filepath.Join(s.options.Root, "acme/notes/new.md"), []byte("first"), 0600)
		})
		second := time.AfterFunc(300*time.Millisecond, func() {
			_ = os.WriteFile(filepath.Join(s.options.Root, "acme/notes/new.md"), []byte("second"), 0600)
		})
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		err := w.wait(ctx, 10*time.Second)
		cancel()
		timer.Stop()
		second.Stop()
		if err != nil {
			t.Fatal(err)
		}
		if elapsed := time.Since(started); elapsed < 750*time.Millisecond || elapsed > 2*time.Second {
			t.Fatalf("watcher did not debounce saves: %s", elapsed)
		}
	}
}

func TestRecoveryCopyCollisionNeverDropsPendingBytes(t *testing.T) {
	root, err := openDirectory(testDir(t))
	if err != nil {
		t.Fatal(err)
	}
	defer root.close()
	name := "test.rejected.md"
	if err := root.install(name, []byte("someone else's text"), 0600); err != nil {
		t.Fatal(err)
	}
	actual, err := root.preserveRejection(name, []byte("pending bytes"))
	if err != nil {
		t.Fatal(err)
	}
	if actual == name {
		t.Fatal("trusted unrelated existing recovery copy")
	}
	body, err := root.read(actual, 100)
	if err != nil || string(body) != "pending bytes" {
		t.Fatal(string(body), err)
	}
	original, _ := root.read(name, 100)
	if string(original) != "someone else's text" {
		t.Fatal("overwrote collision")
	}
}

func TestPollCapturesEditMadeDuringBlobFetch(t *testing.T) {
	f, server := newWriteFixture(t)
	s := openTest(t, server)
	onceOK(t, s)
	f.mu.Lock()
	f.bodies["acme/notes/test.md"] = "remote changed"
	f.revisions["acme/notes/test.md"] = "2"
	f.beforeBlob = func() { localWrite(t, s, "acme/notes/test.md", "raced local edit") }
	f.mu.Unlock()
	onceOK(t, s)
	pending, exists := s.journal.pending["acme/notes/test.md"]
	if !exists || string(pending.Body) != "raced local edit" || pending.Revision != "1" {
		t.Fatal("poll lost local edit or rebased its revision", pending)
	}
	onceOK(t, s)
	body, _ := s.root.read("acme/notes/test.md", maxBlob)
	if string(body) != "remote changed" {
		t.Fatal(string(body))
	}
	copies, _ := filepath.Glob(filepath.Join(s.options.Root, "acme/notes/*.rejected.md"))
	if len(copies) == 0 {
		t.Fatal("stale raced edit not preserved")
	}
}
