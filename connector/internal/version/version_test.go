package version

import (
	"os"
	"strings"
	"testing"
)

func TestStringNamesBothHalvesOfThePinnedPair(t *testing.T) {
	got := String()
	want := "connector " + Connector + " (tailscale " + Tailscale + ", genscale " + Genscale + ")"
	if got != want {
		t.Fatalf("String() = %q, want %q", got, want)
	}
	if Tailscale != "v1.101.0-pre" {
		t.Fatalf("Tailscale = %q, want the tag ~/Desktop/lib/headscale/go.mod pins", Tailscale)
	}
}

func TestGenscaleIsTheTagGoModReplacesWith(t *testing.T) {
	gomod, err := os.ReadFile("../../go.mod")
	if err != nil {
		t.Fatal(err)
	}
	want := "replace tailscale.com => github.com/gentility-ai/genscale " + Genscale + "\n"
	if !strings.Contains(string(gomod), want) {
		t.Fatalf("go.mod has no line %q; Genscale and the replace tag must move together", strings.TrimSpace(want))
	}
}

func TestStringReleaseIdentity(t *testing.T) {
	oldConnector, oldCommit := Connector, Commit
	t.Cleanup(func() { Connector, Commit = oldConnector, oldCommit })
	for _, tc := range []struct {
		name, connector, commit, want string
	}{
		{"local", "dev", "", "connector dev"},
		{"desk", "0.1.391", "", "connector 0.1.391"},
		{"server", "server-d6464ff07f5d", "d6464ff07f5d012345678901234567890123456789", "connector server-d6464ff07f5d (commit d6464ff07f5d012345678901234567890123456789)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			Connector, Commit = tc.connector, tc.commit
			want := tc.want + " (tailscale " + Tailscale + ", genscale " + Genscale + ")"
			if got := String(); got != want {
				t.Fatalf("String() = %q, want %q", got, want)
			}
		})
	}
}
