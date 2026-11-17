// Package state is the connector's on-disk memory: the bearer minted by the
// device flow, and the organization the last command named.
//
// The bearer is stored in a file. A wrong-permissions token file is a bug,
// so every secret
// in the directory is 0600, and every write goes to a temp file in the same
// directory and is renamed over the target. A crash never leaves half a token
// behind, and a token never exists at 0644 even for an instant.
//
// The directory is 0711 and status.json is 0644: the daemon writes as root and
// the person reads its status as themselves, so that one file, which carries no
// secret, is readable by its exact path. Nothing else in the directory is.
//
// The rename is atomic because the temp file and its target share a directory,
// which is a Unix guarantee. On Windows it is best effort — this targets
// macOS and Linux laptops, and a Windows connector will want a real
// replace-file call before it can make the same promise.
package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

const (
	// dirPerm is traversable but not listable: the person can open the one
	// public file by its exact path, nothing else in the directory is
	// advertised, and every secret stays unreadable by its own mode.
	dirPerm = 0o711
	// filePerm is every file's mode but one: owner-only, because the file may
	// hold a secret.
	filePerm = 0o600
	// publicFilePerm is status.json's mode alone. That file is observability
	// with no secret in it — see statusFile — and is the one thing the person
	// reads from a directory the root daemon owns.
	publicFilePerm = 0o644

	authFile  = "auth.json"
	prefsFile = "prefs.json"
)

// ErrNotLoggedIn reports that no bearer is stored. It is a state, not a
// failure: only the caller decides whether it is fatal.
var ErrNotLoggedIn = errors.New("not logged in")

// IsNotLoggedIn reports whether err means "no bearer stored".
func IsNotLoggedIn(err error) bool { return errors.Is(err, ErrNotLoggedIn) }

// HostMismatchError reports that the stored bearer belongs to a different MCP
// host than the one the command is about to talk to.
//
// It names both hosts because either one can be the mistake: a laptop signed
// in to production and pointed at a worktree, or a laptop signed in to a
// worktree and pointed at production. A message naming only the host it wanted
// leaves the reader guessing which way round they got it.
type HostMismatchError struct {
	Stored string // the origin the bearer was minted by
	Wanted string // the origin the command means to talk to
}

func (e *HostMismatchError) Error() string {
	return fmt.Sprintf("signed in to %s, not %s — run `connector login --url %s`",
		e.Stored, e.Wanted, e.Wanted)
}

// Auth is the credential half of the state: which MCP host the bearer is for,
// the opaque bearer itself, and when the server said it expires (Unix
// seconds, 0 if the server did not say).
type Auth struct {
	BaseURL     string `json:"base_url"`
	AccessToken string `json:"access_token"`
	ExpiresAt   int64  `json:"expires_at,omitempty"`
}

// Prefs is the remembered half: the organization the last command named,
// which becomes the default for the required `organization` argument. It is
// not secret, but it keeps the secrets' 0600 all the same.
type Prefs struct {
	RecentOrg string `json:"recent_org,omitempty"`
}

// Store is one state directory.
type Store struct {
	Dir               string
	writeJSONOverride func(string, any, os.FileMode) error
}

// New returns a Store over DefaultDir.
func New() *Store { return &Store{Dir: DefaultDir()} }

// DefaultDir is where state lives: $GENTILITY_STATE_DIR if set, else the
// per-OS user config directory (~/Library/Application Support on macOS,
// $XDG_CONFIG_HOME or ~/.config on Linux, %AppData% on Windows), else
// $HOME/.config as a last resort.
func DefaultDir() string {
	if dir := os.Getenv("GENTILITY_STATE_DIR"); dir != "" {
		return dir
	}
	if cfg, err := os.UserConfigDir(); err == nil {
		return filepath.Join(cfg, "gentility")
	}
	return filepath.Join(os.Getenv("HOME"), ".config", "gentility")
}

// LoadAuth reads the stored bearer. A missing file is ErrNotLoggedIn.
func (s *Store) LoadAuth() (Auth, error) {
	var auth Auth
	if err := s.readJSON(authFile, &auth); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Auth{}, ErrNotLoggedIn
		}
		return Auth{}, err
	}
	if auth.AccessToken == "" {
		return Auth{}, ErrNotLoggedIn
	}
	return auth, nil
}

