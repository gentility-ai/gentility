package tree

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func testDir(t *testing.T) string {
	t.Helper()
	p, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return p
}
func testManifest(body string) Manifest {
	m := Manifest{Principal: "person", Directories: []Directory{{Path: "", Write: json.RawMessage(`false`)}, {Path: "acme", Write: json.RawMessage(`false`)}}}
	m.AsOf.Cursors = json.RawMessage(`{"notes":12,"tasks":3}`)
	m.AsOf.RenderedAt = "2026-11-01T00:00:00Z"
	size := int64(len(body))
	m.Files = []Entry{{Path: "acme/note.md", SHA256: digest([]byte(body)), Size: &size}}
	return m
}
func openTest(t *testing.T, server *httptest.Server) *Syncer {
	t.Helper()
	base := testDir(t)
	s, err := Open(Options{Root: filepath.Join(base, "tree"), StateDir: filepath.Join(base, "state"), Principal: "person", Source: func(context.Context) (Source, error) {
		return Source{BaseURL: server.URL, Bearer: "test", Identity: "person-one", Organization: "acme", HTTP: server.Client()}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s
}
func TestReconcile(t *testing.T) {
	body := "first"
	m := testManifest(body)
	etag := `W/"one"`
	blobs := 0
	conditional := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.Header.Get("Authorization") != "Bearer test" {
			t.Error("unexpected request")
		}
		if strings.HasSuffix(r.URL.Path, "manifest") {
			if r.Header.Get("If-None-Match") == etag {
				conditional = true
				w.WriteHeader(304)
				return
			}
			w.Header().Set("ETag", etag)
			json.NewEncoder(w).Encode(m)
			return
		}
		blobs++
		io.WriteString(w, body)
	}))
	defer server.Close()
	s := openTest(t, server)
	ctx := context.Background()
	if err := s.Once(ctx); err != nil {
		t.Fatal(err)
	}
	name := filepath.Join(s.options.Root, "acme/note.md")
	old, err := os.Open(name)
	if err != nil {
		t.Fatal(err)
	}
	defer old.Close()
	info, _ := old.Stat()
	if info.Mode().Perm() != 0400 {
		t.Fatal(info.Mode())
	}
	if err := s.Once(ctx); err != nil {
		t.Fatal(err)
	}
	if !conditional || blobs != 1 {
		t.Fatalf("conditional=%v blobs=%d", conditional, blobs)
	}
	body = "second"
	m = testManifest(body)
	etag = `W/"two"`
	if err := s.Once(ctx); err != nil {
		t.Fatal(err)
	}
	previous, _ := io.ReadAll(old)
	current, _ := os.ReadFile(name)
	if string(previous) != "first" || string(current) != "second" {
		t.Fatal("replacement was not atomic")
	}
	unknown := filepath.Join(s.options.Root, "acme/local.md")
	if err := os.WriteFile(unknown, []byte("local"), 0600); err != nil {
		t.Fatal(err)
	}
	m.Files = nil
	etag = `W/"three"`
	if err := s.Once(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(name); !os.IsNotExist(err) {
		t.Fatal("managed file remains")
	}
	if _, err := os.Stat(unknown); err != nil {
		t.Fatal("unmanaged file removed", err)
	}
}
func TestBadBlobPreservesFile(t *testing.T) {
	body := "good"
	m := testManifest(body)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "manifest") {
			json.NewEncoder(w).Encode(m)
		} else {
			io.WriteString(w, body)
		}
	}))
	defer server.Close()
	s := openTest(t, server)
	if err := s.Once(context.Background()); err != nil {
		t.Fatal(err)
	}
	m = testManifest("expected")
	body = "corrupt"
	if err := s.Once(context.Background()); err == nil || !strings.Contains(err.Error(), "SHA-256") {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(filepath.Join(s.options.Root, "acme/note.md"))
	if string(got) != "good" {
		t.Fatal(string(got))
	}
}
func TestUnsafePaths(t *testing.T) {
	for _, name := range []string{"../outside", "/absolute", "acme/../outside", "acme//file", "acme/./file", "acme\\file"} {
		t.Run(name, func(t *testing.T) {
			m := testManifest("x")
			m.Files[0].Path = name
			if _, err := m.validate("person", "acme"); err == nil {
				t.Fatal("accepted unsafe path")
			}
		})
	}
	for _, kind := range []string{"directory", "file"} {
		t.Run(kind, func(t *testing.T) {
			m := testManifest("x")
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { json.NewEncoder(w).Encode(m) }))
			defer server.Close()
			s := openTest(t, server)
			outside := testDir(t)
			link := filepath.Join(s.options.Root, "acme")
			if kind == "file" {
				if err := os.Mkdir(link, 0700); err != nil {
					t.Fatal(err)
				}
				link = filepath.Join(link, "note.md")
				outside = filepath.Join(outside, "untouched")
				os.WriteFile(outside, []byte("private"), 0600)
			}
			if err := os.Symlink(outside, link); err != nil {
				t.Fatal(err)
			}
			if err := s.Once(context.Background()); err == nil {
				t.Fatal("followed symlink")
			}
		})
	}
}
func TestBackoffAndTerminal(t *testing.T) {
	statuses := []int{503, 503, 200, 503, 403}
	i := 0
	m := testManifest("unused")
	m.Files = nil
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		status := statuses[i]
		i++
		if status == 200 {
			json.NewEncoder(w).Encode(m)
		} else {
			w.WriteHeader(status)
		}
	}))
	defer server.Close()
	s := openTest(t, server)
	var delays []time.Duration
	s.options.Random = func() float64 { return 0 }
	s.options.Wait = func(_ context.Context, d time.Duration) error { delays = append(delays, d); return nil }
	err := s.Run(context.Background())
	if err == nil || !Terminal(err) {
		t.Fatal(err)
	}
	want := []time.Duration{time.Second / 2, time.Second, 12 * time.Second, time.Second / 2}
	if len(delays) != len(want) {
		t.Fatal(delays)
	}
	for i := range want {
		if delays[i] != want[i] {
			t.Fatal(delays)
		}
	}
	data, _ := os.ReadFile(filepath.Join(s.options.StateDir, "sync.json"))
	if !strings.Contains(string(data), "HTTP 403") {
		t.Fatal("failure not recorded")
	}
	for _, status := range []int{401, 403} {
		if !Terminal(&HTTPError{status}) {
			t.Fatal(status)
		}
	}
}

