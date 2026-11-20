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

// layoutFixture renders the same skills in any layout, as the server does:
// layout 2, 3 or 4 only when asked for exactly that and able to render it,
// layout 1 otherwise, and blobs only from the requested layout's render.
type layoutFixture struct {
	mu        sync.Mutex
	layouts   int // the highest layout rendered; 1 is an older server, 2 and 3 predate layout 4
	pinned    int // answers a layout-4 request with this earlier layout; see answer
	principal string
	skills    map[string]string // keyed by layout-2 path
	notes     map[string]string // writable for a person in every layout
	revisions map[string]string
	requests  []string // GET routes, with their query
	puts      []string
	putStatus int
	failBlob  string // fail the next request for the blob with this body
	forge     func(*Manifest)
}

func newLayoutFixture(t *testing.T, principal string) (*layoutFixture, *Syncer) {
	t.Helper()
	f := &layoutFixture{layouts: 1, principal: principal,
		skills:    map[string]string{"skills/acme/review.md": "org skill", "skills/acme/projects/web/deploy.md": "project skill", "skills/me/notes.md": "personal skill"},
		notes:     map[string]string{"acme/notes/plan.md": "plan"},
		revisions: map[string]string{"skills/acme/review.md": "1", "skills/acme/projects/web/deploy.md": "1", "skills/me/notes.md": "1", "acme/notes/plan.md": "1"}}
	server := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(server.Close)
	base := testDir(t)
	s, err := Open(Options{Root: filepath.Join(base, "tree"), StateDir: filepath.Join(base, "state"), Principal: principal,
		GroupID: func() (int, error) { return os.Getgid(), nil },
		Source: func(context.Context) (Source, error) {
			return Source{BaseURL: server.URL, HTTP: server.Client(), Bearer: "test", Identity: "person-one", Organization: "acme", OrganizationID: testOrganizationID}, nil
		}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return f, s
}

// layout1Path is where layout 1 keeps a skill named by its layout-2 path.
func layout1Path(name string) string {
	rest := strings.TrimPrefix(name, "skills/")
	owner, rest, _ := strings.Cut(rest, "/")
	if project, slug, ok := strings.Cut(strings.TrimPrefix(rest, "projects/"), "/"); ok && strings.HasPrefix(rest, "projects/") {
		return owner + "/projects/" + project + "/skills/" + slug
	}
	return owner + "/skills/" + rest
}

// layout3Path is where layout 3 keeps a skill named by its layout-2 path: in
// its plugin root, skills/me, skills/<org>/org or skills/<org>/projects/<p>.
func layout3Path(name string) string {
	root, slug := parentPath(name), strings.TrimSuffix(path.Base(name), ".md")
	if root != "skills/me" && !strings.Contains(root, "/projects/") {
		root += "/org"
	}
	return root + "/skills/" + slug + "/SKILL.md"
}

// layout4Path is where layout 4 keeps a skill named by its layout-2 path: in
// its plugin folder, skills/_me, skills/<org> or skills/<org>.<project>.
func layout4Path(name string) string {
	folder := parentPath(name)
	if folder == "skills/me" {
		folder = "skills/_me"
	}
	folder = strings.Replace(folder, "/projects/", ".", 1)
	return folder + "/" + strings.TrimSuffix(path.Base(name), ".md") + "/SKILL.md"
}

// pluginBody4 is the manifest layout 4 renders for the plugin folder named.
func pluginBody4(folder string) string {
	return "{\n  \"name\": \"" + folder + "\",\n  \"description\": \"" + folder + " skills\",\n  \"skills\": \"./\"\n}\n"
}

// answer has the fixture answer the connector's layout-4 requests with the
// layout given. An earlier layout stands in for a root an earlier connector
// brought there by asking for it; the server itself knows layout 4.
func (f *layoutFixture) answer(layout int) {
	f.layouts, f.pinned = 4, 0
	if layout < 4 {
		f.pinned = layout
	}
}

func (f *layoutFixture) render(layout int) (Manifest, map[string]string) {
	m := Manifest{Principal: f.principal, Organization: &Organization{ID: testOrganizationID, Slug: "acme"}}
	if layout != 1 {
		m.Layout = layout
	}
	m.AsOf.Cursors = json.RawMessage(`{"skills":[]}`)
	m.AsOf.RenderedAt = time.Now().UTC().Format(time.RFC3339Nano)
	bodies := map[string]string{}
	paths := map[string]string{}
	for key := range f.skills {
		if f.principal == "node" && strings.HasPrefix(key, "skills/me/") {
			continue
		}
		switch layout {
		case 1:
			paths[key] = layout1Path(key)
		case 2:
			paths[key] = key
		case 3:
			paths[key] = layout3Path(key)
		case 4:
			paths[key] = layout4Path(key)
		}
	}
	// Layouts 3 and 4 are read-only; earlier layouts let a person write skills.
	writable := map[string]bool{"acme/notes": f.principal == "person"}
	for _, p := range paths {
		writable[parentPath(p)] = f.principal == "person" && layout < 3
	}
	seen := map[string]bool{}
	dir := func(p string) {
		if seen[p] {
			return
		}
		seen[p] = true
		flags := `false`
		if writable[p] {
			flags = `{"create":true,"edit":true}`
		}
		m.Directories = append(m.Directories, Directory{Path: p, Write: json.RawMessage(flags)})
	}
	file := func(p, body, revision, kind string) {
		for parent := parentPath(p); parent != ""; parent = parentPath(parent) {
			dir(parent)
		}
		size := int64(len(body))
		bodies[digest([]byte(body))] = body
		m.Files = append(m.Files, Entry{Path: p, SHA256: digest([]byte(body)), Size: &size, Revision: revision, Kind: kind})
	}
	dir("")
	dir("acme")
	keys := make([]string, 0, len(paths))
	for key := range paths {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		file(paths[key], f.skills[key], f.revisions[key], "skill")
	}
	for name, body := range f.notes {
		file(name, body, f.revisions[name], "note")
	}
	index := func(name, body string) {
		for parent := parentPath(name); parent != ""; parent = parentPath(parent) {
			dir(parent)
		}
		bodies[digest([]byte(body))] = body
		m.Indexes = append(m.Indexes, Entry{Path: name, SHA256: digest([]byte(body))})
	}
	switch layout {
	case 1:
		index("acme/INDEX.md", "acme index")
	case 2:
		// Layout 2 points the old skill directories at skills/ during the dual
		// period. The root me/ has no pointer: layout 2 has no root me/ at all.
		index("acme/skills/INDEX.md", "moved to skills/")
		index("acme/projects/web/skills/INDEX.md", "moved to skills/")
		index("skills/INDEX.md", "skills index")
	case 3:
		// Each plugin root holds its manifest and skills, and no INDEX; only
		// the plain directories above them carry cards.
		roots := map[string]bool{}
		for _, key := range keys {
			root := parentPath(parentPath(parentPath(paths[key])))
			if !roots[root] {
				roots[root] = true
				file(root+"/.claude-plugin/plugin.json", `{"name": "`+path.Base(root)+`"}`, "1", "plugin")
			}
		}
		index("skills/INDEX.md", "skills index")
		index("skills/acme/INDEX.md", "acme skills")
		index("skills/acme/projects/INDEX.md", "project skills")
	case 4:
		// Each folder is one plugin: its card, its manifest and its skills.
		folders := map[string]bool{}
		for _, key := range keys {
			folder := parentPath(parentPath(paths[key]))
			if !folders[folder] {
				folders[folder] = true
				file(folder+"/.claude-plugin/plugin.json", pluginBody4(path.Base(folder)), "1", "plugin")
				index(folder+"/INDEX.md", path.Base(folder)+" plugin")
			}
		}
		index("skills/INDEX.md", "skills index")
	}
	return m, bodies
}

func (f *layoutFixture) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	route := strings.TrimPrefix(r.URL.Path, "/overlay/self/tree/")
	layout := 1
	if asked := r.URL.Query().Get("layout"); (asked == "2" || asked == "3" || asked == "4") && int(asked[0]-'0') <= f.layouts {
		layout = int(asked[0] - '0')
	}
	if layout == 4 && f.pinned != 0 {
		layout = f.pinned
	}
	m, bodies := f.render(layout)
	if f.forge != nil {
		f.forge(&m)
	}
	if r.Method == "GET" {
		f.requests = append(f.requests, strings.TrimPrefix(r.URL.RequestURI(), "/overlay/self/tree/"))
		if route == "manifest" {
			json.NewEncoder(w).Encode(m)
			return
		}
		if f.failBlob != "" && route == "blob/"+digest([]byte(f.failBlob)) {
			f.failBlob = ""
			w.WriteHeader(503)
			return
		}
		if body, ok := bodies[strings.TrimPrefix(route, "blob/")]; ok {
			io.WriteString(w, body)
			return
		}
		w.WriteHeader(404)
		return
	}
	f.puts = append(f.puts, route)
	if f.putStatus != 0 {
		w.WriteHeader(f.putStatus)
		return
	}
	files, key := f.skills, ""
	for candidate := range f.skills {
		if route == candidate || route == layout1Path(candidate) || route == layout3Path(candidate) || route == layout4Path(candidate) {
			key = candidate
		}
	}
	if _, ok := f.notes[route]; ok {
		files, key = f.notes, route
	}
	if key == "" {
		key = route // The tests create skills on layout-2 paths only.
	} else if r.Header.Get("If-Match") != `"`+f.revisions[key]+`"` {
		w.WriteHeader(412)
		fmt.Fprint(w, `{"error":"stale"}`)
		return
	}
	body, _ := io.ReadAll(r.Body)
	next, _ := strconv.Atoi(f.revisions[key])
	files[key], f.revisions[key] = string(body), strconv.Itoa(next+1)
	w.Header().Set("Location", "/overlay/self/tree/"+route)
	w.Header().Set("ETag", `"`+f.revisions[key]+`"`)
	w.WriteHeader(204)
}

func (f *layoutFixture) takeRequests() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	requests := f.requests
	f.requests = nil
	return requests
}

