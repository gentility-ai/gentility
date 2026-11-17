package tree

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

type writeFixture struct {
	mu           sync.Mutex
	bodies       map[string]string
	revisions    map[string]string
	puts         int
	posts        int
	status       int
	loseResponse bool
	beforeBlob   func()
	beforePut    func()
}

func newWriteFixture(t *testing.T) (*writeFixture, *httptest.Server) {
	t.Helper()
	f := &writeFixture{bodies: map[string]string{"acme/notes/test.md": "server"}, revisions: map[string]string{"acme/notes/test.md": "1"}}
	server := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(server.Close)
	return f, server
}
func (f *writeFixture) manifest() Manifest {
	m := Manifest{Principal: "person"}
	for _, dir := range []string{"", "acme", "acme/notes", "acme/boards", "acme/boards/general"} {
		flags := `false`
		if dir == "acme/notes" {
			flags = `{"create":true,"edit":true}`
		}
		if dir == "acme/boards/general" {
			flags = `{"create":true,"edit":false}`
		}
		m.Directories = append(m.Directories, Directory{Path: dir, Write: json.RawMessage(flags)})
	}
	m.AsOf.Cursors = json.RawMessage(`{"notes":5,"boards":[{"id":"board","seq":2}]}`)
	m.AsOf.RenderedAt = time.Now().UTC().Format(time.RFC3339Nano)
	for name, body := range f.bodies {
		for parent := path.Dir(name); parent != "."; parent = path.Dir(parent) {
			found := false
			for _, dir := range m.Directories {
				found = found || dir.Path == parent
			}
			if !found {
				m.Directories = append(m.Directories, Directory{Path: parent, Write: json.RawMessage(`{"create":true,"edit":true}`)})
			}
		}
		kind := "note"
		if postPath(name) {
			kind = "post"
		}
		size := int64(len(body))
		m.Files = append(m.Files, Entry{Path: name, SHA256: digest([]byte(body)), Size: &size, Revision: f.revisions[name], Kind: kind})
	}
	sort.Slice(m.Files, func(i, j int) bool { return m.Files[i].Path < m.Files[j].Path })
	return m
}
func (f *writeFixture) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	route := strings.TrimPrefix(r.URL.Path, "/overlay/self/tree/")
	if r.Method == "GET" {
		if route == "manifest" {
			json.NewEncoder(w).Encode(f.manifest())
			return
		}
		if f.beforeBlob != nil {
			fn := f.beforeBlob
			f.beforeBlob = nil
			fn()
		}
		for _, body := range f.bodies {
			if route == "blob/"+digest([]byte(body)) {
				io.WriteString(w, body)
				return
			}
		}
		w.WriteHeader(404)
		return
	}
	f.puts++
	if f.beforePut != nil {
		fn := f.beforePut
		f.beforePut = nil
		fn()
	}
	if f.status != 0 {
		w.WriteHeader(f.status)
		fmt.Fprint(w, `{"error":"stale"}`)
		return
	}
	body, _ := io.ReadAll(r.Body)
	if postPath(route) {
		f.posts++
		route = "acme/boards/general/" + time.Now().UTC().Format("20060102-1504") + "-post-" + strconv.Itoa(f.posts) + ".md"
		heading, rest, _ := strings.Cut(string(body), "\n\n")
		body = []byte(heading + "\n\nid: post-id\nposted_at: " + time.Now().UTC().Format(time.RFC3339Nano) + "\n" + rest)
		f.revisions[route] = "post-id-" + strconv.Itoa(f.posts)
	} else {
		old, exists := f.revisions[route]
		if exists && r.Header.Get("If-Match") != "\""+old+"\"" {
			w.WriteHeader(412)
			fmt.Fprint(w, `{"error":"stale"}`)
			return
		}
		next, _ := strconv.Atoi(old)
		f.revisions[route] = strconv.Itoa(next + 1)
	}
	f.bodies[route] = string(body)
	if f.loseResponse {
		f.loseResponse = false
		conn, _, _ := w.(http.Hijacker).Hijack()
		conn.Close()
		return
	}
	w.Header().Set("Location", "/overlay/self/tree/"+route)
	w.Header().Set("ETag", "\""+f.revisions[route]+"\"")
	w.WriteHeader(204)
}
func localWrite(t *testing.T, s *Syncer, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(s.options.Root, name), []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
}
func onceOK(t *testing.T, s *Syncer) {
	t.Helper()
	if err := s.Once(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestUserCreateEditAndDeleteRestore(t *testing.T) {
	f, server := newWriteFixture(t)
	s := openTest(t, server)
	onceOK(t, s)
	localWrite(t, s, "acme/notes/test.md", "edited")
	onceOK(t, s)
	localWrite(t, s, "acme/notes/new.md", "created")
	onceOK(t, s)
	if f.puts != 2 || f.bodies["acme/notes/test.md"] != "edited" || f.bodies["acme/notes/new.md"] != "created" {
		t.Fatal(f.puts, f.bodies)
	}
	if err := os.Remove(filepath.Join(s.options.Root, "acme/notes/test.md")); err != nil {
		t.Fatal(err)
	}
	onceOK(t, s)
	body, _ := os.ReadFile(filepath.Join(s.options.Root, "acme/notes/test.md"))
	if string(body) != "edited" || f.puts != 2 {
		t.Fatal("delete sent or not restored")
	}
	status, err := os.ReadFile(filepath.Join(s.options.Root, ".sync-status"))
	if err != nil || !strings.Contains(string(status), `"boards"`) {
		t.Fatal(string(status), err)
	}
}

func TestUserRejectionsRestoreAndPreserve(t *testing.T) {
	for _, status := range []int{412, 413, 422, 401, 403} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			f, server := newWriteFixture(t)
			s := openTest(t, server)
			onceOK(t, s)
			f.status = status
			localWrite(t, s, "acme/notes/test.md", "person text")
			err := s.Once(context.Background())
			if status == 401 || status == 403 {
				if !Terminal(err) {
					t.Fatalf("not terminal: %v", err)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
			}
			copies, _ := filepath.Glob(filepath.Join(s.options.Root, "acme/notes/*.rejected.md"))
			if len(copies) == 0 {
				t.Fatal("missing rejected copy")
			}
			preserved, _ := os.ReadFile(copies[0])
			if !strings.Contains(string(preserved), "person text") {
				t.Fatal(string(preserved))
			}
			if status != 401 && status != 403 {
				body, _ := os.ReadFile(filepath.Join(s.options.Root, "acme/notes/test.md"))
				if string(body) != "server" {
					t.Fatal(string(body))
				}
			}
		})
	}
}

