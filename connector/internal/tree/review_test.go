package tree

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSourceFailureBacksOff(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(403) }))
	defer server.Close()
	s := openTest(t, server)
	source := s.options.Source
	calls, waits := 0, 0
	s.options.Source = func(ctx context.Context) (Source, error) {
		calls++
		if calls < 3 {
			return Source{}, errors.New("discovery not ready")
		}
		return source(ctx)
	}
	s.options.Wait = func(context.Context, time.Duration) error { waits++; return nil }
	if err := s.Run(context.Background()); !Terminal(err) || waits != 2 {
		t.Fatalf("%v waits=%d", err, waits)
	}
	for _, err := range []error{errors.New("disk full"), errors.New("hash mismatch"), &HTTPError{302}, &HTTPError{400}, &HTTPError{429}, &HTTPError{503}} {
		if Terminal(err) {
			t.Fatal(err)
		}
	}
}
func TestRedirectAndResponseLimits(t *testing.T) {
	reached := false
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { reached = true }))
	defer target.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, 302) }))
	defer redirect.Close()
	_, _, _, err := get(context.Background(), Source{BaseURL: redirect.URL, Bearer: "secret"}, "manifest", "", maxManifest)
	if err == nil || reached {
		t.Fatal("followed redirect", err)
	}
	large := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(w, strings.NewReader(strings.Repeat("x", maxManifest+1)))
	}))
	defer large.Close()
	for _, route := range []string{"manifest", "blob/fixture"} {
		_, _, _, err := get(context.Background(), Source{BaseURL: large.URL}, route, "", maxBlob)
		if err == nil || !strings.Contains(err.Error(), "exceeds limit") || Terminal(err) {
			t.Fatal(route, err)
		}
	}
}
func TestUnmanagedCollisionAndWriteMode(t *testing.T) {
	m := testManifest("remote")
	m.Directories[1].Write = json.RawMessage(`{"edit":true}`)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "manifest") {
			json.NewEncoder(w).Encode(m)
		} else {
			io.WriteString(w, "remote")
		}
	}))
	defer server.Close()
	s := openTest(t, server)
	os.Mkdir(filepath.Join(s.options.Root, "acme"), 0700)
	name := filepath.Join(s.options.Root, "acme/note.md")
	os.WriteFile(name, []byte("local"), 0600)
	for i := 0; i < 2; i++ {
		if err := s.Once(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	data, _ := os.ReadFile(name)
	if string(data) != "remote" || len(s.checkpoint.Conflicts) != 1 {
		t.Fatal("did not materialize remote alongside preserved collision")
	}
	copies, _ := filepath.Glob(filepath.Join(s.options.Root, "acme/*.conflict.md"))
	if len(copies) != 1 {
		t.Fatal(copies)
	}
	preserved, _ := os.ReadFile(copies[0])
	if string(preserved) != "local" {
		t.Fatal("lost local collision")
	}
	os.Remove(name)
	if err := s.Once(context.Background()); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(name)
	if info.Mode().Perm() != 0600 {
		t.Fatal("write.edit did not make file writable")
	}
}
func TestManagedFileDirectoryTransitions(t *testing.T) {
	m := testManifest("remote")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "manifest") {
			json.NewEncoder(w).Encode(m)
		} else {
			io.WriteString(w, "remote")
		}
	}))
	defer server.Close()
	s := openTest(t, server)
	if err := s.Once(context.Background()); err != nil {
		t.Fatal(err)
	}
	m.Directories = append(m.Directories, Directory{Path: "acme/note.md", Write: json.RawMessage(`false`)})
	m.Files[0].Path = "acme/note.md/child.md"
	if err := s.Once(context.Background()); err != nil {
		t.Fatal("file to directory", err)
	}
	m = testManifest("remote")
	if err := s.Once(context.Background()); err != nil {
		t.Fatal("directory to file", err)
	}
}
func TestTempCleanupAndPrivateMetadata(t *testing.T) {
	base := testDir(t)
	root := filepath.Join(base, "tree")
	os.MkdirAll(filepath.Join(root, "acme"), 0700)
	temp := filepath.Join(root, "acme/.tree-abandoned")
	os.WriteFile(temp, []byte("partial"), 0600)
	s, err := Open(Options{Root: root, StateDir: filepath.Join(base, "state"), Principal: "person"})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := os.Stat(temp); !os.IsNotExist(err) {
		t.Fatal("temporary file remains")
	}
	for _, name := range []string{".tree-lock", ".tree-identity"} {
		if _, err := os.Stat(filepath.Join(root, name)); !os.IsNotExist(err) {
			t.Fatal("metadata in rendered root", name)
		}
	}
}
func TestManifestASCIIAndCursors(t *testing.T) {
	m := testManifest("x")
	m.Files[0].Path = "acme/café.md"
	if _, err := m.validate("person", "acme"); err == nil {
		t.Fatal("non-ASCII accepted")
	}
	m = testManifest("x")
	m.AsOf.Cursors = json.RawMessage(`null`)
	if _, err := m.validate("person", "acme"); err == nil {
		t.Fatal("null cursors accepted")
	}
}
func TestDiscoveryAdditionalBoundaries(t *testing.T) {
	dir := testDir(t)
	d := discoveryFixture(t)
	if err := WriteDiscovery(dir, d, testTrust); err != nil {
		t.Fatal(err)
	}
	before, _ := os.Stat(filepath.Join(dir, "discovery.json"))
	if err := WriteDiscovery(dir, d, testTrust); err != nil {
		t.Fatal(err)
	}
	after, _ := os.Stat(filepath.Join(dir, "discovery.json"))
	if !os.SameFile(before, after) {
		t.Fatal("unchanged discovery rewritten")
	}
	link := filepath.Join(testDir(t), "alias")
	os.Symlink(dir, link)
	if _, err := ReadDiscovery(link, testTrust); err == nil {
		t.Fatal("directory symlink accepted")
	}
	gateway, _ := netip.ParseAddrPort(d.Gateway)
	for _, address := range []string{"fd12::1", "2001:db8::1"} {
		d.Gateway = netip.AddrPortFrom(netip.MustParseAddr(address), gateway.Port()).String()
		data, _ := json.Marshal(d)
		os.WriteFile(filepath.Join(dir, "discovery.json"), data, 0644)
		if _, err := ReadDiscovery(dir, testTrust); err == nil {
			t.Fatal("read accepted off-overlay", address)
		}
		if _, _, err := d.Client(); err == nil {
			t.Fatal("client accepted off-overlay", address)
		}
	}
}
func TestLinuxDaemonOwnerAndGroup(t *testing.T) {
	dir := testDir(t)
	info, _ := os.Stat(dir)
	lookup := func(name string) (*user.User, error) {
		if name != "gentility" {
			t.Fatal(name)
		}
		return &user.User{Uid: "1234"}, nil
	}
	if err := daemonTrust(ownerInfo{info, 1234}, "linux", lookup); err != nil {
		t.Fatal(err)
	}
	if err := daemonTrust(ownerInfo{info, 1235}, "linux", lookup); err == nil {
		t.Fatal("wrong daemon owner accepted")
	}
	if DaemonGroup("linux") != "gentility" || DaemonGroup("darwin") != "admin" {
		t.Fatal("wrong operator groups")
	}
}