func assertFile(t *testing.T, s *Syncer, name, want string) {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(s.options.Root, name))
	if err != nil || string(body) != want {
		t.Fatalf("%s: %q %v", name, body, err)
	}
}

func assertAbsent(t *testing.T, s *Syncer, name string) {
	t.Helper()
	if _, err := os.Lstat(filepath.Join(s.options.Root, name)); !os.IsNotExist(err) {
		t.Fatalf("%s should be absent: %v", name, err)
	}
}

// assertRequestLayouts checks every request asked for the layout in hand.
func assertRequestLayouts(t *testing.T, requests []string, manifest, blob string) {
	t.Helper()
	for _, request := range requests {
		route, query, _ := strings.Cut(request, "?")
		want := blob
		if route == "manifest" {
			want = manifest
		}
		if query != want {
			t.Fatalf("%s: query %q, want %q (all: %v)", route, query, want, requests)
		}
	}
}

// assertNoTreeCopies checks no conflict or rejection copy was made anywhere.
func assertNoTreeCopies(t *testing.T, s *Syncer) {
	t.Helper()
	if len(s.checkpoint.Conflicts) != 0 {
		t.Fatal("conflicts", s.checkpoint.Conflicts)
	}
	filepath.WalkDir(s.options.Root, func(name string, _ os.DirEntry, err error) error {
		if err != nil {
			t.Fatal(err)
		}
		if strings.HasSuffix(name, ".conflict.md") || strings.HasSuffix(name, ".rejected.md") {
			t.Fatal("unexpected copy", name)
		}
		return nil
	})
}

func readStatus(t *testing.T, s *Syncer) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(s.options.Root, ".sync-status"))
	if err != nil {
		t.Fatal(err)
	}
	var status struct {
		LastError string `json:"last_error"`
	}
	if err := json.Unmarshal(body, &status); err != nil {
		t.Fatal(err)
	}
	return status.LastError
}

// Where layouts 3 and 4 keep the fixture's skills, and the organization's
// plugin manifest.
const (
	orgSkill3      = "skills/acme/org/skills/review/SKILL.md"
	projectSkill3  = "skills/acme/projects/web/skills/deploy/SKILL.md"
	personalSkill3 = "skills/me/skills/notes/SKILL.md"
	orgPlugin3     = "skills/acme/org/.claude-plugin/plugin.json"
	orgSkill4      = "skills/acme/review/SKILL.md"
	projectSkill4  = "skills/acme.web/deploy/SKILL.md"
	personalSkill4 = "skills/_me/notes/SKILL.md"
	orgPlugin4     = "skills/acme/.claude-plugin/plugin.json"
)

// pluginLayouts names, for each layout that renders plugins, where it keeps
// the fixture's organization skills and manifest, and that manifest's body.
var pluginLayouts = []struct {
	layout                                        int
	orgSkill, projectSkill, orgPlugin, pluginBody string
}{
	{3, orgSkill3, projectSkill3, orgPlugin3, `{"name": "org"}`},
	{4, orgSkill4, projectSkill4, orgPlugin4, pluginBody4("acme")},
}

func assertLayout(t *testing.T, s *Syncer, want int) {
	t.Helper()
	if got := s.checkpoint.Manifest.layout(); got != want {
		t.Fatalf("checkpoint layout %d, want %d", got, want)
	}
}