// LoadAuthFor reads the stored bearer for one MCP origin, and refuses to hand
// it to any other.
//
// A bearer is minted by a host and means nothing anywhere else, so a token
// posted at the wrong host is at best a 401 and at worst a production
// credential offered to whatever a mistyped `--url` resolves to. Every command
// that talks to a host pairs auth.ResolveOrigin with this loader: the origin
// is decided once, and the bearer must agree with it.
//
// origin is compared as the normalised origin string both sides already store
// — mcp.NormalizeBaseURL's output — so scheme, host and port must all match.
func (s *Store) LoadAuthFor(origin string) (Auth, error) {
	auth, err := s.LoadAuth()
	if err != nil {
		return Auth{}, err
	}
	if auth.BaseURL != origin {
		return Auth{}, &HostMismatchError{Stored: auth.BaseURL, Wanted: origin}
	}
	return auth, nil
}

// SaveAuth writes the bearer, owner-readable only.
func (s *Store) SaveAuth(auth Auth) error { return s.writeJSON(authFile, auth, filePerm) }

// ClearAuth forgets the bearer. Forgetting what is already forgotten is not
// an error.
func (s *Store) ClearAuth() error {
	if err := os.Remove(filepath.Join(s.Dir, authFile)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("removing %s: %w", authFile, err)
	}
	return nil
}

// RecentOrg is the organization the last command named, or "" if there is
// none to remember — an unreadable prefs file is the same answer, because a
// forgotten default is a prompt, not a failure.
func (s *Store) RecentOrg() string {
	var prefs Prefs
	if err := s.readJSON(prefsFile, &prefs); err != nil {
		return ""
	}
	return prefs.RecentOrg
}

// SetRecentOrg remembers the organization for the next command's default.
func (s *Store) SetRecentOrg(org string) error {
	return s.writeJSON(prefsFile, Prefs{RecentOrg: org}, filePerm)
}

// ClearPrefs forgets what the laptop remembered about the session — today,
// the default organization. Signing out ends the session, and the next person
// at this terminal is not meant to inherit the last one's org. Forgetting
// what is already forgotten is not an error.
func (s *Store) ClearPrefs() error {
	if err := os.Remove(filepath.Join(s.Dir, prefsFile)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("removing %s: %w", prefsFile, err)
	}
	return nil
}

func (s *Store) readJSON(name string, into any) error {
	data, err := os.ReadFile(filepath.Join(s.Dir, name))
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, into); err != nil {
		return fmt.Errorf("parsing %s: %w", filepath.Join(s.Dir, name), err)
	}
	return nil
}

// writeJSON writes atomically: a temp file in the target's own directory,
// chmod'd to mode before anything lands in it, renamed over the target. The
// mode is the caller's to name, because only the caller knows whether what it
// writes is a secret.
func (s *Store) writeJSON(name string, value any, mode os.FileMode) error {
	if s.writeJSONOverride != nil {
		return s.writeJSONOverride(name, value, mode)
	}
	if err := os.MkdirAll(s.Dir, dirPerm); err != nil {
		return fmt.Errorf("creating %s: %w", s.Dir, err)
	}
	// MkdirAll on a directory that already exists says nothing about its mode,
	// and a state dir someone else created at 0755 lists every file in it to
	// anyone who asks. Set it every time; on the common path this is one cheap
	// no-op syscall.
	if err := os.Chmod(s.Dir, dirPerm); err != nil {
		return fmt.Errorf("securing %s: %w", s.Dir, err)
	}

	data, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("encoding %s: %w", name, err)
	}

	tmp, err := os.CreateTemp(s.Dir, "."+name+".*")
	if err != nil {
		return fmt.Errorf("creating a temp file in %s: %w", s.Dir, err)
	}
	defer os.Remove(tmp.Name()) // no-op once the rename has succeeded

	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return fmt.Errorf("securing %s: %w", tmp.Name(), err)
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("writing %s: %w", tmp.Name(), err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("closing %s: %w", tmp.Name(), err)
	}

	if err := os.Rename(tmp.Name(), filepath.Join(s.Dir, name)); err != nil {
		return fmt.Errorf("installing %s: %w", name, err)
	}
	return nil
}
