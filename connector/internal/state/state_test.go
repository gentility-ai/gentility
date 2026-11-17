package state

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSaveAuthWritesOwnerOnlyFileInUnlistableDir(t *testing.T) {
	s := &Store{Dir: filepath.Join(t.TempDir(), "gentility")}

	if err := s.SaveAuth(Auth{BaseURL: "https://mcp.example", AccessToken: "tok", ExpiresAt: 1}); err != nil {
		t.Fatalf("SaveAuth: %v", err)
	}

	fi, err := os.Stat(filepath.Join(s.Dir, "auth.json"))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if got := fi.Mode().Perm(); got != 0o600 {
		t.Fatalf("auth.json mode = %o, want 600", got)
	}

	di, err := os.Stat(s.Dir)
	if err != nil {
		t.Fatalf("stat dir: %v", err)
	}
	if got := di.Mode().Perm(); got != 0o711 {
		t.Fatalf("dir mode = %o, want 711", got)
	}
}

func TestLoadAuthRoundTrips(t *testing.T) {
	s := &Store{Dir: filepath.Join(t.TempDir(), "gentility")}
	want := Auth{BaseURL: "https://mcp.example", AccessToken: "tok", ExpiresAt: 42}

	if err := s.SaveAuth(want); err != nil {
		t.Fatal(err)
	}
	got, err := s.LoadAuth()
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("LoadAuth() = %+v, want %+v", got, want)
	}
}

func TestLoadAuthMissingIsErrNotLoggedIn(t *testing.T) {
	s := &Store{Dir: filepath.Join(t.TempDir(), "gentility")}

	if _, err := s.LoadAuth(); !IsNotLoggedIn(err) {
		t.Fatalf("want ErrNotLoggedIn, got %v", err)
	}
}

func TestClearAuthIsIdempotent(t *testing.T) {
	s := &Store{Dir: filepath.Join(t.TempDir(), "gentility")}

	if err := s.ClearAuth(); err != nil {
		t.Fatalf("ClearAuth on empty store: %v", err)
	}
}

func TestClearAuthRemovesAStoredBearer(t *testing.T) {
	s := &Store{Dir: filepath.Join(t.TempDir(), "gentility")}

	if err := s.SaveAuth(Auth{BaseURL: "https://mcp.example", AccessToken: "tok"}); err != nil {
		t.Fatal(err)
	}
	if err := s.ClearAuth(); err != nil {
		t.Fatalf("ClearAuth: %v", err)
	}
	if _, err := s.LoadAuth(); !IsNotLoggedIn(err) {
		t.Fatalf("after ClearAuth, LoadAuth err = %v, want ErrNotLoggedIn", err)
	}
}

func TestRecentOrgRemembersTheLastOneUsed(t *testing.T) {
	s := &Store{Dir: filepath.Join(t.TempDir(), "gentility")}

	if got := s.RecentOrg(); got != "" {
		t.Fatalf("RecentOrg on fresh store = %q, want empty", got)
	}
	if err := s.SetRecentOrg("acme"); err != nil {
		t.Fatal(err)
	}
	if got := s.RecentOrg(); got != "acme" {
		t.Fatalf("RecentOrg = %q, want acme", got)
	}
}

func TestSetRecentOrgWritesAnOwnerOnlyFile(t *testing.T) {
	s := &Store{Dir: filepath.Join(t.TempDir(), "gentility")}

	if err := s.SetRecentOrg("acme"); err != nil {
		t.Fatal(err)
	}

	fi, err := os.Stat(filepath.Join(s.Dir, "prefs.json"))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if got := fi.Mode().Perm(); got != 0o600 {
		t.Fatalf("prefs.json mode = %o, want 600", got)
	}
}