// assertLayout3 checks the plugin shape: skills and manifests in each plugin
// root, cards only above them, nothing left where layouts 1 and 2 kept skills.
func assertLayout3(t *testing.T, s *Syncer, principal string) {
	t.Helper()
	assertFile(t, s, orgSkill3, "org skill")
	assertFile(t, s, projectSkill3, "project skill")
	assertFile(t, s, orgPlugin3, `{"name": "org"}`)
	assertFile(t, s, "skills/acme/projects/web/.claude-plugin/plugin.json", `{"name": "web"}`)
	assertFile(t, s, "skills/INDEX.md", "skills index")
	assertFile(t, s, "skills/acme/INDEX.md", "acme skills")
	assertAbsent(t, s, "skills/acme/org/INDEX.md")
	assertAbsent(t, s, "skills/acme/org/skills/INDEX.md")
	assertAbsent(t, s, "skills/acme/review.md")
	assertAbsent(t, s, "acme/skills")
	assertAbsent(t, s, "acme/projects/web/skills")
	assertAbsent(t, s, "me")
	if principal == "person" {
		assertFile(t, s, personalSkill3, "personal skill")
		assertFile(t, s, "skills/me/.claude-plugin/plugin.json", `{"name": "me"}`)
	} else {
		assertAbsent(t, s, "skills/me")
	}
	assertLayout(t, s, 3)
}

// assertLayout4 checks the flat plugin shape: one folder per scope holding its
// card, manifest and skills, and nothing left where layouts 1 to 3 kept them.
func assertLayout4(t *testing.T, s *Syncer, principal string) {
	t.Helper()
	assertFile(t, s, orgSkill4, "org skill")
	assertFile(t, s, projectSkill4, "project skill")
	assertFile(t, s, orgPlugin4, pluginBody4("acme"))
	assertFile(t, s, "skills/acme.web/.claude-plugin/plugin.json", pluginBody4("acme.web"))
	assertFile(t, s, "skills/INDEX.md", "skills index")
	assertFile(t, s, "skills/acme/INDEX.md", "acme plugin")
	assertFile(t, s, "skills/acme.web/INDEX.md", "acme.web plugin")
	for _, old := range []string{"skills/acme/review/INDEX.md", "skills/acme/org", "skills/acme/projects", "skills/me",
		"skills/acme/review.md", "acme/skills", "acme/projects/web/skills", "me"} {
		assertAbsent(t, s, old)
	}
	if principal == "person" {
		assertFile(t, s, personalSkill4, "personal skill")
		assertFile(t, s, "skills/_me/.claude-plugin/plugin.json", pluginBody4("_me"))
	} else {
		assertAbsent(t, s, "skills/_me")
	}
	assertLayout(t, s, 4)
}

// onLayout brings a root onto layout 2 or 3 the way an earlier connector did,
// by asking for it; the fixture answers so until the test clears f.pinned.
func onLayout(t *testing.T, f *layoutFixture, s *Syncer, layout int) {
	t.Helper()
	f.answer(layout)
	onceOK(t, s)
	switch layout {
	case 2:
		assertFile(t, s, "skills/acme/review.md", "org skill")
		assertFile(t, s, "acme/skills/INDEX.md", "moved to skills/")
	case 3:
		assertLayout3(t, s, f.principal)
	}
	assertLayout(t, s, layout)
	f.takeRequests()
}

func TestOlderServersKeepLayout1ThenSwitchTo4(t *testing.T) {
	for _, principal := range []string{"person", "node"} {
		t.Run(principal, func(t *testing.T) {
			f, s := newLayoutFixture(t, principal)
			// An older server ignores the parameter and omits the echo; one
			// that knows only layout 2 or 3 answers a layout-4 request the same way.
			for _, layouts := range []int{1, 2, 3} {
				f.layouts = layouts
				onceOK(t, s)
				assertRequestLayouts(t, f.takeRequests(), "layout=4", "")
				assertFile(t, s, "acme/skills/review.md", "org skill")
				assertFile(t, s, "acme/projects/web/skills/deploy.md", "project skill")
				assertAbsent(t, s, "skills")
				if principal == "person" {
					assertFile(t, s, "me/skills/notes.md", "personal skill")
				} else {
					assertAbsent(t, s, "me")
				}
				assertLayout(t, s, 1)
			}

			f.layouts = 4
			onceOK(t, s)
			assertRequestLayouts(t, f.takeRequests(), "layout=4", "layout=4")
			assertLayout4(t, s, principal)
			if principal == "person" {
				assertNoTreeCopies(t, s)
			}
			// The recorded layout survives a restart and still validates.
			opts := s.options
			s.Close()
			reopened, err := Open(opts)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			if reopened.checkpoint.Manifest.layout() != 4 {
				t.Fatal("layout lost on restart")
			}
		})
	}
}

// A root an earlier connector left on layout 2 or 3, such as a gateway's,
// moves to layout 4 when nothing is pending, pointers and plugins and all.
func TestEarlierLayoutRootsSwitchTo4(t *testing.T) {
	for _, from := range []int{2, 3} {
		for _, principal := range []string{"person", "node"} {
			t.Run(fmt.Sprintf("layout%d/%s", from, principal), func(t *testing.T) {
				f, s := newLayoutFixture(t, principal)
				onLayout(t, f, s, from)
				f.pinned = 0
				onceOK(t, s)
				assertRequestLayouts(t, f.takeRequests(), "layout=4", "layout=4")
				assertLayout4(t, s, principal)
				assertAbsent(t, s, "skills/acme/projects/web/deploy.md")
				assertAbsent(t, s, "skills/me/notes.md")
				if principal == "person" {
					assertNoTreeCopies(t, s)
				}
			})
		}
	}
}

// Until it switches, a layout-2 root edits its skills in place.
func TestLayout2RootEditsInPlaceUntilItSwitches(t *testing.T) {
	f, s := newLayoutFixture(t, "person")
	onLayout(t, f, s, 2)
	for _, name := range []string{"skills/acme/review.md", "skills/me/notes.md", "skills/acme/projects/web/deploy.md"} {
		info, err := os.Stat(filepath.Join(s.options.Root, name))
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatalf("%s not editable: %v %v", name, info, err)
		}
	}
	localWrite(t, s, "skills/acme/review.md", "edited org skill")
	localWrite(t, s, "skills/me/notes.md", "edited personal skill")
	localWrite(t, s, "skills/acme/fresh.md", "new org skill")
	onceOK(t, s)
	sort.Strings(f.puts)
	if strings.Join(f.puts, ",") != "skills/acme/fresh.md,skills/acme/review.md,skills/me/notes.md" {
		t.Fatal(f.puts)
	}
	if f.skills["skills/acme/review.md"] != "edited org skill" || f.skills["skills/me/notes.md"] != "edited personal skill" || f.skills["skills/acme/fresh.md"] != "new org skill" {
		t.Fatal(f.skills)
	}
	assertNoTreeCopies(t, s)
	f.pinned = 0
	onceOK(t, s)
	assertFile(t, s, orgSkill4, "edited org skill")
	assertFile(t, s, personalSkill4, "edited personal skill")
	assertFile(t, s, "skills/acme/fresh/SKILL.md", "new org skill")
	assertAbsent(t, s, "skills/acme/fresh.md")
	assertNoTreeCopies(t, s)
}