func TestStartupFailureBacksOff(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(403) }))
	defer server.Close()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	calls, pauses := 0, 0
	err = Run(context.Background(), Options{
		Root: filepath.Join(base, "tree"), StateDir: filepath.Join(base, "state"), Principal: "node",
		GroupID: func() (int, error) {
			calls++
			if calls < 3 {
				return 0, errors.New("group lookup temporarily unavailable")
			}
			return os.Getegid(), nil
		},
		Source: func(context.Context) (Source, error) {
			return Source{BaseURL: server.URL, Bearer: "fixture", Organization: "acme"}, nil
		},
		Wait: func(context.Context, time.Duration) error { pauses++; return nil },
	})
	if !Terminal(err) || pauses != 2 {
		t.Fatalf("error=%v pauses=%d", err, pauses)
	}
}

func TestEmptyDaemonOrganizationRetriesWithoutBinding(t *testing.T) {
	m := testManifest("remote")
	m.Principal = "node"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "manifest") {
			json.NewEncoder(w).Encode(m)
		} else {
			io.WriteString(w, "remote")
		}
	}))
	defer server.Close()
	base := testDir(t)
	org := ""
	s, err := Open(Options{
		Root: filepath.Join(base, "tree"), StateDir: filepath.Join(base, "state"), Principal: "node",
		GroupID: func() (int, error) { return os.Getegid(), nil },
		Source: func(context.Context) (Source, error) {
			return Source{BaseURL: server.URL, Bearer: "fixture", Organization: org}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pauses := 0
	s.options.Wait = func(context.Context, time.Duration) error {
		pauses++
		if pauses == 1 {
			if s.checkpoint.Identity != "" {
				t.Fatal("bound empty org")
			}
			if _, err := os.Stat(filepath.Join(base, "state/sync.json")); !os.IsNotExist(err) {
				t.Fatal("wrote empty-org checkpoint", err)
			}
			org = "acme"
			return nil
		}
		cancel()
		return ctx.Err()
	}
	if err := s.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if s.checkpoint.Identity != "node-org:acme" || pauses != 2 {
		t.Fatalf("identity=%q pauses=%d", s.checkpoint.Identity, pauses)
	}
}

func TestDirectoryToFilePreservesFinderFile(t *testing.T) {
	m := testManifest("remote")
	m.Directories = append(m.Directories, Directory{Path: "acme/note.md", Write: json.RawMessage(`false`)})
	m.Files[0].Path = "acme/note.md/child.md"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "manifest") {
			json.NewEncoder(w).Encode(m)
		} else {
			io.WriteString(w, "remote")
		}
	}))
	defer server.Close()
	s := openTest(t, server)
	if err := s.Once(context.Background()); err != nil {
		t.Fatal(err)
	}
	stray := filepath.Join(s.options.Root, "acme/note.md/.DS_Store")
	if err := os.WriteFile(stray, []byte("finder"), 0600); err != nil {
		t.Fatal(err)
	}
	m = testManifest("remote")
	for attempt := 0; attempt < 2; attempt++ {
		if err := s.Once(context.Background()); err != nil {
			t.Fatal(err)
		}
		if len(s.checkpoint.Conflicts) != 1 {
			t.Fatal(s.checkpoint.Conflicts)
		}
		copies, _ := filepath.Glob(filepath.Join(s.options.Root, "acme/*.conflict.md/.DS_Store"))
		if len(copies) != 1 {
			t.Fatal(copies)
		}
		if data, err := os.ReadFile(copies[0]); err != nil || string(data) != "finder" {
			t.Fatal("lost Finder file", err)
		}
	}
}

