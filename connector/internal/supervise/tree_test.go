package supervise

import (
	"context"
	"encoding/xml"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestTreeUnits(t *testing.T) {
	u := TreeUnit{Binary: "/opt/My & Tools/connector", Home: "/home/someone", StateDir: "/home/someone/state % $x", DiscoveryDir: "/system/discovery"}
	plist, err := RenderTree("darwin", u)
	if err != nil {
		t.Fatal(err)
	}
	decoder := xml.NewDecoder(strings.NewReader(plist))
	var values []string
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if start, ok := token.(xml.StartElement); ok && start.Name.Local == "string" {
			var value string
			if err := decoder.DecodeElement(&value, &start); err != nil {
				t.Fatal(err)
			}
			values = append(values, value)
		}
	}
	joined := strings.Join(values, "\n")
	for _, want := range []string{u.Binary, u.StateDir, u.DiscoveryDir, "tree", "run", u.Home} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing %s", want)
		}
	}
	if strings.Contains(plist, "UserName") || !strings.Contains(plist, "SuccessfulExit") {
		t.Fatal(plist)
	}
	unit, err := RenderTree("linux", u)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"/opt/My & Tools/connector"`, `"/home/someone/state %% $$x"`, "Restart=on-failure", "WantedBy=default.target"} {
		if !strings.Contains(unit, want) {
			t.Fatalf("missing %q in %s", want, unit)
		}
	}
	if strings.Contains(unit, "User=") {
		t.Fatal("user unit must run as its owner")
	}
	u.StateDir = "/tmp/injected\nExecStart=bad"
	if _, err := RenderTree("linux", u); err == nil {
		t.Fatal("accepted injected unit line")
	}
}

type treeActivationRunner struct {
	calls        []string
	output       string
	bootoutError bool
}

func (r *treeActivationRunner) Run(_ context.Context, name string, args ...string) error {
	call := strings.Join(append([]string{name}, args...), " ")
	r.calls = append(r.calls, call)
	if r.bootoutError && strings.Contains(call, "bootout") {
		return errors.New("label already unloaded")
	}
	return nil
}
func (r *treeActivationRunner) Output(_ context.Context, name string, args ...string) (string, error) {
	// After a bootout, launchd no longer holds the label.
	for _, call := range r.calls {
		if strings.Contains(call, "bootout") {
			return "", errors.New("Could not find service in domain")
		}
	}
	return r.output, nil
}
func TestTreeActivationRecoversSpawnFailure(t *testing.T) {
	for _, changed := range []bool{false, true} {
		runner := &treeActivationRunner{output: "job state = spawn failed", bootoutError: true}
		if err := ActivateTree(context.Background(), runner, "darwin", 501, "/home/someone/tree.plist", changed, io.Discard); err != nil {
			t.Fatal(err)
		}
		joined := strings.Join(runner.calls, "\n")
		if !strings.Contains(joined, "bootout gui/501/ai.gentility.tree") || !strings.Contains(joined, "bootstrap gui/501 /home/someone/tree.plist") {
			t.Fatal(joined)
		}
	}
}

func TestTreeInstallRestartsLoadedStoppedJob(t *testing.T) {
	for _, goos := range []string{"darwin", "linux"} {
		runner := &treeActivationRunner{output: "state = not running"}
		if err := ActivateTree(context.Background(), runner, goos, 501, "/home/someone/tree.plist", false, io.Discard); err != nil {
			t.Fatal(err)
		}
		want := "launchctl kickstart -k gui/501/ai.gentility.tree"
		if goos == "linux" {
			want = "systemctl --user restart gentility-tree.service"
		}
		if !strings.Contains(strings.Join(runner.calls, "\n"), want) {
			t.Fatal(goos, runner.calls)
		}
	}
}