// Layouts 3 and 4 are read-only: their files are installed so, and an edit to
// one is preserved locally and restored, never sent.
func TestPluginLayoutSkillsAreReadOnly(t *testing.T) {
	for _, p := range pluginLayouts {
		for _, principal := range []string{"person", "node"} {
			t.Run(fmt.Sprintf("layout%d/%s", p.layout, principal), func(t *testing.T) {
				f, s := newLayoutFixture(t, principal)
				f.answer(p.layout)
				onceOK(t, s)
				assertLayout(t, s, p.layout)
				want := os.FileMode(0400)
				if principal == "node" {
					want = 0440
				}
				for _, name := range []string{p.orgSkill, p.projectSkill, p.orgPlugin, "skills/INDEX.md"} {
					info, err := os.Stat(filepath.Join(s.options.Root, name))
					if err != nil || info.Mode().Perm() != want {
						t.Fatalf("%s: %v %v", name, info, err)
					}
				}
				if principal == "node" {
					return
				}
				for _, name := range []string{p.orgSkill, p.orgPlugin} {
					if err := os.Chmod(filepath.Join(s.options.Root, name), 0600); err != nil {
						t.Fatal(err)
					}
					localWrite(t, s, name, "do not lose")
				}
				onceOK(t, s)
				if len(f.puts) != 0 {
					t.Fatal("read-only skill uploaded", f.puts)
				}
				assertFile(t, s, p.orgSkill, "org skill")
				assertFile(t, s, p.orgPlugin, p.pluginBody)
				for _, name := range []string{p.orgSkill, p.orgPlugin} {
					copies, _ := filepath.Glob(filepath.Join(s.options.Root, strings.TrimSuffix(name, path.Ext(name))+"*.rejected.md"))
					if len(copies) != 1 {
						t.Fatal("read-only edit lost", name, copies)
					}
				}
			})
		}
	}
}

func TestLayoutScopeRefusals(t *testing.T) {
	f := &layoutFixture{principal: "person", skills: map[string]string{"skills/acme/review.md": "org skill", "skills/me/notes.md": "personal skill"}, revisions: map[string]string{}}
	rootMe := func(m *Manifest) {
		m.Directories = append(m.Directories, Directory{Path: "me", Write: json.RawMessage(`false`)})
	}
	addDirs := func(paths ...string) func(*Manifest) {
		return func(m *Manifest) {
			for _, p := range paths {
				m.Directories = append(m.Directories, Directory{Path: p, Write: json.RawMessage(`false`)})
			}
		}
	}
	addFile := func(name string, index bool) func(*Manifest) {
		return func(m *Manifest) {
			size := int64(1)
			if index {
				m.Indexes = append(m.Indexes, Entry{Path: name, SHA256: digest([]byte("x"))})
			} else {
				m.Files = append(m.Files, Entry{Path: name, SHA256: digest([]byte("x")), Size: &size})
			}
		}
	}
	// onlySkillsMe leaves skills/me the one folder besides the organization's.
	onlySkillsMe := func(m *Manifest) {
		m.Files, m.Indexes = nil, nil
		dirs := m.Directories[:0]
		for _, d := range m.Directories {
			if !strings.HasPrefix(d.Path, "skills/_me") {
				dirs = append(dirs, d)
			}
		}
		m.Directories = append(dirs, Directory{Path: "skills/me", Write: json.RawMessage(`false`)})
	}
	cases := []struct {
		name, principal string
		layout          int
		mutate          func(*Manifest)
		want            string
	}{
		{"layout 2 valid", "person", 2, func(*Manifest) {}, ""},
		{"layout 3 valid", "person", 3, func(*Manifest) {}, ""},
		{"layout 4 valid", "person", 4, func(*Manifest) {}, ""},
		{"top-level skills without echo", "person", 3, func(m *Manifest) { m.Layout = 0 }, "a second organization"},
		{"another organization's skills", "person", 3, addDirs("skills/other"), "a second organization"},
		{"root me in layout 2", "person", 2, rootMe, "root me/ is not part of layout 2"},
		{"root me in layout 3", "person", 3, rootMe, "root me/ is not part of layout 3"},
		{"root me in layout 4", "person", 4, rootMe, "root me/ is not part of layout 4"},
		{"daemon given skills/me", "node", 2, func(m *Manifest) {}, "private node path"},
		{"daemon given layout-3 skills/me", "node", 3, func(m *Manifest) {}, "private node path"},
		{"daemon given layout-4 skills/_me", "node", 4, func(m *Manifest) {}, "private node path"},
		{"daemon given skills/<org>/me", "node", 2, func(m *Manifest) {
			m.Files, m.Indexes = nil, nil
			dirs := m.Directories[:0]
			for _, d := range m.Directories {
				if d.Path != "skills/me" {
					dirs = append(dirs, d)
				}
			}
			m.Directories = append(dirs, Directory{Path: "skills/acme/me", Write: json.RawMessage(`false`)})
		}, "private node path"},
		// Layout 4 names each folder for its scope: <org>.<project> is the
		// organization's, and skills/me an organization slugged me, never a
		// person's.
		{"layout-4 project folder", "person", 4, addDirs("skills/acme.web", "skills/acme.web/deploy"), ""},
		{"another organization's layout-4 folder", "person", 4, addDirs("skills/other"), "a second organization"},
		{"another organization's layout-4 project folder", "person", 4, addDirs("skills/other.web"), "a second organization"},
		{"layout-4 skills/me for a person", "person", 4, onlySkillsMe, "a second organization"},
		{"layout-4 skills/me for a daemon", "node", 4, onlySkillsMe, "a second organization"},
		{"stray file in a layout-4 folder", "person", 4, addFile("skills/acme/notes.md", false), "unsupported skill plugin file"},
		{"stray file in a layout-4 skill", "person", 4, addFile("skills/acme/review/run.sh", false), "unsupported skill plugin file"},
		{"nested directory in a layout-4 skill", "person", 4, addDirs("skills/acme/review/nested"), "unsupported skill plugin directory"},
		{"index in a layout-4 skill", "person", 4, addFile("skills/acme/review/INDEX.md", true), "unsupported skill plugin file"},
		{"layout-4 skill not a slug", "person", 4, addDirs("skills/acme/Review"), "unsupported skill plugin directory"},
		{"layout-4 folder in capitals", "person", 4, addDirs("skills/Acme"), "unsupported skill plugin directory"},
		{"layout-4 folder with no project", "person", 4, addDirs("skills/acme."), "unsupported skill plugin directory"},
		{"layout-4 folder with two dots", "person", 4, addDirs("skills/acme.web.x"), "unsupported skill plugin directory"},
		{"layout-4 folder of another person", "person", 4, addDirs("skills/_you"), "unsupported skill plugin directory"},
		{"unknown layout", "person", 3, func(m *Manifest) { m.Layout = 5 }, "unsupported manifest layout"},
		{"skills as organization slug", "person", 1, func(m *Manifest) { m.Organization.Slug = "skills" }, "invalid manifest organization"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m, _ := f.render(c.layout)
			m.Principal = c.principal
			c.mutate(&m)
			_, err := m.validate(c.principal, "")
			if c.want == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("got %v, want %q", err, c.want)
			}
		})
	}
	// A legacy manifest naming no organization cannot bind a root to skills/.
	legacy := Manifest{Principal: "person", Directories: []Directory{{Path: "", Write: json.RawMessage(`false`)}, {Path: "skills", Write: json.RawMessage(`false`)}}}
	legacy.AsOf.Cursors, legacy.AsOf.RenderedAt = json.RawMessage(`{}`), "now"
	if _, err := legacy.validate("person", ""); !Terminal(err) {
		t.Fatalf("legacy skills root bound: %v", err)
	}
	if validOrganizationSlug("skills") || validOrganizationSlug("me") || !validOrganizationSlug("acme") {
		t.Fatal("reserved slugs")
	}
}

