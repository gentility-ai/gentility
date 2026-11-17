package tree

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gentility-ai/gentility/connector/internal/supervise"
)

type recordingRunner struct{ calls []string }

func (r *recordingRunner) Run(_ context.Context, name string, args ...string) error {
	r.calls = append(r.calls, strings.Join(append([]string{name}, args...), " "))
	return nil
}
func TestInstallAndUninstall(t *testing.T) {
	for _, platform := range []string{"darwin", "linux"} {
		t.Run(platform, func(t *testing.T) {
			dir := testDir(t)
			discovery := filepath.Join(dir, "daemon")
			if err := os.Mkdir(discovery, 0700); err != nil {
				t.Fatal(err)
			}
			if err := WriteDiscovery(discovery, discoveryFixture(t), testTrust); err != nil {
				t.Fatal(err)
			}
			runner := &recordingRunner{}
			o := InstallOptions{GOOS: platform, UID: 501, Runner: runner, Trust: testTrust, Unit: supervise.TreeUnit{Binary: filepath.Join(dir, "connector"), Home: dir, StateDir: filepath.Join(dir, "state"), DiscoveryDir: discovery}}
			if err := Install(context.Background(), o); err != nil {
				t.Fatal(err)
			}
			parent, name, _ := o.location()
			unit := filepath.Join(parent, name)
			info, err := os.Stat(unit)
			if err != nil {
				t.Fatal(err)
			}
			if info.Mode().Perm() != 0600 {
				t.Fatal(info.Mode())
			}
			if len(runner.calls) == 0 {
				t.Fatal("service not started")
			}
			runner.calls = nil
			if err := Install(context.Background(), o); err != nil {
				t.Fatal(err)
			}
			want := "kickstart -k"
			if platform == "linux" {
				want = "systemctl --user restart"
			}
			if !strings.Contains(strings.Join(runner.calls, "\n"), want) {
				t.Fatalf("unchanged install did not restart service: %v", runner.calls)
			}
			if err := Uninstall(context.Background(), o); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(unit); !os.IsNotExist(err) {
				t.Fatal("unit remains")
			}
			if _, err := os.Stat(o.Unit.StateDir); err != nil {
				t.Fatal("uninstall removed state")
			}
		})
	}
}
func TestRootRefusal(t *testing.T) {
	for _, o := range []InstallOptions{{UID: 0}, {UID: 501, SudoUser: "person"}} {
		if err := Install(context.Background(), o); err == nil {
			t.Fatal("install accepted root/sudo")
		}
		if err := Uninstall(context.Background(), o); err == nil {
			t.Fatal("uninstall accepted root/sudo")
		}
	}
	if err := RefuseRoot(501, ""); err != nil {
		t.Fatal(err)
	}
}