func TestSaveAuthLeavesNoTemporaryFilesBehind(t *testing.T) {
	s := &Store{Dir: filepath.Join(t.TempDir(), "gentility")}

	if err := s.SaveAuth(Auth{BaseURL: "https://mcp.example", AccessToken: "tok"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetRecentOrg("acme"); err != nil {
		t.Fatal(err)
	}

	entries, err := os.ReadDir(s.Dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if len(names) != 2 {
		t.Fatalf("state dir holds %v, want exactly auth.json and prefs.json", names)
	}
}

func TestSaveAuthOverwritesAnExistingFile(t *testing.T) {
	s := &Store{Dir: filepath.Join(t.TempDir(), "gentility")}

	if err := s.SaveAuth(Auth{BaseURL: "https://mcp.example", AccessToken: "first"}); err != nil {
		t.Fatal(err)
	}
	want := Auth{BaseURL: "https://mcp.other", AccessToken: "second", ExpiresAt: 7}
	if err := s.SaveAuth(want); err != nil {
		t.Fatal(err)
	}

	got, err := s.LoadAuth()
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("LoadAuth() = %+v, want %+v", got, want)
	}
}

func TestDefaultDirHonoursTheOverride(t *testing.T) {
	t.Setenv("GENTILITY_STATE_DIR", "/tmp/explicit")

	if got := DefaultDir(); got != "/tmp/explicit" {
		t.Fatalf("DefaultDir = %q", got)
	}
}

func TestDefaultDirFallsBackToTheUserConfigDir(t *testing.T) {
	t.Setenv("GENTILITY_STATE_DIR", "")
	t.Setenv("HOME", "/home/someone")
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("AppData", `C:\Users\someone\AppData\Roaming`)

	cfg, err := os.UserConfigDir()
	if err != nil {
		t.Skipf("no user config dir on this platform: %v", err)
	}
	if got, want := DefaultDir(), filepath.Join(cfg, "gentility"); got != want {
		t.Fatalf("DefaultDir = %q, want %q", got, want)
	}
}

func TestSaveAuthTightensAPreExistingDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "gentility")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(dir); err != nil || fi.Mode().Perm() != 0o755 {
		t.Skipf("the umask here refuses a 0755 dir (%v, %v); nothing to tighten", fi, err)
	}

	s := &Store{Dir: dir}
	if err := s.SaveAuth(Auth{BaseURL: "https://mcp.example", AccessToken: "tok"}); err != nil {
		t.Fatalf("SaveAuth: %v", err)
	}

	di, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := di.Mode().Perm(); got != 0o711 {
		t.Fatalf("dir mode = %o, want 711 — a pre-existing dir must be tightened, not left listable", got)
	}
}

func TestLoadAuthCorruptFileIsNotErrNotLoggedIn(t *testing.T) {
	s := &Store{Dir: filepath.Join(t.TempDir(), "gentility")}
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.Dir, "auth.json"), []byte("\x00not json at all{"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := s.LoadAuth()
	if err == nil {
		t.Fatal("LoadAuth on a corrupt file returned no error")
	}
	if IsNotLoggedIn(err) {
		t.Fatalf("a corrupt auth.json must not read as logged out: %v", err)
	}
}

// A bearer is minted by one host and is meaningless to another. Loading it
// for the host a command is about to talk to is what stops a connector
// pointed at a worktree from posting a production token at it.
func TestLoadAuthForReturnsTheBearerWhenTheOriginMatches(t *testing.T) {
	s := &Store{Dir: filepath.Join(t.TempDir(), "gentility")}
	want := Auth{BaseURL: "https://mcp.example", AccessToken: "tok", ExpiresAt: 42}

	if err := s.SaveAuth(want); err != nil {
		t.Fatal(err)
	}

	got, err := s.LoadAuthFor("https://mcp.example")
	if err != nil {
		t.Fatalf("LoadAuthFor: %v", err)
	}
	if got != want {
		t.Fatalf("LoadAuthFor() = %+v, want %+v", got, want)
	}
}

func TestLoadAuthForRefusesABearerMintedByAnotherHost(t *testing.T) {
	s := &Store{Dir: filepath.Join(t.TempDir(), "gentility")}
	if err := s.SaveAuth(Auth{BaseURL: "https://mcp.gentility.ai", AccessToken: "tok"}); err != nil {
		t.Fatal(err)
	}

	_, err := s.LoadAuthFor("https://mcp.localhost:9000")
	if err == nil {
		t.Fatal("a bearer for another host was handed out")
	}
	if IsNotLoggedIn(err) {
		t.Fatalf("a wrong-host bearer must not read as logged out: %v", err)
	}

	var mismatch *HostMismatchError
	if !errors.As(err, &mismatch) {
		t.Fatalf("err = %v, want a *HostMismatchError the command layer can match on", err)
	}

	// Both hosts, and the command that fixes it: a person who sees only one of
	// them cannot tell which way round the mistake is.
	message := err.Error()
	for _, want := range []string{
		"https://mcp.gentility.ai",
		"https://mcp.localhost:9000",
		"connector login --url https://mcp.localhost:9000",
	} {
		if !strings.Contains(message, want) {
			t.Fatalf("err = %q, want it to name %q", message, want)
		}
	}
}

func TestLoadAuthForOnAnEmptyStoreIsNotLoggedIn(t *testing.T) {
	s := &Store{Dir: filepath.Join(t.TempDir(), "gentility")}

	if _, err := s.LoadAuthFor("https://mcp.example"); !IsNotLoggedIn(err) {
		t.Fatalf("want ErrNotLoggedIn, got %v", err)
	}
}

func TestClearPrefsForgetsTheRememberedOrganization(t *testing.T) {
	store := &Store{Dir: t.TempDir()}

	if err := store.SetRecentOrg("acme"); err != nil {
		t.Fatalf("SetRecentOrg: %v", err)
	}
	if err := store.ClearPrefs(); err != nil {
		t.Fatalf("ClearPrefs: %v", err)
	}
	if got := store.RecentOrg(); got != "" {
		t.Fatalf("RecentOrg = %q, want empty", got)
	}
	// Forgetting what is already forgotten is not an error.
	if err := store.ClearPrefs(); err != nil {
		t.Fatalf("a second ClearPrefs: %v", err)
	}
}