// A layout-4 skill's folder sits at its plugin's root, where Claude Code loads
// a folder of one of these names as another part of the plugin: each refuses
// the manifest, as a folder and as a skill file.
func TestLayout4RefusesPluginPartSkills(t *testing.T) {
	want := []string{"agents", "bin", "commands", "hooks", "monitors", "output-styles", "skills", "themes", "workflows"}
	if len(pluginParts) != len(want) {
		t.Fatalf("pluginParts holds %d names, want %d", len(pluginParts), len(want))
	}
	f := &layoutFixture{principal: "person", skills: map[string]string{"skills/acme/review.md": "org skill"}, revisions: map[string]string{}}
	size := int64(1)
	for _, part := range want {
		if !pluginParts[part] {
			t.Fatalf("pluginParts lacks %q", part)
		}
		for _, folder := range []string{"skills/acme/", "skills/_me/", "skills/acme.web/"} {
			m, _ := f.render(4)
			m.Directories = append(m.Directories,
				Directory{Path: "skills/acme.web", Write: json.RawMessage(`false`)},
				Directory{Path: folder + part, Write: json.RawMessage(`false`)})
			if _, err := m.validate("person", ""); err == nil || !strings.Contains(err.Error(), "unsupported skill plugin directory") {
				t.Errorf("%s%s as a folder: got %v", folder, part, err)
			}
		}
		if skillPluginPath4("skills/acme/"+part+"/SKILL.md", false) {
			t.Errorf("skills/acme/%s/SKILL.md accepted as a skill file", part)
		}
		m, _ := f.render(4)
		m.Files = append(m.Files, Entry{Path: "skills/acme/" + part + "/SKILL.md", SHA256: digest([]byte("x")), Size: &size})
		if _, err := m.validate("person", ""); err == nil || !strings.Contains(err.Error(), "unsupported skill plugin file") {
			t.Errorf("skills/acme/%s/SKILL.md: got %v", part, err)
		}
	}
	// Control: a slug that only begins with one is a skill.
	if !skillPluginPath4("skills/acme/agents-handbook/SKILL.md", false) || !skillPluginPath4("skills/acme/agents-handbook", true) {
		t.Fatal("agents-handbook refused")
	}
}

// In layout 4 skills/me is the folder of an organization slugged me, so a
// node tree bound to that organization holds it. The connector reserves the
// slug me when binding a root (organization.go), so this checks the scope rule
// alone, with the binding given and no organization in the manifest.
func TestLayout4SkillsMeIsAnOrganization(t *testing.T) {
	m := Manifest{Principal: "node", Layout: 4}
	m.AsOf.Cursors, m.AsOf.RenderedAt = json.RawMessage(`{}`), "now"
	for _, dir := range []string{"", "skills", "skills/me", "skills/me/.claude-plugin", "skills/me/review", "skills/me.web", "skills/me.web/deploy"} {
		m.Directories = append(m.Directories, Directory{Path: dir, Write: json.RawMessage(`false`)})
	}
	size := int64(1)
	for _, name := range []string{"skills/me/.claude-plugin/plugin.json", "skills/me/review/SKILL.md", "skills/me.web/deploy/SKILL.md"} {
		m.Files = append(m.Files, Entry{Path: name, SHA256: digest([]byte("x")), Size: &size})
	}
	m.Indexes = append(m.Indexes, Entry{Path: "skills/me/INDEX.md", SHA256: digest([]byte("x"))})
	if org, err := m.validate("node", "me"); err != nil || org != "me" {
		t.Fatalf("organization me's folder refused: %q %v", org, err)
	}
	// Bound to another organization, the folder is that second organization's.
	m.Directories, m.Files = m.Directories[:5], m.Files[:2]
	if _, err := m.validate("node", "acme"); err == nil || !strings.Contains(err.Error(), "a second organization") {
		t.Fatalf("got %v, want a second organization", err)
	}
	// In layout 3 the same folder is a person's.
	m.Layout = 3
	if _, err := m.validate("node", "me"); err == nil || !strings.Contains(err.Error(), "private node path") {
		t.Fatalf("got %v, want private node path", err)
	}
}

// Tamper test: a forged manifest offering a daemon a personal plugin.
func TestDaemonRefusesForgedPersonalSkills(t *testing.T) {
	for _, c := range []struct {
		layout int
		dirs   []string
		file   string
	}{
		{3, []string{"skills/me", "skills/me/skills", "skills/me/skills/secret"}, "skills/me/skills/secret/SKILL.md"},
		{4, []string{"skills/_me", "skills/_me/secret"}, "skills/_me/secret/SKILL.md"},
	} {
		t.Run(fmt.Sprintf("layout%d", c.layout), func(t *testing.T) {
			f, s := newLayoutFixture(t, "node")
			f.answer(c.layout)
			f.forge = func(m *Manifest) {
				body := "stolen"
				size := int64(len(body))
				for _, dir := range c.dirs {
					m.Directories = append(m.Directories, Directory{Path: dir, Write: json.RawMessage(`false`)})
				}
				m.Files = append(m.Files, Entry{Path: c.file, SHA256: digest([]byte(body)), Size: &size, Kind: "skill"})
			}
			err := s.Once(context.Background())
			if !Terminal(err) || !strings.Contains(err.Error(), "private node path") {
				t.Fatalf("forged personal skills accepted: %v", err)
			}
			assertAbsent(t, s, "skills")
		})
	}
}