func TestPostLostResponseAndCrashDeduplicate(t *testing.T) {
	for _, restart := range []bool{false, true} {
		t.Run(fmt.Sprint(restart), func(t *testing.T) {
			f, server := newWriteFixture(t)
			base := testDir(t)
			options := Options{Root: filepath.Join(base, "tree"), StateDir: filepath.Join(base, "state"), Principal: "person", Source: func(context.Context) (Source, error) {
				return Source{BaseURL: server.URL, Bearer: "test", Identity: "person-one", Organization: "acme"}, nil
			}}
			s, err := Open(options)
			if err != nil {
				t.Fatal(err)
			}
			onceOK(t, s)
			localWrite(t, s, "acme/boards/general/draft.md", "# Title\n\nText")
			f.loseResponse = true
			if err := s.Once(context.Background()); err == nil {
				t.Fatal("expected lost response")
			}
			key := s.journal.pending["acme/boards/general/draft.md"].PostKey
			if restart {
				s.Close()
				s, err = Open(options)
				if err != nil {
					t.Fatal(err)
				}
			}
			defer s.Close()
			onceOK(t, s) // grace: neither scan adoption nor another PUT
			if f.puts != 1 {
				t.Fatal("retried within grace")
			}
			p := s.journal.pending["acme/boards/general/draft.md"]
			if p.PostKey != key {
				t.Fatal("key changed")
			}
			p.SentAt = time.Now().Add(-postCommitGrace - time.Second)
			if err := s.journal.save(p); err != nil {
				t.Fatal(err)
			}
			onceOK(t, s)
			if f.posts != 1 || f.puts != 1 || len(s.journal.pending) != 0 {
				t.Fatalf("posts=%d puts=%d pending=%d", f.posts, f.puts, len(s.journal.pending))
			}
			if _, err := os.Stat(filepath.Join(s.options.Root, "acme/boards/general/draft.md")); !os.IsNotExist(err) {
				t.Fatal("draft not moved")
			}
		})
	}
}

