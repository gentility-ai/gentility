// Package tree synchronizes the authorized shared tree and journals user writes.
package tree

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path"
	"strconv"
	"strings"
)

const maxBlob = 16 << 20
const maxManifest = 16 << 20

type Organization struct {
	ID   string `json:"id"`
	Slug string `json:"slug"`
}

type Manifest struct {
	// Layout is echoed by a server that rendered the requested layout 2 or 3.
	// An absent field is layout 1, which is all an older server knows.
	Layout       int           `json:"layout,omitempty"`
	Organization *Organization `json:"organization,omitempty"`
	Principal    string        `json:"principal"`
	AsOf         struct {
		Cursors    json.RawMessage `json:"cursors"`
		RenderedAt string          `json:"rendered_at"`
	} `json:"as_of"`
	Directories []Directory `json:"directories"`
	Files       []Entry     `json:"files"`
	Indexes     []Entry     `json:"indexes"`
}
type Directory struct {
	Path   string          `json:"path"`
	Write  json.RawMessage `json:"write"`
	Delete bool            `json:"delete"`
}
type Entry struct {
	Path     string `json:"path"`
	SHA256   string `json:"sha256"`
	Size     *int64 `json:"size,omitempty"`
	Kind     string `json:"kind,omitempty"`
	Revision string `json:"revision,omitempty"`
}

func safePath(name string, root bool) error {
	if name == "" && root {
		return nil
	}
	if name == "" || strings.HasPrefix(name, "/") || strings.ContainsAny(name, "\\:\x00\r\n") {
		return refuse("tree: unsafe path %q", name)
	}
	for _, c := range name {
		if c > 127 {
			return refuse("tree: non-ASCII path %q", name)
		}
	}
	for _, part := range strings.Split(name, "/") {
		if part == "" || part == "." || part == ".." {
			return refuse("tree: unsafe path %q", name)
		}
	}
	return nil
}

// newestLayout is the layout the connector asks for. A server that cannot
// render it answers layout 1.
const newestLayout = 3

// The layouts differ only in where skills live. Layout 1 keeps them beside
// their owner: me/skills/, <org>/skills/ and <org>/projects/<p>/skills/.
// Layout 2 gathers them under one top-level skills/: skills/me/,
// skills/<org>/ and skills/<org>/projects/<p>/, and has no root me/. Layout 3
// makes each of those sets a read-only plugin a harness loads: skills/me/,
// skills/<org>/org/ and skills/<org>/projects/<p>/, each holding only
// .claude-plugin/plugin.json and skills/<slug>/SKILL.md.
func (m Manifest) layout() int {
	if m.Layout == 2 || m.Layout == 3 {
		return m.Layout
	}
	return 1
}

// layoutRoute asks for layout 2 or 3 explicitly; no parameter is layout 1. A
// blob request carries the layout of the manifest it materialises.
func layoutRoute(route string, layout int) string {
	if layout >= 2 {
		return route + "?layout=" + strconv.Itoa(layout)
	}
	return route
}

