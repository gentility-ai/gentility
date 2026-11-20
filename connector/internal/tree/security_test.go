package tree

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestSkillPluginsRefuseExecutableFiles(t *testing.T) {
	for layout, names := range map[int][]string{
		3: {
			"skills/acme/org/hooks/hooks.json", "skills/acme/org/.mcp.json",
			"skills/acme/org/.claude-plugin/extra.json", "skills/acme/org/skills/review/run.sh",
			"skills/acme/org/skills/review/nested/SKILL.md", "skills/acme/org/INDEX.md",
			"skills/acme/rogue/.claude-plugin/plugin.json",
		},
		4: {
			"skills/acme/hooks/hooks.json", "skills/acme/.mcp.json",
			"skills/acme/.claude-plugin/extra.json", "skills/acme/review/run.sh",
			"skills/acme/review/nested/SKILL.md", "skills/acme/review/INDEX.md",
			"skills/acme.web.x/.claude-plugin/plugin.json", "skills/_you/.claude-plugin/plugin.json",
		},
	} {
		for _, name := range names {
			t.Run(fmt.Sprintf("layout%d/%s", layout, name), func(t *testing.T) {
				f, s := newLayoutFixture(t, "person")
				f.answer(layout)
				f.notes[name] = "untrusted"
				if err := s.Once(context.Background()); err == nil {
					t.Fatal("accepted plugin file")
				}
				if _, err := os.Stat(filepath.Join(s.options.Root, name)); !os.IsNotExist(err) {
					t.Fatalf("untrusted file materialized: %v", err)
				}
			})
		}
	}
}

// Where each plugin layout keeps the organization's manifest, a body it
// accepts, and bodies it refuses.
var pluginManifests = []struct {
	layout        int
	name, valid   string
	active, wrong []string
}{
	{3, "skills/acme/org/.claude-plugin/plugin.json", `{"name":"review"}`,
		[]string{`{"name":"review","hooks":{"SessionStart":"run"}}`, `{"name":"review","mcpServers":{}}`},
		[]string{`{"name":"review","skills":"elsewhere"}`, `{"name":"review","skills":"./"}`,
			`{"name":"review","version":"1"}`, `{"name":{},"description":"text"}`,
			`null`, `[]`, `{"name":"review"} {"hooks":{}}`}},
	{4, "skills/acme/.claude-plugin/plugin.json", `{"name":"acme","skills":"./"}`,
		[]string{`{"name":"acme","skills":"./","hooks":{"SessionStart":"run"}}`, `{"name":"acme","skills":"./","mcpServers":{}}`},
		[]string{`{"name":"acme","skills":"elsewhere"}`, `{"name":"acme"}`, `{"name":"acme","skills":["./"]}`,
			`{"name":"acme","skills":"./","version":"1"}`, `{"name":{},"skills":"./"}`,
			`null`, `[]`, `{"name":"acme","skills":"./"} {"hooks":{}}`}},
}

func TestSkillPluginManifestOnlyDescribes(t *testing.T) {
	for _, p := range pluginManifests {
		for _, principal := range []string{"person", "node"} {
			for _, body := range append(append([]string{}, p.active...), p.wrong...) {
				t.Run(fmt.Sprintf("layout%d/%s/%s", p.layout, principal, body), func(t *testing.T) {
					f, s := newLayoutFixture(t, principal)
					f.answer(p.layout)
					f.skills = map[string]string{}
					f.notes[p.name] = body
					if err := s.Once(context.Background()); err == nil {
						t.Fatal("accepted active plugin manifest")
					}
					if _, err := os.Stat(filepath.Join(s.options.Root, p.name)); !os.IsNotExist(err) {
						t.Fatalf("untrusted manifest materialized: %v", err)
					}
				})
			}
		}
	}
}

func TestInstalledPluginManifestIsRevalidated(t *testing.T) {
	for _, p := range pluginManifests {
		for _, principal := range []string{"person", "node"} {
			t.Run(fmt.Sprintf("layout%d/%s", p.layout, principal), func(t *testing.T) {
				f, s := newLayoutFixture(t, principal)
				f.answer(p.layout)
				f.skills = map[string]string{}
				f.notes[p.name] = p.valid
				if err := s.Once(context.Background()); err != nil {
					t.Fatal(err)
				}
				assertLayout(t, s, p.layout)
				// Model a manifest installed by a version predating validation.
				body := p.active[0]
				filename := filepath.Join(s.options.Root, p.name)
				if err := os.Chmod(filename, 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filename, []byte(body), 0600); err != nil {
					t.Fatal(err)
				}
				mode := os.FileMode(0400)
				if principal == "node" {
					mode = 0440
				}
				if err := os.Chmod(filename, mode); err != nil {
					t.Fatal(err)
				}
				f.notes[p.name] = body
				s.checkpoint.Manifest, _ = f.render(p.layout)
				if err := s.Once(context.Background()); err == nil {
					t.Fatal("accepted an already-installed active plugin manifest")
				}
			})
		}
	}
}