func TestPostGenuineFailureRetriesSameAttempt(t *testing.T) {
	f, server := newWriteFixture(t)
	s := openTest(t, server)
	onceOK(t, s)
	localWrite(t, s, "acme/boards/general/draft.md", "# Title\n\nText")
	f.status = 503
	if err := s.Once(context.Background()); err == nil {
		t.Fatal("expected failure")
	}
	p := s.journal.pending["acme/boards/general/draft.md"]
	key := p.PostKey
	p.SentAt = time.Now().Add(-postCommitGrace - time.Second)
	if err := s.journal.save(p); err != nil {
		t.Fatal(err)
	}
	f.status = 0
	onceOK(t, s)
	if f.posts != 1 || f.puts != 2 {
		t.Fatal(f.posts, f.puts)
	}
	for name, body := range f.bodies {
		if postPath(name) && !strings.Contains(body, key) {
			t.Fatal("retry changed key")
		}
	}
}

func TestNestedNotesAndAmbiguousRename(t *testing.T) {
	f, server := newWriteFixture(t)
	s := openTest(t, server)
	onceOK(t, s)
	for _, name := range []string{"acme/notes/a/b/c/allowed.md", "acme/notes/a/b/c/d/refused.md"} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(s.options.Root, name)), 0700); err != nil {
			t.Fatal(err)
		}
		localWrite(t, s, name, "nested text")
	}
	onceOK(t, s)
	if f.puts != 1 || f.bodies["acme/notes/a/b/c/allowed.md"] != "nested text" {
		t.Fatal(f.puts, f.bodies)
	}
	copies, _ := filepath.Glob(filepath.Join(s.options.Root, "acme/notes/a/b/c/d/*.rejected.md"))
	if len(copies) != 1 {
		t.Fatal("deep note not rejected", copies)
	}
	if err := os.Rename(filepath.Join(s.options.Root, "acme/notes/test.md"), filepath.Join(s.options.Root, "acme/notes/renamed.md")); err != nil {
		t.Fatal(err)
	}
	localWrite(t, s, "acme/notes/renamed.md", "rename plus edit")
	onceOK(t, s)
	if f.puts != 1 {
		t.Fatal("sent ambiguous rename", f.puts)
	}
	body, err := s.root.read("acme/notes/test.md", maxBlob)
	if err != nil || string(body) != "server" {
		t.Fatal(string(body), err)
	}
	copies, _ = filepath.Glob(filepath.Join(s.options.Root, "acme/notes/renamed-*.conflict.md"))
	if len(copies) != 1 {
		t.Fatal("ambiguous rename not retained", copies)
	}
}

func TestMalformedPostRejectedWithoutPUT(t *testing.T) {
	f, server := newWriteFixture(t)
	s := openTest(t, server)
	onceOK(t, s)
	localWrite(t, s, "acme/boards/general/draft.md", "missing title")
	onceOK(t, s)
	if f.puts != 0 || len(s.journal.pending) != 0 {
		t.Fatal("malformed post sent or stuck")
	}
	copies, _ := filepath.Glob(filepath.Join(s.options.Root, "acme/boards/general/*.rejected.md"))
	if len(copies) != 1 {
		t.Fatal("missing rejected draft")
	}
}

func TestRemovedTargetIsRejectedWithoutRecreation(t *testing.T) {
	f, server := newWriteFixture(t)
	s := openTest(t, server)
	onceOK(t, s)
	localWrite(t, s, "acme/notes/test.md", "archived while editing")
	f.mu.Lock()
	delete(f.bodies, "acme/notes/test.md")
	delete(f.revisions, "acme/notes/test.md")
	f.mu.Unlock()
	onceOK(t, s)
	if f.puts != 0 {
		t.Fatal("recreated removed target")
	}
	copies, _ := filepath.Glob(filepath.Join(s.options.Root, "acme/notes/*.rejected.md"))
	if len(copies) != 1 {
		t.Fatal("removed target text lost")
	}
}