func TestWriteCapabilityObjectsSetFileModes(t *testing.T) {
	for _, policy := range []string{`{"create":true,"edit":true}`, `{"create":true,"edit":false}`} {
		t.Run(policy, func(t *testing.T) {
			m := testManifest("remote")
			m.Directories = append(m.Directories, Directory{Path: "acme/notes", Write: json.RawMessage(policy)})
			m.Files[0].Path = "acme/notes/note.md"
			encoded, err := json.Marshal(m)
			if err != nil {
				t.Fatal(err)
			}
			var decoded Manifest
			if err := json.Unmarshal(encoded, &decoded); err != nil {
				t.Fatal(err)
			}
			if _, err := decoded.validate("person", "acme"); err != nil {
				t.Fatal(err)
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					t.Errorf("unexpected request: %s", r.Method)
				}
				if strings.HasSuffix(r.URL.Path, "manifest") {
					w.Write(encoded)
				} else {
					io.WriteString(w, "remote")
				}
			}))
			defer server.Close()
			s := openTest(t, server)
			if err := s.Once(context.Background()); err != nil {
				t.Fatal(err)
			}
			name := filepath.Join(s.options.Root, "acme/notes/note.md")
			body, err := os.ReadFile(name)
			if err != nil || string(body) != "remote" {
				t.Fatalf("body=%q err=%v", body, err)
			}
			info, err := os.Stat(name)
			if err != nil {
				t.Fatal(err)
			}
			want := os.FileMode(0400)
			if policy == `{"create":true,"edit":true}` {
				want = 0600
			}
			if info.Mode().Perm() != want {
				t.Fatalf("unexpected write mode: %v", info.Mode())
			}
		})
	}
}
