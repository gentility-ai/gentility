package tree

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestSkillPluginsRefuseExecutableFiles(t *testing.T) {
	for _, name := range []string{
		"skills/acme/org/hooks/hooks.json", "skills/acme/org/.mcp.json",
		"skills/acme/org/.claude-plugin/extra.json", "skills/acme/org/skills/review/run.sh",
		"skills/acme/org/skills/review/nested/SKILL.md", "skills/acme/org/INDEX.md",
		"skills/acme/rogue/.claude-plugin/plugin.json",
	} {
		t.Run(name, func(t *testing.T) {
			f, s := newLayoutFixture(t, "person")
			f.layouts = 3
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

func TestSkillPluginManifestOnlyDescribes(t *testing.T) {
	for _, principal := range []string{"person", "node"} {
		t.Run(principal, func(t *testing.T) {
			for _, body := range []string{
				`{"name":"review","hooks":{"SessionStart":"run"}}`,
				`{"name":"review","mcpServers":{}}`, `{"name":"review","skills":"elsewhere"}`,
				`{"name":"review","version":"1"}`, `{"name":{},"description":"text"}`,
				`null`, `[]`, `{"name":"review"} {"hooks":{}}`,
			} {
				t.Run(body, func(t *testing.T) {
					f, s := newLayoutFixture(t, principal)
					f.layouts = 3
					f.skills = map[string]string{}
					name := "skills/acme/org/.claude-plugin/plugin.json"
					f.notes[name] = body
					if err := s.Once(context.Background()); err == nil {
						t.Fatal("accepted active plugin manifest")
					}
					if _, err := os.Stat(filepath.Join(s.options.Root, name)); !os.IsNotExist(err) {
						t.Fatalf("untrusted manifest materialized: %v", err)
					}
				})
			}
		})
	}
}

func TestInstalledPluginManifestIsRevalidated(t *testing.T) {
	for _, principal := range []string{"person", "node"} {
		t.Run(principal, func(t *testing.T) {
			f, s := newLayoutFixture(t, principal)
			f.layouts = 3
			f.skills = map[string]string{}
			name := "skills/acme/org/.claude-plugin/plugin.json"
			f.notes[name] = `{"name":"review"}`
			if err := s.Once(context.Background()); err != nil {
				t.Fatal(err)
			}
			// Model a manifest installed by a version predating validation.
			body := `{"name":"review","hooks":{"SessionStart":"run"}}`
			filename := filepath.Join(s.options.Root, name)
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
			f.notes[name] = body
			s.checkpoint.Manifest, _ = f.render(3)
			if err := s.Once(context.Background()); err == nil {
				t.Fatal("accepted an already-installed active plugin manifest")
			}
		})
	}
}
