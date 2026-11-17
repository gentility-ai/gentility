package tsnode

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"tailscale.com/ipn"
)

func TestHasStateReadsTheFileTsnetPersists(t *testing.T) {
	dir := t.TempDir()

	if hasState(dir) {
		t.Fatal("a fresh directory reads as holding state")
	}
	if hasState("") {
		t.Fatal("no directory at all reads as holding state")
	}

	path := filepath.Join(dir, stateFile)
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	// tsnet creates the file before it has a node key to put in it, so an empty
	// one is a first join that did not finish, not an identity to come back on.
	if hasState(dir) {
		t.Fatalf("an empty %s reads as holding state", stateFile)
	}

	if err := os.WriteFile(path, []byte(`{"_machinekey":"…"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if !hasState(dir) {
		t.Fatalf("a written %s does not read as holding state", stateFile)
	}
}

// A directory where the state file is a directory is not a node's state — and
// must not be mistaken for one, because the answer to "has state" decides
// whether an empty auth key is sound.
func TestHasStateIgnoresSomethingThatIsNotAFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, stateFile), 0o700); err != nil {
		t.Fatal(err)
	}

	if hasState(dir) {
		t.Fatalf("a directory named %s reads as holding state", stateFile)
	}
}

func TestUpRefusesWithNoControlURL(t *testing.T) {
	_, err := Up(context.Background(), Options{AuthKey: "tskey-auth-abc", Dir: t.TempDir()})
	if err == nil {
		t.Fatal("a node with no control URL came up")
	}
	if !strings.Contains(err.Error(), "control URL") {
		t.Fatalf("the refusal does not name the control URL: %v", err)
	}
}

// A first join has nothing but the key, so an empty one is a refusal — and it
// has to be reached before tsnet is built, or a node with no way in would sit
// on the network waiting for a login that is never coming.
func TestUpRefusesAFirstJoinWithNoAuthKey(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "tailscale")

	_, err := Up(context.Background(), Options{ControlURL: "https://head.example", Dir: dir})
	if err == nil {
		t.Fatal("a first join with no auth key came up")
	}
	if !strings.Contains(err.Error(), "auth key") {
		t.Fatalf("the refusal does not name the auth key: %v", err)
	}

	// tsnet.Server.Start makes its Dir before it does anything else. That the
	// directory is still absent is the proof the guard came first and no packet
	// was sent.
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("tsnet was started before the guard refused: %v", err)
	}
}

// The rejoin the whole change is for: a state directory holding the node key a
// previous join earned is an identity, and tsnet re-presents it without any
// auth key. Getting past the guard is all this asserts — coming up needs a
// control server, which the overlay_integration tests have.
func TestUpAcceptsAnEmptyAuthKeyWhenTheDirHoldsState(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, stateFile), []byte(`{"_machinekey":"…"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := Up(ctx, Options{ControlURL: "https://head.example", Dir: dir})
	if err == nil {
		t.Fatal("a cancelled Up returned a node")
	}
	if strings.Contains(err.Error(), "auth key") {
		t.Fatalf("a rejoin from a state-bearing directory was refused for its auth key: %v", err)
	}
}

// The stream a rejoin is judged on. WatchIPNBus cannot be driven without a
// head and a netstack; the decision it feeds can, and it is the decision that
// is worth being sure of.
type fakeBus struct {
	notifications []ipn.Notify
	read          int
	err           error
}

func (b *fakeBus) Next() (ipn.Notify, error) {
	if b.read == len(b.notifications) {
		if b.err == nil {
			return ipn.Notify{}, io.EOF
		}
		return ipn.Notify{}, b.err
	}
	n := b.notifications[b.read]
	b.read++
	return n, nil
}

func inState(s ipn.State) ipn.Notify { return ipn.Notify{State: &s} }

func browseTo(url string) ipn.Notify { return ipn.Notify{BrowseToURL: &url} }

// NeedsLogin and BrowseToURL are the same event seen from two sides, and
// NotifyInitialState can deliver either first, so both have to be read as the
// backend waiting on a person nobody is going to send.
func TestNeedsLoginReadsBothTells(t *testing.T) {
	for _, want := range []struct {
		name   string
		notify ipn.Notify
		login  bool
	}{
		{"the state itself", inState(ipn.NeedsLogin), true},
		{"the URL it is waiting on", browseTo("https://head.example/register/abc"), true},
		{"a node that is up", inState(ipn.Running), false},
		{"a node still coming up", inState(ipn.Starting), false},
		{"a node that has not decided", inState(ipn.NoState), false},
		{"a notification about something else", ipn.Notify{}, false},
		{"an empty URL, which is the field being cleared", browseTo(""), false},
	} {
		if got := needsLogin(want.notify); got != want.login {
			t.Errorf("needsLogin(%s) = %v, want %v", want.name, got, want.login)
		}
	}
}

// The rejoin the whole change is for: the backend comes up, cannot continue,
// and asks for a login. awaitLogin says so at the first tell and reads no
// further, because there is nothing after it worth waiting for.
func TestAwaitLoginStopsAtTheBackendAskingForALogin(t *testing.T) {
	bus := &fakeBus{notifications: []ipn.Notify{
		inState(ipn.NoState),
		inState(ipn.NeedsLogin),
		inState(ipn.Starting),
	}}

	if !awaitLogin(bus) {
		t.Fatal("a backend asking for a login was not read as a refused rejoin")
	}
	if bus.read != 2 {
		t.Errorf("awaitLogin read %d notifications, want it to stop at the 2nd", bus.read)
	}
}

// A join that is going well is never called a refusal, however long it takes.
func TestAwaitLoginSaysNothingAboutAJoinThatComesUp(t *testing.T) {
	bus := &fakeBus{notifications: []ipn.Notify{
		inState(ipn.NoState),
		inState(ipn.Starting),
		inState(ipn.Running),
	}}

	if awaitLogin(bus) {
		t.Fatal("a node that came up was read as a refused rejoin")
	}
}

// The bus ending is the context being cancelled — Up finished, or the caller
// stopped the node — and neither is the head refusing anything.
func TestAwaitLoginTreatsTheBusEndingAsNoAnswer(t *testing.T) {
	if awaitLogin(&fakeBus{err: context.Canceled}) {
		t.Fatal("a cancelled watch was read as a refused rejoin")
	}
}

// The watcher is armed on a rejoin and only on a rejoin: a first join passes
// through NeedsLogin while the auth key is presented, so a first join that was
// merely interrupted must not come back as "re-enrol".
func TestUpDoesNotCallAnInterruptedFirstJoinARefusal(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := Up(ctx, Options{
		ControlURL: "https://head.example",
		AuthKey:    "tskey-auth-abc",
		Dir:        t.TempDir(),
	})
	if err == nil {
		t.Fatal("a cancelled Up returned a node")
	}
	if errors.Is(err, ErrRejoinRefused) {
		t.Fatalf("an interrupted first join was reported as a refused rejoin: %v", err)
	}
}

// And an interrupted rejoin is not one either: the sentinel means the head
// answered, not that the wait ended.
func TestUpDoesNotCallAnInterruptedRejoinARefusal(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, stateFile), []byte(`{"_machinekey":"…"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := Up(ctx, Options{ControlURL: "https://head.example", Dir: dir})
	if err == nil {
		t.Fatal("a cancelled Up returned a node")
	}
	if errors.Is(err, ErrRejoinRefused) {
		t.Fatalf("an interrupted rejoin was reported as a refused rejoin: %v", err)
	}
}

// The sentinel is the whole of what an operator gets — the process exits and
// the unit stays stopped — so it has to carry the remedy.
func TestErrRejoinRefusedSaysWhatToDo(t *testing.T) {
	// The remedy is named to the letter: the tool that mints the replacement,
	// the variables it comes back as, and the command that consumes them. An
	// operator reading this in a unit's log has nothing else to go on.
	for _, want := range []string{
		"identity", "stale", "pre-auth key",
		"Re-enrol", "net_machine_enroll", "OVERLAY_", "install --replace",
	} {
		if !strings.Contains(ErrRejoinRefused.Error(), want) {
			t.Errorf("ErrRejoinRefused does not say %q: %v", want, ErrRejoinRefused)
		}
	}
}

// A phone told "nothing to present" pairs again (phonenode.Rejoin), so the
// refusal has to be recognisable without reading its sentence.
func TestUpNamesAMissingIdentity(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "tailscale")

	_, err := Up(context.Background(), Options{ControlURL: "https://head.example", Dir: dir})
	if !errors.Is(err, ErrNoIdentity) {
		t.Fatalf("err = %v, want ErrNoIdentity", err)
	}
}