func TestBearerRotationAndBinding(t *testing.T) {
	m := testManifest("first")
	body := "first"
	bearer := "one"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+bearer {
			t.Error("stale bearer")
		}
		if strings.HasSuffix(r.URL.Path, "manifest") {
			json.NewEncoder(w).Encode(m)
		} else {
			io.WriteString(w, body)
		}
	}))
	defer server.Close()
	s := openTest(t, server)
	identity := "https://login.example:uid:501"
	org := "acme"
	s.options.Source = func(context.Context) (Source, error) {
		return Source{BaseURL: server.URL, Bearer: bearer, Identity: identity, Organization: org, HTTP: server.Client()}, nil
	}
	if err := s.Once(context.Background()); err != nil {
		t.Fatal(err)
	}
	bearer = "two"
	body = "second"
	m = testManifest(body)
	m.Files[0].Path = "acme/new.md"
	if err := s.Once(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(s.options.Root, "acme/note.md")); !os.IsNotExist(err) {
		t.Fatal("old person's managed file remains")
	}
	for _, other := range []string{"https://other.example:uid:501", "https://login.example:uid:502"} {
		identity = other
		if err := s.Once(context.Background()); err == nil {
			t.Fatal("accepted different identity")
		}
	}
	identity = "https://login.example:uid:501"
	org = "elsewhere"
	if err := s.Once(context.Background()); err == nil {
		t.Fatal("accepted different org")
	}
}
func TestNodeModeAndPrivatePathRefusal(t *testing.T) {
	m := testManifest("public")
	m.Principal = "node"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "manifest") {
			json.NewEncoder(w).Encode(m)
		} else {
			io.WriteString(w, "public")
		}
	}))
	defer server.Close()
	base := testDir(t)
	gid := os.Getegid()
	groups, err := os.Getgroups()
	if err != nil {
		t.Fatal(err)
	}
	for _, candidate := range groups {
		if candidate != gid {
			gid = candidate
			break
		}
	}
	s, err := Open(Options{Root: filepath.Join(base, "tree"), StateDir: filepath.Join(base, "state"), Principal: "node", GroupID: func() (int, error) { return gid, nil }, Source: func(context.Context) (Source, error) {
		return Source{BaseURL: server.URL, Bearer: "node", Identity: "node:one", Organization: "acme", HTTP: server.Client()}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Once(context.Background()); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(base, "tree/acme/note.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"tree", "tree/acme"} {
		dirInfo, e := os.Stat(filepath.Join(base, name))
		if e != nil || dirInfo.Mode().Perm() != 0750 {
			t.Fatalf("daemon directory %s: %v %v", name, dirInfo, e)
		}
	}
	for _, name := range []string{"tree", "tree/acme", "tree/acme/note.md"} {
		info, err := os.Stat(filepath.Join(base, name))
		if err != nil {
			t.Fatal(err)
		}
		if actual := int(info.Sys().(*syscall.Stat_t).Gid); actual != gid {
			t.Fatalf("%s group=%d, want %d", name, actual, gid)
		}
	}
	if info.Mode().Perm() != 0440 {
		t.Fatal(info.Mode())
	}
	source := s.options.Source
	s.options.Source = func(ctx context.Context) (Source, error) {
		value, err := source(ctx)
		value.Identity = "node:reenrolled"
		value.Bearer = "replacement"
		return value, err
	}
	if err := s.Once(context.Background()); err != nil {
		t.Fatal("same-org reenrollment", err)
	}
	m.Directories = append(m.Directories, Directory{Path: "acme/me", Write: json.RawMessage(`false`)})
	if err := s.Once(context.Background()); err == nil {
		t.Fatal("node accepted private directory")
	}
	if _, err := os.Stat(filepath.Join(base, "tree/acme/me")); !os.IsNotExist(err) {
		t.Fatal("created private directory")
	}
}
func TestRestartRepairsAndRecoversInventory(t *testing.T) {
	m := testManifest("good")
	conditional := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "manifest") {
			conditional = r.Header.Get("If-None-Match") != ""
			w.Header().Set("ETag", `"same"`)
			json.NewEncoder(w).Encode(m)
		} else {
			io.WriteString(w, "good")
		}
	}))
	defer server.Close()
	base := testDir(t)
	options := Options{Root: filepath.Join(base, "tree"), StateDir: filepath.Join(base, "state"), Principal: "person", Source: func(context.Context) (Source, error) {
		return Source{BaseURL: server.URL, Bearer: "test", Identity: "one", Organization: "acme", HTTP: server.Client()}, nil
	}}
	s, err := Open(options)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Once(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(options); err == nil {
		t.Fatal("second syncer acquired root")
	}
	// Model a crash after inventory was persisted and a new file installed.
	s.checkpoint.Managed = []string{"acme/partial.md"}
	if err := s.save(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(options.Root, "acme/partial.md"), []byte("partial"), 0400); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(options.Root, "acme/note.md")); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = Open(options)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Once(context.Background()); err != nil {
		t.Fatal(err)
	}
	if conditional {
		t.Fatal("restart trusted ETag without checking disk")
	}
	if data, err := os.ReadFile(filepath.Join(options.Root, "acme/note.md")); err != nil || string(data) != "good" {
		t.Fatal("failed repair", err)
	}
	if _, err := os.Stat(filepath.Join(options.Root, "acme/partial.md")); !os.IsNotExist(err) {
		t.Fatal("partial work remains")
	}
}
