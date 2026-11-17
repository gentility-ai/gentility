package contracttest

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadLocalArtifact(t *testing.T) {
	t.Setenv("CI", "")
	t.Setenv("GITHUB_ACTIONS", "")
	root := t.TempDir()
	t.Setenv("GENTILITY_CONTRACT_DIR", root)
	if err := os.WriteFile(filepath.Join(root, "case.json"), []byte("fixture"), 0644); err != nil {
		t.Fatal(err)
	}
	if got := string(Read(t, "case.json")); got != "fixture" {
		t.Fatalf("read %q", got)
	}
}

func TestReadRefusesUnsafeOrUnavailableSources(t *testing.T) {
	if mode := os.Getenv("CONTRACT_READER_TEST"); mode != "" {
		t.Setenv("GENTILITY_CONTRACT_DIR", t.TempDir())
		t.Setenv("CI", "")
		t.Setenv("GITHUB_ACTIONS", "")
		name := "missing.json"
		switch mode {
		case "ci":
			t.Setenv("CI", "true")
		case "escape":
			name = "../outside"
		}
		Read(t, name)
		return
	}
	for mode, message := range map[string]string{
		"ci": "overrides are forbidden in CI", "escape": "invalid contract path", "missing": "read published contract",
	} {
		t.Run(mode, func(t *testing.T) {
			command := exec.Command(os.Args[0], "-test.run=^TestReadRefusesUnsafeOrUnavailableSources$")
			command.Env = append(os.Environ(), "CONTRACT_READER_TEST="+mode)
			output, err := command.CombinedOutput()
			if err == nil || !strings.Contains(string(output), message) {
				t.Fatalf("expected refusal %q; got %v: %s", message, err, output)
			}
		})
	}
}