func TestLayoutHoldsWhileEditsPendThenSwitches(t *testing.T) {
	f, s := newLayoutFixture(t, "person")
	onceOK(t, s)
	f.takeRequests()
	localWrite(t, s, "acme/skills/review.md", "edited on layout 1")
	f.layouts, f.putStatus = 4, 500
	if err := s.Once(context.Background()); err == nil {
		t.Fatal("expected failed send")
	}
	// The pending edit stays at the path it was made on; layout 4 is not applied.
	if _, pending := s.journal.pending["acme/skills/review.md"]; !pending || len(s.journal.pending) != 1 {
		t.Fatal("pending edit lost or re-pathed", s.journal.paths())
	}
	assertFile(t, s, "acme/skills/review.md", "edited on layout 1")
	assertAbsent(t, s, "skills")
	if got := f.takeRequests(); len(got) < 2 || got[0] != "manifest?layout=4" || got[1] != "manifest" {
		t.Fatal("hold should ask again for layout 1", got)
	}

	// A server-side change while holding is fetched as a layout-1 blob.
	f.skills["skills/acme/projects/web/deploy.md"] = "project skill, revised"
	f.putStatus = 0
	onceOK(t, s)
	if got := readStatus(t, s); got != "holding for layout 4: 1 pending edits" {
		t.Fatalf("status %q", got)
	}
	requests := f.takeRequests()
	assertRequestLayouts(t, requests[1:], "", "")
	assertFile(t, s, "acme/projects/web/skills/deploy.md", "project skill, revised")
	assertAbsent(t, s, "skills")
	for _, put := range f.puts {
		if put != "acme/skills/review.md" {
			t.Fatal("edit re-pathed", f.puts)
		}
	}
	if f.skills["skills/acme/review.md"] != "edited on layout 1" {
		t.Fatal("edit lost", f.skills)
	}

	onceOK(t, s) // The journal is empty, so this round switches.
	if len(s.journal.pending) != 0 {
		t.Fatal(s.journal.paths())
	}
	assertFile(t, s, orgSkill4, "edited on layout 1")
	assertAbsent(t, s, "acme/skills")
	if got := readStatus(t, s); got != "" {
		t.Fatalf("status %q", got)
	}
	assertNoTreeCopies(t, s)
}

// A layout-2 root holds for layout 4 the same way, on layout 2.
func TestLayout2RootHoldsForLayout4(t *testing.T) {
	f, s := newLayoutFixture(t, "person")
	onLayout(t, f, s, 2)
	localWrite(t, s, "skills/acme/review.md", "edited on layout 2")
	f.pinned = 0
	onceOK(t, s)
	if got := readStatus(t, s); got != "holding for layout 4: 1 pending edits" {
		t.Fatalf("status %q", got)
	}
	if got := f.takeRequests(); len(got) < 2 || got[0] != "manifest?layout=4" || got[1] != "manifest?layout=2" {
		t.Fatal("hold should ask again for layout 2", got)
	}
	if strings.Join(f.puts, ",") != "skills/acme/review.md" || f.skills["skills/acme/review.md"] != "edited on layout 2" {
		t.Fatal(f.puts, f.skills)
	}
	assertLayout(t, s, 2)
	onceOK(t, s)
	assertFile(t, s, orgSkill4, "edited on layout 2")
	assertAbsent(t, s, "skills/acme/review.md")
	assertLayout(t, s, 4)
	assertNoTreeCopies(t, s)
}

// A server rolled back cannot take edits made on layout-3 paths, so a layout-3
// root holds without applying or sending until it returns; with nothing
// pending, it follows the server back.
func TestLayout3RootHoldsWhenServerRollsBack(t *testing.T) {
	f, s := newLayoutFixture(t, "person")
	f.answer(3)
	onceOK(t, s)
	localWrite(t, s, "acme/notes/plan.md", "edited on layout 3")
	f.putStatus = 500
	if err := s.Once(context.Background()); err == nil {
		t.Fatal("expected failed send")
	}
	f.layouts, f.putStatus, f.puts = 1, 0, nil
	onceOK(t, s)
	if got := readStatus(t, s); got != "holding for layout 1: 1 pending edits" {
		t.Fatalf("status %q", got)
	}
	if len(f.puts) != 0 {
		t.Fatal("sent during hold", f.puts)
	}
	assertFile(t, s, orgSkill3, "org skill")
	assertAbsent(t, s, "acme/skills")
	assertLayout(t, s, 3)
	f.layouts = 4
	onceOK(t, s)
	if strings.Join(f.puts, ",") != "acme/notes/plan.md" || f.notes["acme/notes/plan.md"] != "edited on layout 3" {
		t.Fatal(f.puts, f.notes)
	}

	f.layouts = 1
	onceOK(t, s)
	assertFile(t, s, "acme/skills/review.md", "org skill")
	assertAbsent(t, s, "skills")
	assertLayout(t, s, 1)
	assertNoTreeCopies(t, s)
}

// The same for a layout-4 root. A server rolled back to layout 3 answers a
// layout-4 request with layout 1, as the fixture does; one answering with
// layout 3 instead is held and followed the same way.
func TestLayout4RootHoldsWhenServerRollsBack(t *testing.T) {
	for _, c := range []struct {
		name     string
		rollBack func(*layoutFixture)
		back     int
	}{
		{"to layout 3", func(f *layoutFixture) { f.answer(4); f.layouts = 3 }, 1},
		{"answering layout 3", func(f *layoutFixture) { f.answer(3) }, 3},
	} {
		t.Run(c.name, func(t *testing.T) {
			f, s := newLayoutFixture(t, "person")
			f.answer(4)
			onceOK(t, s)
			localWrite(t, s, "acme/notes/plan.md", "edited on layout 4")
			f.putStatus = 500
			if err := s.Once(context.Background()); err == nil {
				t.Fatal("expected failed send")
			}
			c.rollBack(f)
			f.putStatus, f.puts = 0, nil
			onceOK(t, s)
			if got, want := readStatus(t, s), fmt.Sprintf("holding for layout %d: 1 pending edits", c.back); got != want {
				t.Fatalf("status %q, want %q", got, want)
			}
			if len(f.puts) != 0 {
				t.Fatal("sent during hold", f.puts)
			}
			assertFile(t, s, orgSkill4, "org skill")
			assertAbsent(t, s, "acme/skills")
			assertAbsent(t, s, "skills/acme/org")
			assertLayout(t, s, 4)
			f.answer(4)
			onceOK(t, s)
			if strings.Join(f.puts, ",") != "acme/notes/plan.md" || f.notes["acme/notes/plan.md"] != "edited on layout 4" {
				t.Fatal(f.puts, f.notes)
			}

			c.rollBack(f)
			onceOK(t, s)
			if c.back == 1 {
				assertFile(t, s, "acme/skills/review.md", "org skill")
				assertAbsent(t, s, "skills")
			} else {
				assertLayout3(t, s, "person")
				for _, gone := range []string{"skills/acme/review", "skills/acme/.claude-plugin", "skills/acme.web", "skills/_me"} {
					assertAbsent(t, s, gone)
				}
			}
			assertLayout(t, s, c.back)
			assertNoTreeCopies(t, s)
		})
	}
}

