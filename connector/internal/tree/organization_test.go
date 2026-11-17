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
	"testing"
	"time"
)

const testOrganizationID = "019a3f00-0000-7000-8000-000000000001"

// The fixture changes only the public slug, retaining the authenticated ID.
func organizationFixture(t *testing.T, principal string) (*Syncer, *Manifest, *Source) {
	t.Helper()
	m := (&writeFixture{bodies: map[string]string{"acme/notes/test.md": "server"}, revisions: map[string]string{"acme/notes/test.md": "1"}}).manifest()
	m.Principal = principal
	if principal == "node" {
		for i := range m.Directories {
			m.Directories[i].Write = json.RawMessage(`false`)
		}
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			t.Error("rename must not replay a write")
			w.WriteHeader(500)
			return
		}
		if strings.HasSuffix(r.URL.Path, "manifest") {
			json.NewEncoder(w).Encode(m)
			return
		}
		io.WriteString(w, "server")
	}))
	t.Cleanup(server.Close)
	source := &Source{BaseURL: server.URL, HTTP: server.Client(), Bearer: "test", Identity: "person-one", Organization: "acme"}
	base := testDir(t)
	s, err := Open(Options{Root: filepath.Join(base, "tree"), StateDir: filepath.Join(base, "state"), Principal: principal, GroupID: func() (int, error) { return os.Getgid(), nil }, Source: func(context.Context) (Source, error) { return *source, nil }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s, &m, source
}
func syncOrganization(t *testing.T, s *Syncer) {
	t.Helper()
	if err := s.Once(context.Background()); err != nil {
		t.Fatal(err)
	}
}
func renameManifest(m *Manifest, slug string) {
	old := m.Organization.Slug
	m.Organization = &Organization{ID: m.Organization.ID, Slug: slug}
	replace := func(p string) string {
		if p == old {
			return slug
		}
		return strings.Replace(p, old+"/", slug+"/", 1)
	}
	for i := range m.Directories {
		m.Directories[i].Path = replace(m.Directories[i].Path)
	}
	for i := range m.Files {
		m.Files[i].Path = replace(m.Files[i].Path)
	}
	for i := range m.Indexes {
		m.Indexes[i].Path = replace(m.Indexes[i].Path)
	}
}
func TestOrganizationBindingMigrationAndMove(t *testing.T) {
	for _, principal := range []string{"person", "node"} {
		t.Run(principal, func(t *testing.T) {
			s, m, source := organizationFixture(t, principal)
			syncOrganization(t, s) // Old config and manifest establish a legacy root.
			source.OrganizationID = testOrganizationID
			m.Organization = &Organization{ID: testOrganizationID, Slug: "acme"}
			syncOrganization(t, s)
			if s.checkpoint.OrganizationID != testOrganizationID {
				t.Fatal("legacy binding did not learn authenticated ID")
			}
			if err := os.WriteFile(filepath.Join(s.options.Root, "acme", "unmanaged.txt"), []byte("keep"), 0600); err != nil {
				t.Fatal(err)
			}
			renameManifest(m, "renamed") // Discovery intentionally still carries the old slug.
			syncOrganization(t, s)
			got, err := os.ReadFile(filepath.Join(s.options.Root, "renamed", "unmanaged.txt"))
			if err != nil || string(got) != "keep" {
				t.Fatalf("unmanaged text lost: %q %v", got, err)
			}
			if s.checkpoint.Organization != "renamed" {
				t.Fatal("checkpoint not moved")
			}
			source.Organization = "renamed"
			syncOrganization(t, s)
			source.OrganizationID = ""
			if err := s.Once(context.Background()); !Terminal(err) {
				t.Fatalf("ID binding downgrade allowed: %v", err)
			}
		})
	}
}
func TestOrganizationIdentityRefusals(t *testing.T) {
	for _, scenario := range []string{"wrong_id", "legacy_rename", "occupied", "symlink"} {
		t.Run(scenario, func(t *testing.T) {
			s, m, source := organizationFixture(t, "person")
			if scenario != "legacy_rename" {
				source.OrganizationID = testOrganizationID
				m.Organization = &Organization{ID: testOrganizationID, Slug: "acme"}
			}
			syncOrganization(t, s)
			switch scenario {
			case "wrong_id":
				source.OrganizationID = "019a3f00-0000-7000-8000-000000000002"
			default:
				m.Organization = &Organization{ID: testOrganizationID, Slug: "acme"}
				source.OrganizationID = testOrganizationID
				renameManifest(m, "renamed")
				if scenario == "occupied" {
					if err := os.Mkdir(filepath.Join(s.options.Root, "renamed"), 0700); err != nil {
						t.Fatal(err)
					}
				}
				if scenario == "symlink" {
					if err := os.Symlink("acme", filepath.Join(s.options.Root, "renamed")); err != nil {
						t.Fatal(err)
					}
				}
			}
			if err := s.Once(context.Background()); !Terminal(err) {
				t.Fatalf("unsafe transition allowed: %v", err)
			}
			got, err := os.ReadFile(filepath.Join(s.options.Root, "acme/notes/test.md"))
			if err != nil || string(got) != "server" {
				t.Fatal("old tree altered")
			}
		})
	}
}
func TestRenameDrainsPendingEditAndAmbiguousPostWithoutResending(t *testing.T) {
	s, m, source := organizationFixture(t, "person")
	source.OrganizationID = testOrganizationID
	m.Organization = &Organization{ID: testOrganizationID, Slug: "acme"}
	syncOrganization(t, s)
	note := "acme/notes/test.md"
	if err := os.WriteFile(filepath.Join(s.options.Root, note), []byte("my pending edit"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := s.journal.enqueue(note, "1", []byte("my pending edit"), time.Now()); err != nil {
		t.Fatal(err)
	}
	post := "acme/boards/general/draft.md"
	p := pendingWrite{Path: post, Body: []byte("my ambiguous post"), LocalHash: digest([]byte("my ambiguous post")), PostKey: testOrganizationID, Started: time.Now(), Sending: true, SentAt: time.Now(), AmbiguousAt: time.Now()}
	if err := s.journal.save(p); err != nil {
		t.Fatal(err)
	}
	renameManifest(m, "renamed")
	syncOrganization(t, s)
	if s.checkpoint.Organization != "acme" || !strings.Contains(s.checkpoint.Failure, "rename") {
		t.Fatal("pending journal did not hold move")
	}
	if len(s.journal.pending) != 1 {
		t.Fatalf("edit not drained: %d", len(s.journal.pending))
	}
	p = s.journal.pending[post]
	p.AmbiguousAt = time.Now().Add(-25 * time.Hour)
	p.SentAt = p.AmbiguousAt
	if err := s.journal.save(p); err != nil {
		t.Fatal(err)
	}
	syncOrganization(t, s)
	syncOrganization(t, s)
	if s.checkpoint.Organization != "renamed" {
		t.Fatal("drained root did not move")
	}
	for _, entry := range []struct{ dir, text string }{{"notes", "my pending edit"}, {"boards/general", "my ambiguous post"}} {
		matches, err := filepath.Glob(filepath.Join(s.options.Root, "renamed", entry.dir, "*.rejected.md"))
		if err != nil || len(matches) != 1 {
			t.Fatalf("missing preserved text: %v %v", matches, err)
		}
		body, err := os.ReadFile(matches[0])
		if err != nil || !strings.Contains(string(body), entry.text) || !strings.Contains(string(body), "organization renamed") {
			t.Fatalf("incorrect preserved text: %s %v", body, err)
		}
	}
}

func TestRootMoveResumesAtPersistenceBoundaries(t *testing.T) {
	for _, renamed := range []bool{false, true} {
		t.Run(map[bool]string{false: "intent", true: "rename"}[renamed], func(t *testing.T) {
			s, m, source := organizationFixture(t, "person")
			source.OrganizationID = testOrganizationID
			m.Organization = &Organization{ID: testOrganizationID, Slug: "acme"}
			syncOrganization(t, s)
			stat, err := s.directoryIdentity("acme")
			if err != nil {
				t.Fatal(err)
			}
			s.checkpoint.Move = &rootMove{OrganizationID: testOrganizationID, From: "acme", To: "renamed", Device: uint64(stat.Dev), Inode: uint64(stat.Ino)}
			if err := s.save(); err != nil {
				t.Fatal(err)
			}
			if renamed {
				if err := renameExclusive(s.root.fd, "acme", "renamed"); err != nil {
					t.Fatal(err)
				}
			}
			options := s.options
			s.Close()
			reopened, err := Open(options)
			if err != nil {
				t.Fatal(err)
			}
			*s = *reopened // fixture cleanup closes the reopened descriptors.
			renameManifest(m, "renamed")
			syncOrganization(t, s)
			if s.checkpoint.Move != nil || s.checkpoint.Organization != "renamed" {
				t.Fatal("intent did not complete")
			}
			syncOrganization(t, s)
			body, err := os.ReadFile(filepath.Join(options.Root, "renamed/notes/test.md"))
			if err != nil || string(body) != "server" {
				t.Fatalf("moved data: %s %v", body, err)
			}
		})
	}
}

func TestRenameRecoversAcceptedPostByNewBoardPrefix(t *testing.T) {
	s, m, source := organizationFixture(t, "person")
	source.OrganizationID = testOrganizationID
	m.Organization = &Organization{ID: testOrganizationID, Slug: "acme"}
	syncOrganization(t, s)
	start := time.Now().Add(-5 * time.Minute)
	post := "acme/boards/general/draft.md"
	body := []byte("# Title\n\nposted_at: " + start.Format(time.RFC3339Nano) + "\nrefs: {\"tree_write\":\"" + testOrganizationID + "\"}\n\nAccepted text")
	p := pendingWrite{Path: post, Body: body, LocalHash: digest(body), PostKey: testOrganizationID, Started: start, SentAt: start, AmbiguousAt: start, Sending: true}
	if err := s.root.replace(post, body, 0600); err != nil {
		t.Fatal(err)
	}
	if err := s.journal.save(p); err != nil {
		t.Fatal(err)
	}
	renameManifest(m, "renamed")
	size := int64(len(body))
	canonical := "renamed/boards/general/" + start.Format("20060102-1504") + "-title-abc.md"
	m.Files = append(m.Files, Entry{Path: canonical, SHA256: digest(body), Size: &size, Kind: "post", Revision: "post-id"})
	original := source.HTTP
	source.HTTP = &http.Client{Transport: renameBlobTransport{body: body, fallback: original.Transport}}
	syncOrganization(t, s)
	if len(s.journal.pending) != 0 {
		t.Fatal("accepted post did not drain")
	}
	syncOrganization(t, s)
	got, err := os.ReadFile(filepath.Join(s.options.Root, canonical))
	if err != nil || string(got) != string(body) {
		t.Fatalf("accepted post lost: %s %v", got, err)
	}
	matches, _ := filepath.Glob(filepath.Join(s.options.Root, "renamed/boards/general/*.rejected.md"))
	if len(matches) != 0 {
		t.Fatal("accepted post rejected")
	}
}

type renameBlobTransport struct {
	body     []byte
	fallback http.RoundTripper
}

func (r renameBlobTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if strings.HasSuffix(req.URL.Path, "blob/"+digest(r.body)) {
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(string(r.body)))}, nil
	}
	transport := r.fallback
	if transport == nil {
		transport = http.DefaultTransport
	}
	return transport.RoundTrip(req)
}

func TestOrganizationWireCompatibilityAndNoIdentityInference(t *testing.T) {
	for _, configID := range []string{"", testOrganizationID} {
		t.Run("config_"+configID, func(t *testing.T) {
			s, m, source := organizationFixture(t, "person")
			source.OrganizationID = configID
			syncOrganization(t, s) // old manifest remains compatible with new config.
			if s.checkpoint.OrganizationID != "" {
				t.Fatal("bound without manifest corroboration")
			}
			m.Organization = &Organization{ID: testOrganizationID, Slug: "acme"}
			syncOrganization(t, s)
			if s.checkpoint.OrganizationID != configID {
				t.Fatal("inferred ID from manifest instead of config")
			}
			if configID != "" {
				m.Organization = nil
				if err := s.Once(context.Background()); !Terminal(err) {
					t.Fatalf("manifest downgrade: %v", err)
				}
			}
		})
	}
}

func TestOrganizationRejectsMalformedOrContradictoryIdentity(t *testing.T) {
	for _, id := range []string{"not-an-id", "019a3f00-0000-7000-8000-000000000002"} {
		t.Run(id, func(t *testing.T) {
			s, m, source := organizationFixture(t, "person")
			source.OrganizationID = testOrganizationID
			m.Organization = &Organization{ID: id, Slug: "acme"}
			if err := s.Once(context.Background()); !Terminal(err) {
				t.Fatalf("invalid ID accepted: %v", err)
			}
			if _, err := os.Stat(filepath.Join(s.options.Root, "acme")); !os.IsNotExist(err) {
				t.Fatal("refusal materialized a tree")
			}
		})
	}
}

func TestRenameDuringWriteResponsePreservesEdit(t *testing.T) {
	s, m, source := organizationFixture(t, "person")
	source.OrganizationID = testOrganizationID
	m.Organization = &Organization{ID: testOrganizationID, Slug: "acme"}
	syncOrganization(t, s)
	body := "edit while staff renames"
	if err := os.WriteFile(filepath.Join(s.options.Root, "acme/notes/test.md"), []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	transport := &renameOnWriteTransport{manifest: m, fallback: source.HTTP.Transport}
	source.HTTP = &http.Client{Transport: transport}
	syncOrganization(t, s)
	if transport.writes != 1 {
		t.Fatalf("writes: %d", transport.writes)
	}
	syncOrganization(t, s)
	copies, err := filepath.Glob(filepath.Join(s.options.Root, "renamed/notes/*.rejected.md"))
	if err != nil || len(copies) != 1 {
		t.Fatalf("rejected copies: %v %v", copies, err)
	}
	preserved, err := os.ReadFile(copies[0])
	if err != nil || !strings.Contains(string(preserved), body) || !strings.Contains(string(preserved), "organization renamed") {
		t.Fatalf("lost edit or reason: %s %v", preserved, err)
	}
	if s.checkpoint.Organization != "renamed" {
		t.Fatal("move did not finish")
	}
}

type renameOnWriteTransport struct {
	manifest *Manifest
	fallback http.RoundTripper
	writes   int
}

func (r *renameOnWriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Method == http.MethodPut {
		r.writes++
		renameManifest(r.manifest, "renamed")
		return &http.Response{StatusCode: 422, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"error":"readonly_target"}`))}, nil
	}
	transport := r.fallback
	if transport == nil {
		transport = http.DefaultTransport
	}
	return transport.RoundTrip(req)
}

func TestRenameMissingDirectoryMaterializesFresh(t *testing.T) {
	for _, principal := range []string{"person", "node"} {
		t.Run(principal, func(t *testing.T) {
			s, m, source := organizationFixture(t, principal)
			source.OrganizationID = testOrganizationID
			m.Organization = &Organization{ID: testOrganizationID, Slug: "acme"}
			syncOrganization(t, s)
			if err := os.RemoveAll(filepath.Join(s.options.Root, "acme")); err != nil {
				t.Fatal(err)
			}
			renameManifest(m, "renamed")
			syncOrganization(t, s)
			syncOrganization(t, s)
			if s.checkpoint.Move != nil || s.checkpoint.Organization != "renamed" || s.checkpoint.OrganizationID != testOrganizationID {
				t.Fatal("missing source did not advance the same ID binding")
			}
			body, err := os.ReadFile(filepath.Join(s.options.Root, "renamed/notes/test.md"))
			if err != nil || string(body) != "server" {
				t.Fatalf("fresh materialization: %s %v", body, err)
			}
		})
	}
}

func TestLegacyRenameReportsSlugRecoveryForBothPrincipals(t *testing.T) {
	for _, principal := range []string{"person", "node"} {
		t.Run(principal, func(t *testing.T) {
			s, _, source := organizationFixture(t, principal)
			syncOrganization(t, s)
			source.Organization = "renamed"
			source.OrganizationID = testOrganizationID
			err := s.Once(context.Background())
			if !Terminal(err) || !strings.Contains(err.Error(), "restore the old slug") {
				t.Fatalf("wrong recovery refusal: %v", err)
			}
		})
	}
}

func TestRenamePendingEditAloneHoldsThenMoves(t *testing.T) {
	s, m, source := organizationFixture(t, "person")
	source.OrganizationID = testOrganizationID
	m.Organization = &Organization{ID: testOrganizationID, Slug: "acme"}
	syncOrganization(t, s)
	if err := os.WriteFile(filepath.Join(s.options.Root, "acme/notes/test.md"), []byte("edit only"), 0600); err != nil {
		t.Fatal(err)
	}
	renameManifest(m, "renamed")
	syncOrganization(t, s)
	if s.checkpoint.Organization != "acme" || !strings.Contains(s.checkpoint.Failure, "rename") {
		t.Fatal("edit did not hold move")
	}
	if len(s.journal.pending) != 0 {
		t.Fatal("edit did not drain")
	}
	syncOrganization(t, s)
	if s.checkpoint.Organization != "renamed" {
		t.Fatal("drained edit did not allow move")
	}
	copies, err := filepath.Glob(filepath.Join(s.options.Root, "renamed/notes/*.rejected.md"))
	if err != nil || len(copies) != 1 {
		t.Fatalf("missing preserved edit: %v %v", copies, err)
	}
	body, err := os.ReadFile(copies[0])
	if err != nil || !strings.Contains(string(body), "edit only") || !strings.Contains(string(body), "organization renamed") {
		t.Fatalf("lost edit: %s %v", body, err)
	}
}
