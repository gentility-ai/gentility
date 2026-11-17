package tree

import (
	"testing"
	"time"
)

func TestPendingPostSurvivesRestartWithSameKeyAndBytes(t *testing.T) {
	state, err := openDirectory(testDir(t))
	if err != nil {
		t.Fatal(err)
	}
	defer state.close()
	journal, err := openWriteJournal(state, "origin:uid:test", "acme")
	if err != nil {
		t.Fatal(err)
	}
	name := "acme/boards/general/draft.md"
	body := []byte("# Title\n\nBody")
	if err := journal.enqueue(name, "", body, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	first := journal.pending[name]
	first.Sending = true
	first.SentAt = time.Now().UTC()
	if err := journal.save(first); err != nil {
		t.Fatal(err)
	}
	journal.dir.close()
	restored, err := openWriteJournal(state, "origin:uid:test", "acme")
	if err != nil {
		t.Fatal(err)
	}
	defer restored.dir.close()
	second := restored.pending[name]
	if first.PostKey == "" || first.PostKey != second.PostKey || string(first.Body) != string(second.Body) || !second.Sending || second.SentAt.IsZero() {
		t.Fatalf("pending operation changed across restart: %+v", second)
	}
	if second.LocalHash != digest(body) {
		t.Fatal("original bytes hash lost")
	}
	if err := restored.remove(name); err != nil {
		t.Fatal(err)
	}
	empty, err := openWriteJournal(state, "origin:uid:test", "acme")
	if err != nil {
		t.Fatal(err)
	}
	defer empty.dir.close()
	if len(empty.pending) != 0 {
		t.Fatal("completed operation resurrected")
	}
}

func TestPendingWritesRefuseAnotherIdentity(t *testing.T) {
	state, err := openDirectory(testDir(t))
	if err != nil {
		t.Fatal(err)
	}
	defer state.close()
	journal, err := openWriteJournal(state, "first", "acme")
	if err != nil {
		t.Fatal(err)
	}
	if err := journal.enqueue("acme/notes/n.md", "7", []byte("edit"), time.Now()); err != nil {
		t.Fatal(err)
	}
	journal.dir.close()
	if other, err := openWriteJournal(state, "second", "acme"); err == nil {
		other.dir.close()
		t.Fatal("rebound pending writes")
	}
}