// A misbehaving server answering layout 4 to a layout-1 request is not applied.
func TestLayoutEchoAboveRequestRefused(t *testing.T) {
	f, s := newLayoutFixture(t, "person")
	onceOK(t, s)
	localWrite(t, s, "acme/skills/review.md", "edited on layout 1")
	f.layouts = 4
	f.forge = func(m *Manifest) {
		if m.Layout == 0 {
			*m, _ = f.render(4)
		}
	}
	if err := s.Once(context.Background()); err == nil || !strings.Contains(err.Error(), "answered layout 4 to a layout 1 request") {
		t.Fatal(err)
	}
	assertAbsent(t, s, "skills")
	assertFile(t, s, "acme/skills/review.md", "edited on layout 1")
}

// An interrupted switch to layout 4, from layout 1 or from layout 3, is
// finished by the next round.
func TestInterruptedLayoutSwitchRepairs(t *testing.T) {
	oldOrgSkill := map[int]string{1: "acme/skills/review.md", 3: orgSkill3}
	for _, from := range []int{1, 3} {
		t.Run(fmt.Sprintf("person from layout %d", from), func(t *testing.T) {
			f, s := newLayoutFixture(t, "person")
			f.answer(from)
			onceOK(t, s)
			source, err := s.options.Source(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			f.answer(4)
			f.failBlob = "acme plugin"
			m, _ := f.render(4)
			// Call the install stage directly: interruption discards its in-memory
			// state without the end-of-round checkpoint, like process death.
			if err := s.applyWritable(context.Background(), source, m, ""); err == nil {
				t.Fatal("expected interruption")
			}
			// New files are written before any old file is removed.
			assertFile(t, s, orgSkill4, "org skill")
			assertFile(t, s, oldOrgSkill[from], "org skill")
			opts := s.options
			s.Close()
			s, err = Open(opts)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			onceOK(t, s)
			assertLayout4(t, s, "person")
			if len(f.puts) != 0 || len(s.checkpoint.Conflicts) != 0 {
				t.Fatal(f.puts, s.checkpoint.Conflicts)
			}
		})
		t.Run(fmt.Sprintf("node from layout %d", from), func(t *testing.T) {
			f, s := newLayoutFixture(t, "node")
			f.answer(from)
			onceOK(t, s)
			// An old path that cannot be removed stops the round after installation.
			old := filepath.Join(s.options.Root, oldOrgSkill[from])
			if err := os.Remove(old); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink("elsewhere", old); err != nil {
				t.Fatal(err)
			}
			f.answer(4)
			if err := s.Once(context.Background()); err == nil {
				t.Fatal("expected interruption")
			}
			assertFile(t, s, orgSkill4, "org skill")
			assertFile(t, s, projectSkill4, "project skill")
			if err := os.Remove(old); err != nil {
				t.Fatal(err)
			}
			opts := s.options
			s.Close()
			s, err := Open(opts)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			onceOK(t, s)
			assertLayout4(t, s, "node")
		})
	}
}

// The daemon now installs before removing; a directory that keeps a local file
// where the manifest wants a file still ends as a preserved conflict.
func TestDaemonDirectoryToFilePreservesFinderFile(t *testing.T) {
	m := testManifest("remote")
	m.Principal = "node"
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
	base := testDir(t)
	s, err := Open(Options{Root: filepath.Join(base, "tree"), StateDir: filepath.Join(base, "state"), Principal: "node",
		GroupID: func() (int, error) { return os.Getgid(), nil },
		Source: func(context.Context) (Source, error) {
			return Source{BaseURL: server.URL, Bearer: "test", Identity: "node-org:acme", Organization: "acme", HTTP: server.Client()}, nil
		}})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	onceOK(t, s)
	if err := os.WriteFile(filepath.Join(s.options.Root, "acme/note.md/.DS_Store"), []byte("finder"), 0600); err != nil {
		t.Fatal(err)
	}
	m = testManifest("remote")
	m.Principal = "node"
	for attempt := 0; attempt < 2; attempt++ {
		onceOK(t, s)
		if len(s.checkpoint.Conflicts) != 1 || s.checkpoint.Conflicts[0] != "acme/note.md" {
			t.Fatal(s.checkpoint.Conflicts)
		}
		assertFile(t, s, "acme/note.md/.DS_Store", "finder")
		assertAbsent(t, s, "acme/note.md/child.md")
	}
}

// An interrupted switch followed by an edit on an old path: the hold leaves
// the layout-4 files already installed alone, and the switch adopts them. The
// one copy is the edited skill's stale layout-4 file, which predates the edit
// and has no baseline to prove it untouched; it is preserved, not lost.
func TestInterruptedSwitchThenHoldMakesNoCopies(t *testing.T) {
	f, s := newLayoutFixture(t, "person")
	onceOK(t, s)
	source, err := s.options.Source(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	f.layouts, f.failBlob = 4, "skills index"
	m, _ := f.render(4)
	if err := s.applyWritable(context.Background(), source, m, ""); err == nil {
		t.Fatal("expected interruption")
	}
	opts := s.options
	s.Close()
	s, err = Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	localWrite(t, s, "acme/skills/review.md", "edited on layout 1")
	onceOK(t, s)
	if got := readStatus(t, s); got != "holding for layout 4: 1 pending edits" {
		t.Fatalf("status %q", got)
	}
	assertNoTreeCopies(t, s)
	onceOK(t, s)
	assertFile(t, s, orgSkill4, "edited on layout 1")
	assertFile(t, s, personalSkill4, "personal skill")
	assertAbsent(t, s, "acme/skills")
	copies, _ := filepath.Glob(filepath.Join(s.options.Root, "skills/acme/review/SKILL-*.conflict.md"))
	if len(s.checkpoint.Conflicts) != 1 || len(copies) != 1 {
		t.Fatal(s.checkpoint.Conflicts, copies)
	}
	assertFile(t, s, strings.TrimPrefix(copies[0], s.options.Root+"/"), "org skill")
	if strings.Join(f.puts, ",") != "acme/skills/review.md" {
		t.Fatal(f.puts)
	}
}

// A save still inside the watcher's debounce window holds the switch too.
func TestSettlingSaveHoldsLayoutSwitch(t *testing.T) {
	f, s := newLayoutFixture(t, "person")
	onceOK(t, s)
	watcher, err := newTreeWatcher()
	if err != nil {
		t.Fatal(err)
	}
	defer watcher.close()
	s.watcher = watcher
	localWrite(t, s, "acme/skills/review.md", "still settling")
	watcher.due["acme/skills/review.md"] = time.Now().Add(time.Hour)
	f.layouts = 4
	onceOK(t, s)
	if got := readStatus(t, s); got != "holding for layout 4: 1 pending edits" {
		t.Fatalf("status %q", got)
	}
	assertFile(t, s, "acme/skills/review.md", "still settling")
	assertAbsent(t, s, "skills")
	delete(watcher.due, "acme/skills/review.md")
	onceOK(t, s) // captured and sent on layout 1
	onceOK(t, s) // then the switch
	assertFile(t, s, orgSkill4, "still settling")
	assertAbsent(t, s, "acme/skills/review.md")
	if f.skills["skills/acme/review.md"] != "still settling" {
		t.Fatal("edit lost", f.skills)
	}
}

func TestNotesRootIgnoresSkills(t *testing.T) {
	if notesRoot("skills/notes") || notesRoot("skills/notes/org") || notesRoot("skills/projects/projects/notes") || !notesRoot("notes/notes") {
		t.Fatal("notes roots")
	}
}

func TestIgnoredLocalExemptsOnlyPluginManifests(t *testing.T) {
	for name, want := range map[string]bool{
		"skills/me/.claude-plugin":                        false,
		"skills/me/.claude-plugin/plugin.json":            false,
		"skills/acme/org/.claude-plugin/plugin.json":      false,
		"skills/_me/.claude-plugin":                       false,
		"skills/_me/.claude-plugin/plugin.json":           false,
		"skills/acme/.claude-plugin/plugin.json":          false,
		"skills/acme.web/.claude-plugin/plugin.json":      false,
		"skills/me/.claude-plugin/notes.md":               true,
		"skills/me/.claude-plugin/plugin.json/x":          true,
		"acme/.claude-plugin/plugin.json":                 true,
		"skills/me/.claude-plugin/.plugin.json.swp":       true,
		"skills/me/skills/review/.claude-plugin/x.json":   true,
		"skills/_me/.claude-plugin/notes.md":              true,
		"skills/acme.web/.claude-plugin/.plugin.json.swp": true,
		"skills/acme.web/review/.claude-plugin/x.json":    true,
	} {
		if got := ignoredLocal(name); got != want {
			t.Errorf("ignoredLocal(%q) = %v, want %v", name, got, want)
		}
	}
}

// Layout 4's plugin manifest points a harness at the skills beside it, and at
// nothing else; earlier layouts' manifests name no skills at all.
func TestPluginManifestBodyByLayout(t *testing.T) {
	for _, c := range []struct {
		layout int
		body   string
		ok     bool
	}{
		{4, pluginBody4("acme"), true},
		{4, `{"name": "acme", "skills": "./"}`, true},
		{4, `{"name": "acme", "description": "acme skills"}`, false},
		{4, `{"name": "acme", "skills": "x"}`, false},
		{4, `{"name": "acme", "skills": "./x"}`, false},
		{4, `{"name": "acme", "skills": ["./"]}`, false},
		{4, `{"name": "acme", "skills": "./", "hooks": {}}`, false},
		{4, `{"skills": "./"}`, false},
		{3, `{"name": "org", "description": "acme skills"}`, true},
		{3, `{"name": "org", "skills": "./"}`, false},
		{2, `{"name": "org", "skills": "./"}`, false},
	} {
		err := validatePluginBody("skills/acme/.claude-plugin/plugin.json", []byte(c.body), c.layout)
		if (err == nil) != c.ok {
			t.Errorf("layout %d %s: %v", c.layout, c.body, err)
		}
	}
}

// assertPluginManifestRefused checks a refused local plugin manifest: nothing
// sent or journaled, the person's text kept beside it as a rejected copy.
func assertPluginManifestRefused(t *testing.T, f *layoutFixture, s *Syncer, name string) {
	t.Helper()
	if len(f.puts) != 0 || len(s.journal.pending) != 0 {
		t.Fatal("plugin manifest sent or journaled", f.puts, s.journal.pending)
	}
	copies, _ := filepath.Glob(filepath.Join(s.options.Root, path.Dir(name), "plugin*.rejected.md"))
	if len(copies) != 1 {
		t.Fatal("refused plugin manifest lost", copies)
	}
}

func TestPluginManifestServerOwnedEvenIfAdvertisedWritable(t *testing.T) {
	for _, p := range pluginLayouts {
		t.Run(fmt.Sprintf("layout%d", p.layout), func(t *testing.T) {
			f, s := newLayoutFixture(t, "person")
			f.answer(p.layout)
			f.forge = func(m *Manifest) {
				for i, dir := range m.Directories {
					if dir.Path == path.Dir(p.orgPlugin) {
						m.Directories[i].Write = json.RawMessage(`{"create":true,"edit":true}`)
					}
				}
			}
			onceOK(t, s)
			if err := os.Chmod(filepath.Join(s.options.Root, p.orgPlugin), 0600); err != nil {
				t.Fatal(err)
			}
			localWrite(t, s, p.orgPlugin, `{"name": "org", "skills": "./", "hooks": {}}`)
			onceOK(t, s)
			assertPluginManifestRefused(t, f, s, p.orgPlugin)
			assertFile(t, s, p.orgPlugin, p.pluginBody)
		})
	}
}

func TestHandMadePluginManifestRefusedOnLayout2(t *testing.T) {
	f, s := newLayoutFixture(t, "person")
	onLayout(t, f, s, 2)
	name := "skills/me/.claude-plugin/plugin.json"
	if err := os.MkdirAll(filepath.Join(s.options.Root, path.Dir(name)), 0700); err != nil {
		t.Fatal(err)
	}
	localWrite(t, s, name, `{"name": "me", "mcpServers": {}}`)
	onceOK(t, s)
	assertPluginManifestRefused(t, f, s, name)
	assertAbsent(t, s, name)
	assertLayout(t, s, 2)
}

func TestPluginManifestIsNeverAWritePath(t *testing.T) {
	for _, name := range []string{"skills/me/.claude-plugin/plugin.json", "skills/_me/.claude-plugin/plugin.json", "skills/acme.web/.claude-plugin/plugin.json"} {
		if writePath(name) == nil {
			t.Fatal("plugin manifest write path", name)
		}
	}
	if writePath("acme/notes/plugin.json") != nil {
		t.Fatal("plain plugin.json refused")
	}
}