func parentPath(name string) string {
	p := path.Dir(name)
	if p == "." {
		return ""
	}
	return p
}
func (m Manifest) validate(principal, org string) (string, error) {
	if m.Principal != principal || (principal != "person" && principal != "node") {
		return "", fmt.Errorf("tree: unexpected principal %q", m.Principal)
	}
	if len(m.AsOf.Cursors) == 0 || string(m.AsOf.Cursors) == "null" || m.AsOf.RenderedAt == "" {
		return "", fmt.Errorf("tree: missing as_of")
	}
	if m.Layout != 0 && m.Layout != 2 && m.Layout != 3 {
		return "", fmt.Errorf("tree: unsupported manifest layout %d", m.Layout)
	}
	if m.Organization != nil {
		if !validOrganizationID(m.Organization.ID) || !validOrganizationSlug(m.Organization.Slug) {
			return "", refuse("tree: invalid manifest organization")
		}
		if org != "" && org != m.Organization.Slug {
			return "", refuse("tree: manifest organization differs from binding")
		}
		org = m.Organization.Slug
	}
	dirs := map[string]bool{}
	all := map[string]bool{}
	scope := func(name string) error {
		parts := strings.Split(name, "/")
		for _, part := range parts {
			if strings.HasPrefix(part, ".tree-") {
				return fmt.Errorf("tree: reserved metadata path %q", name)
			}
		}
		if name == "" || name == "INDEX.md" {
			return nil
		}
		// Private to a person: me/ and <org>/me/ in layout 1, skills/me/ and
		// <org>/me/ in layouts 2 and 3. A node tree never holds any of them,
		// nor skills/<org>/me/, the layout-2 counterpart of <org>/me/.
		if principal == "node" && (parts[0] == "me" || len(parts) > 1 && parts[1] == "me" || parts[0] == "skills" && len(parts) > 2 && parts[2] == "me") {
			return refuse("tree: private node path refused: %q", name)
		}
		owner := parts[0]
		switch {
		case parts[0] == "me" && m.layout() >= 2:
			return refuse("tree: root me/ is not part of layout %d: %q", m.layout(), name)
		case parts[0] == "skills" && m.layout() == 1:
			// Layout 1 has no skills/; only an organization could sit there.
			return refuse("tree: a second organization on this root is refused")
		case parts[0] == "skills":
			if len(parts) == 1 || name == "skills/INDEX.md" {
				return nil
			}
			owner = parts[1]
		}
		if owner == "me" {
			return nil
		}
		if org == "" {
			org = owner
		}
		if org != owner {
			return refuse("tree: a second organization on this root is refused")
		}
		return nil
	}
	for _, d := range m.Directories {
		if err := safePath(d.Path, true); err != nil {
			return "", err
		}
		if m.layout() == 3 && !skillPluginPath(d.Path, true) {
			return "", refuse("tree: unsupported skill plugin directory %q", d.Path)
		}
		if all[strings.ToLower(d.Path)] {
			return "", fmt.Errorf("tree: duplicate path %q", d.Path)
		}
		all[strings.ToLower(d.Path)] = true
		if err := scope(d.Path); err != nil {
			return "", err
		}
		dirs[d.Path] = true
		if len(d.Write) == 0 {
			return "", fmt.Errorf("tree: missing directory write policy")
		}
	}
	if !dirs[""] || org == "" {
		return "", fmt.Errorf("tree: missing root or organization")
	}
	for _, d := range m.Directories {
		if d.Path != "" && !dirs[parentPath(d.Path)] {
			return "", fmt.Errorf("tree: missing parent directory")
		}
	}
	check := func(e Entry, index bool) error {
		if m.layout() == 3 && !skillPluginPath(e.Path, false) {
			return refuse("tree: unsupported skill plugin file %q", e.Path)
		}
		if err := safePath(e.Path, false); err != nil {
			return err
		}
		if err := scope(e.Path); err != nil {
			return err
		}
		if all[strings.ToLower(e.Path)] {
			return fmt.Errorf("tree: conflicting path %q", e.Path)
		}
		all[strings.ToLower(e.Path)] = true
		if !dirs[parentPath(e.Path)] {
			return fmt.Errorf("tree: missing file parent")
		}
		digest, err := hex.DecodeString(e.SHA256)
		if err != nil || len(digest) != 32 || strings.ToLower(e.SHA256) != e.SHA256 {
			return fmt.Errorf("tree: invalid SHA-256")
		}
		if index {
			if path.Base(e.Path) != "INDEX.md" {
				return fmt.Errorf("tree: invalid index path")
			}
		} else if e.Size == nil || *e.Size < 0 || *e.Size > maxBlob {
			return fmt.Errorf("tree: invalid file size")
		}
		return nil
	}
	for _, e := range m.Files {
		if err := check(e, false); err != nil {
			return "", err
		}
	}
	for _, e := range m.Indexes {
		if err := check(e, true); err != nil {
			return "", err
		}
	}
	return org, nil
}
func (m Manifest) entries() []Entry { return append(append([]Entry{}, m.Files...), m.Indexes...) }

// Layout 3's containers have indexes, but each plugin contains only its
// descriptive manifest and skill documents. No hooks, scripts or MCP config.
func skillPluginPath(name string, directory bool) bool {
	parts := strings.Split(name, "/")
	if parts[0] != "skills" {
		return true
	}
	parts = parts[1:]
	container := func(rest []string) bool {
		return directory && len(rest) == 0 || !directory && len(rest) == 1 && rest[0] == "INDEX.md"
	}
	if container(parts) {
		return true
	}
	if len(parts) == 0 {
		return false
	}
	if parts[0] == "me" {
		parts = parts[1:]
	} else {
		parts = parts[1:] // organization, checked separately by scope
		if container(parts) {
			return true
		}
		if len(parts) == 0 {
			return false
		}
		switch parts[0] {
		case "org":
			parts = parts[1:]
		case "projects":
			parts = parts[1:]
			if container(parts) {
				return true
			}
			if len(parts) == 0 {
				return false
			}
			parts = parts[1:] // project
		default:
			return false
		}
	}
	if directory {
		return len(parts) == 0 || len(parts) == 1 && (parts[0] == ".claude-plugin" || parts[0] == "skills") || len(parts) == 2 && parts[0] == "skills"
	}
	return len(parts) == 2 && parts[0] == ".claude-plugin" && parts[1] == "plugin.json" || len(parts) == 3 && parts[0] == "skills" && parts[2] == "SKILL.md"
}

func validatePluginBody(name string, body []byte) error {
	if !pluginManifest(name) {
		return nil
	}
	var fields map[string]any
	if err := json.Unmarshal(body, &fields); err != nil || fields == nil {
		return refuse("tree: invalid skill plugin manifest %q", name)
	}
	if value, ok := fields["name"].(string); !ok || value == "" {
		return refuse("tree: skill plugin manifest needs a name: %q", name)
	}
	for key, value := range fields {
		if key != "name" && key != "description" {
			return refuse("tree: unsupported skill plugin manifest key %q", key)
		}
		if _, ok := value.(string); !ok {
			return refuse("tree: skill plugin manifest %q must be text", key)
		}
	}
	return nil
}