func TestWrite404AfterConcurrentArchivePreservesText(t *testing.T) {
	f, server := newWriteFixture(t)
	s := openTest(t, server)
	onceOK(t, s)
	localWrite(t, s, "acme/notes/test.md", "keep after archive")
	f.mu.Lock()
	f.beforePut = func() {
		delete(f.bodies, "acme/notes/test.md")
		delete(f.revisions, "acme/notes/test.md")
		f.status = 404
	}
	f.mu.Unlock()
	onceOK(t, s)
	if f.puts != 1 || len(s.journal.pending) != 0 {
		t.Fatal(f.puts, len(s.journal.pending))
	}
	copies, _ := filepath.Glob(filepath.Join(s.options.Root, "acme/notes/*.rejected.md"))
	if len(copies) != 1 {
		t.Fatal("404 edit not rejected")
	}
	body, _ := os.ReadFile(copies[0])
	if !strings.Contains(string(body), "not_found") || !strings.Contains(string(body), "keep after archive") {
		t.Fatal(string(body))
	}
}

func TestOversizedSaveDuringPUTIsPreserved(t *testing.T) {
	f, server := newWriteFixture(t)
	s := openTest(t, server)
	onceOK(t, s)
	localWrite(t, s, "acme/notes/test.md", "submitted")
	successor := strings.Repeat("z", maxWrite+1)
	f.mu.Lock()
	f.beforePut = func() { localWrite(t, s, "acme/notes/test.md", successor) }
	f.mu.Unlock()
	onceOK(t, s)
	if f.bodies["acme/notes/test.md"] != "submitted" {
		t.Fatal("wrong PUT")
	}
	copies, _ := filepath.Glob(filepath.Join(s.options.Root, "acme/notes/*.rejected.md"))
	if len(copies) != 1 {
		t.Fatal("successor not rejected", copies)
	}
	body, _ := os.ReadFile(copies[0])
	if !strings.HasSuffix(string(body), successor) || !strings.Contains(string(body), "body_too_large") {
		t.Fatal("successor truncated")
	}
	restored, _ := s.root.read("acme/notes/test.md", maxBlob)
	if string(restored) != "submitted" {
		t.Fatal("server copy not restored")
	}
}

func TestExistingPostAndIndexNeverPUT(t *testing.T) {
	f, server := newWriteFixture(t)
	names := []string{"acme/boards/general/existing.md", "acme/notes/INDEX.md"}
	for _, name := range names {
		f.bodies[name] = "immutable"
		f.revisions[name] = "1"
	}
	s := openTest(t, server)
	onceOK(t, s)
	for _, name := range names {
		if err := os.Chmod(filepath.Join(s.options.Root, name), 0600); err != nil {
			t.Fatal(err)
		}
		localWrite(t, s, name, "do not lose")
	}
	onceOK(t, s)
	if f.puts != 0 {
		t.Fatal("immutable file uploaded")
	}
	for _, name := range names {
		body, _ := s.root.read(name, maxBlob)
		if string(body) != "immutable" {
			t.Fatal(name, string(body))
		}
		copies, _ := filepath.Glob(filepath.Join(s.options.Root, strings.TrimSuffix(name, ".md")+"*.rejected.md"))
		if len(copies) != 1 {
			t.Fatal("immutable edit lost", name, copies)
		}
	}
}

func TestUserMaterializesRootIndex(t *testing.T) {
	f, server := newWriteFixture(t)
	f.bodies["INDEX.md"] = "# Shared tree\n"
	s := openTest(t, server)
	onceOK(t, s)
	body, err := os.ReadFile(filepath.Join(s.options.Root, "INDEX.md"))
	if err != nil || string(body) != "# Shared tree\n" {
		t.Fatalf("root index: %q, %v", body, err)
	}
}
