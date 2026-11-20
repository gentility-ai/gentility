package tree

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"math/rand/v2"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gentility-ai/gentility/connector/internal/version"
	"golang.org/x/sys/unix"
)

// Source supplies a verified hop and credential. User discovery and daemon
// enrollment remain outside the syncer. Identity excludes the bearer itself.
type Source struct {
	BaseURL        string
	Bearer         string
	Identity       string
	Organization   string
	OrganizationID string
	HTTP           *http.Client
}
type Options struct {
	Root      string
	StateDir  string
	Principal string
	Source    func(context.Context) (Source, error)
	Log       func(string, ...any)
	Random    func() float64
	Wait      func(context.Context, time.Duration) error
	// GroupID overrides the daemon group lookup for unprivileged fixtures.
	GroupID func() (int, error)
}
type checkpoint struct {
	OrganizationID     string    `json:"organization_id,omitempty"`
	Move               *rootMove `json:"move,omitempty"`
	InventoryVersion   int       `json:"inventory_version"`
	Managed            []string  `json:"managed,omitempty"`
	ManagedDirectories []string  `json:"managed_directories,omitempty"`
	Identity           string    `json:"identity"`
	Organization       string    `json:"organization"`
	ETag               string    `json:"etag"`
	Manifest           Manifest  `json:"manifest"`
	SyncedAt           time.Time `json:"synced_at"`
	Conflicts          []string  `json:"conflicts,omitempty"`
	Failure            string    `json:"failure,omitempty"`
}
type Syncer struct {
	options        Options
	root, state    *directory
	lock           *os.File
	checkpoint     checkpoint
	conditional    bool
	journal        *writeJournal
	warnedPolicy   bool
	watcher        *treeWatcher
	cleanFiles     map[string]cleanFile
	finishedWrites []string
	// holding is this round's status while a layout switch waits for edits.
	holding string
}

func Open(options Options) (*Syncer, error) {
	if options.Principal != "person" && options.Principal != "node" {
		return nil, errors.New("tree: invalid mode")
	}
	root, err := openDirectory(options.Root)
	if err != nil {
		return nil, err
	}
	state, err := openDirectory(options.StateDir)
	if err != nil {
		root.close()
		return nil, err
	}
	lock, err := state.lock()
	if err != nil {
		state.close()
		root.close()
		return nil, err
	}

	root.mode = 0700
	if options.Principal == "node" {
		lookup := options.GroupID
		if lookup == nil {
			lookup = daemonGroupID
		}
		gid, e := lookup()
		if e != nil {
			lock.Close()
			root.close()
			state.close()
			return nil, e
		}
		root.mode, root.gid = 0750, &gid
	}
	for _, dir := range []*directory{root, state} {
		mode := dir.mode
		if mode == 0 {
			mode = 0700
		}
		if _, err := setPermissions(dir.fd, mode, dir.gid); err != nil {
			lock.Close()
			root.close()
			state.close()
			return nil, err
		}
	}
	s := &Syncer{options: options, root: root, state: state, lock: lock}
	body, err := state.read("sync.json", 4*maxManifest)
	if err == nil {
		if err = json.Unmarshal(body, &s.checkpoint); err != nil {
			s.Close()
			return nil, fmt.Errorf("tree: corrupt sync state; preserve it and reset deliberately: %w", err)
		}
		if s.checkpoint.Manifest.Principal != "" {
			if _, err = s.checkpoint.Manifest.validate(options.Principal, s.checkpoint.Organization); err != nil {
				s.Close()
				return nil, err
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		s.Close()
		return nil, err
	}
	for _, name := range append(append([]string{}, s.checkpoint.Managed...), s.checkpoint.ManagedDirectories...) {
		if strings.HasPrefix(name, ".tree-") {
			s.Close()
			return nil, errors.New("tree: reserved path in saved inventory")
		}
		if err := safePath(name, false); err != nil {
			s.Close()
			return nil, err
		}
	}
	if err := root.cleanTemps(); err != nil {
		s.Close()
		return nil, err
	}
	return s, nil
}
func (s *Syncer) Close() {
	if s.journal != nil {
		s.journal.dir.close()
	}
	s.lock.Close()
	s.state.close()
	s.root.close()
}
func digest(body []byte) string { sum := sha256.Sum256(body); return hex.EncodeToString(sum[:]) }
func (s *Syncer) save() error {
	body, err := json.Marshal(s.checkpoint)
	if err != nil {
		return err
	}
	return s.state.replace("sync.json", body, 0600)
}

type HTTPError struct{ Status int }

func (e *HTTPError) Error() string {
	switch e.Status {
	case http.StatusUnauthorized:
		return "tree: server returned HTTP 401; renew connector login (or daemon enrollment), then restart tree synchronization"
	case http.StatusForbidden:
		return "tree: server returned HTTP 403; check organization access and machine ownership for daemon mode, then restart tree synchronization"
	default:
		return fmt.Sprintf("tree: server returned HTTP %d", e.Status)
	}
}

// Terminal distinguishes refusals and unsafe local state from retryable failures.
func Terminal(err error) bool {
	var response *HTTPError
	if errors.As(err, &response) {
		return response.Status == 401 || response.Status == 403
	}
	var refusal *Refusal
	return errors.As(err, &refusal) || errors.Is(err, unix.ELOOP)
}

// Refusal marks a deliberate stop which requires operator intervention.
type Refusal struct{ Message string }

func (e *Refusal) Error() string              { return e.Message }
func refuse(format string, args ...any) error { return &Refusal{fmt.Sprintf(format, args...)} }

type transportError struct{ message string }

func (e *transportError) Error() string { return e.message }
func get(ctx context.Context, source Source, route, etag string, limit int64) ([]byte, string, int, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(source.BaseURL, "/")+"/overlay/self/tree/"+route, nil)
	if err != nil {
		return nil, "", 0, errors.New("tree: invalid gateway URL")
	}
	req.Header.Set("Authorization", "Bearer "+source.Bearer)
	req.Header.Set("X-Node-Capver", strconv.Itoa(version.NodeCapver))
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}
	client := http.Client{}
	if source.HTTP != nil {
		client = *source.HTTP
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := client.Do(req)
	if err != nil {
		return nil, "", 0, &transportError{"tree: gateway request failed"}
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotModified {
		return nil, etag, response.StatusCode, nil
	}
	if response.StatusCode != http.StatusOK {
		return nil, "", response.StatusCode, &HTTPError{response.StatusCode}
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil {
		return nil, "", response.StatusCode, &transportError{"tree: response read failed"}
	}
	if int64(len(body)) > limit {
		return nil, "", response.StatusCode, errors.New("tree: response exceeds limit")
	}
	return body, response.Header.Get("ETag"), response.StatusCode, nil
}

// Once does one reconciliation. A checkpoint describes only a fully applied
// manifest; a partial failure repeats safely and keeps the old removal inventory.
func (s *Syncer) Once(ctx context.Context) error {
	if s.options.Principal == "person" {
		return s.onceWritable(ctx)
	}
	return s.onceReadOnly(ctx)
}

func (s *Syncer) onceReadOnly(ctx context.Context) error {
	source, err := s.options.Source(ctx)
	if err != nil {
		// Discovery and credential availability may change while this unit runs.
		return fmt.Errorf("tree: source unavailable: %v", err)
	}
	if err := s.checkSource(&source); err != nil {
		return err
	}
	if err := s.resumeMove(); err != nil {
		return err
	}
	org := s.checkpoint.Organization
	etag := ""
	if s.conditional {
		etag = s.checkpoint.ETag
	}
	// A node tree has no write journal, so nothing holds it on an older layout.
	body, nextETag, status, err := get(ctx, source, layoutRoute("manifest", newestLayout), etag, maxManifest)
	if err != nil {
		return err
	}
	if status == http.StatusNotModified {
		if !s.conditional {
			return errors.New("tree: unsolicited 304")
		}
		return nil
	}
	var manifest Manifest
	if err := json.Unmarshal(body, &manifest); err != nil {
		return fmt.Errorf("tree: invalid manifest: %w", err)
	}
	org, err = s.manifestOrganization(source, manifest)
	if err != nil {
		return err
	}
	if err := s.bindOrganization(source, manifest, org); err != nil {
		return err
	}
	if s.checkpoint.Organization != "" && s.checkpoint.Organization != org {
		if err := s.moveRoot(org); err != nil {
			return err
		}
	}
	// The saved inventory is ownership, not merely the latest desired manifest.
	// In particular, a collision must never become owned on the next poll.
	managed, managedDirs := map[string]bool{}, map[string]bool{}
	for _, name := range s.checkpoint.Managed {
		managed[name] = true
	}
	if s.checkpoint.InventoryVersion == 0 {
		for _, e := range s.checkpoint.Manifest.entries() {
			managed[e.Path] = true
		}
	}
	for _, name := range s.checkpoint.ManagedDirectories {
		managedDirs[name] = true
	}
	if s.checkpoint.InventoryVersion == 0 {
		for _, d := range s.checkpoint.Manifest.Directories {
			if d.Path != "" {
				managedDirs[d.Path] = true
			}
		}
	}
	dirs := map[string]bool{}
	for _, d := range manifest.Directories {
		dirs[d.Path] = true
	}
	keep, unchanged, blocked := map[string]bool{}, map[string]bool{}, map[string]bool{}
	conflicts := []string{}
	conflict := func(name string) {
		blocked[name] = true
		conflicts = append(conflicts, name)
		if s.options.Log != nil {
			s.options.Log("tree: preserving unmanaged path %q", name)
		}
	}
	for _, d := range manifest.Directories {
		if d.Path == "" {
			continue
		}
		info, e := s.root.stat(d.Path)
		if errors.Is(e, unix.ENOTDIR) {
			continue
		} // a managed file can become a parent
		if e != nil && !errors.Is(e, os.ErrNotExist) {
			return e
		}
		if e == nil && !info.IsDir() {
			if !managed[d.Path] {
				conflict(d.Path)
			}
		}
	}
	isBlocked := func(name string) bool {
		for p := range blocked {
			if name == p || strings.HasPrefix(name, p+"/") {
				return true
			}
		}
		return false
	}
	bodies := map[string][]byte{}
	var buffered int64
	mode := os.FileMode(0400)
	if s.options.Principal == "node" {
		mode = 0440
	}
	for _, entry := range manifest.entries() {
		if isBlocked(entry.Path) {
			conflict(entry.Path)
			continue
		}
		info, e := s.root.stat(entry.Path)
		if e != nil && !errors.Is(e, os.ErrNotExist) && !errors.Is(e, unix.ENOTDIR) {
			return e
		}
		if e == nil && !managed[entry.Path] && !managedDirs[entry.Path] {
			conflict(entry.Path)
			continue
		}
		if e == nil && info.IsDir() && !managedDirs[entry.Path] {
			conflict(entry.Path)
			continue
		}
		keep[entry.Path] = true
		if e == nil && info.Mode().IsRegular() {
			matched, err := s.root.matches(entry.Path, entry.SHA256, entry.Size, mode)
			if err != nil {
				return err
			}
			if matched && !pluginManifest(entry.Path) {
				unchanged[entry.Path] = true
				continue
			}
		}
		data, exists := bodies[entry.SHA256]
		if !exists {
			data, _, _, err = get(ctx, source, layoutRoute("blob/"+entry.SHA256, manifest.layout()), "", maxBlob)
			if err != nil {
				return err
			}
			if digest(data) != entry.SHA256 {
				return errors.New("tree: blob SHA-256 mismatch")
			}
			buffered += int64(len(data))
			if buffered > 256<<20 {
				return errors.New("tree: fetched content exceeds memory budget")
			}
			bodies[entry.SHA256] = data
		}
		if entry.Size != nil && int64(len(data)) != *entry.Size {
			return errors.New("tree: blob size mismatch")
		}
		if err := validatePluginBody(entry.Path, data, manifest.layout()); err != nil {
			return err
		}
	}
	// Record pending ownership before mutating visible paths. Interrupted removal
	// or installation is recovered against this same union after a restart.
	for name := range keep {
		managed[name] = true
	}
	for name := range dirs {
		if name != "" && !isBlocked(name) {
			managedDirs[name] = true
		}
	}
	s.checkpoint.InventoryVersion = 1
	s.checkpoint.Managed, s.checkpoint.ManagedDirectories = keys(managed), keys(managedDirs)
	s.checkpoint.Identity, s.checkpoint.Organization = source.Identity, org
	s.checkpoint.Conflicts = conflicts
	if err := s.save(); err != nil {
		return err
	}
	// Install new paths before removing old ones, so an interrupted change of
	// layout (or any change) leaves both, and the next sync removes the old.
	// Only a retired path standing where the manifest now wants something goes
	// first: a file becoming a directory, or a directory becoming a file.
	// Installation drops a path that collides with a local directory from keep;
	// removal still treats it as wanted, as it did when removal came first.
	wanted := maps.Clone(keep)
	// Every parent of a wanted path is a manifest directory (validate checks).
	obstructs := func(name string) bool {
		if wanted[name] || dirs[name] {
			return true
		}
		for p := parentPath(name); p != ""; p = parentPath(p) {
			if wanted[p] {
				return true
			}
		}
		return false
	}
	previous := keys(managedDirs)
	sort.Slice(previous, func(i, j int) bool { return len(previous[i]) > len(previous[j]) })
	// Remove file leaves first, then empty directories deepest-first.
	retire := func(obstructing bool) error {
		for name := range managed {
			if wanted[name] || obstructs(name) != obstructing {
				continue
			}
			info, err := s.root.stat(name)
			if errors.Is(err, unix.ENOTDIR) {
				continue
			} // a completed parent transition
			if err == nil && info.IsDir() && dirs[name] {
				continue
			}
			if err := s.root.remove(name); err != nil {
				return err
			}
		}
		for _, name := range previous {
			if dirs[name] || obstructs(name) != obstructing {
				continue
			}
			info, err := s.root.stat(name)
			if errors.Is(err, unix.ENOTDIR) || (err == nil && info.Mode().IsRegular() && wanted[name]) {
				continue
			}
			if err := s.root.prune(name); err != nil {
				return err
			}
		}
		return nil
	}
	if err := retire(true); err != nil {
		return err
	}
	for _, d := range manifest.Directories {
		if isBlocked(d.Path) {
			continue
		}
		dir, err := s.root.walk(d.Path, true)
		if err != nil {
			return err
		}
		dir.close()
	}
	for _, entry := range manifest.entries() {
		if !keep[entry.Path] || unchanged[entry.Path] {
			continue
		}
		// A directory may retain local files (for example Finder's .DS_Store)
		// after managed children are pruned. Keep it as an unmanaged collision.
		info, err := s.root.stat(entry.Path)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err == nil && info.IsDir() {
			conflict(entry.Path)
			delete(keep, entry.Path)
			continue
		}
		if err := s.root.replace(entry.Path, bodies[entry.SHA256], mode); err != nil {
			return err
		}
	}
	if err := retire(false); err != nil {
		return err
	}
	// Exclude collisions from the completed manifest so they remain unowned.
	owned := manifest
	owned.Files, owned.Indexes = nil, nil
	for _, e := range manifest.Files {
		if keep[e.Path] {
			owned.Files = append(owned.Files, e)
		}
	}
	for _, e := range manifest.Indexes {
		if keep[e.Path] {
			owned.Indexes = append(owned.Indexes, e)
		}
	}
	ownedDirs := map[string]bool{}
	for name := range dirs {
		if name != "" && !isBlocked(name) {
			ownedDirs[name] = true
		}
	}
	s.checkpoint = checkpoint{
		InventoryVersion: 1, Managed: keys(keep), ManagedDirectories: keys(ownedDirs),
		Identity: source.Identity, Organization: org, OrganizationID: s.checkpoint.OrganizationID, ETag: nextETag,
		Manifest: owned, Conflicts: conflicts, SyncedAt: time.Now().UTC(),
	}
	if err := s.save(); err != nil {
		return err
	}
	s.conditional = len(conflicts) == 0
	return nil
}
func wait(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// Run opens and attends a tree, retrying startup I/O failures as well as sync
// failures. This matters inside the daemon, where no separate unit restarts us.
func Run(ctx context.Context, options Options) error {
	pause := options.Wait
	if pause == nil {
		pause = wait
	}
	roll := options.Random
	if roll == nil {
		roll = rand.Float64
	}
	delay := time.Second
	for ctx.Err() == nil {
		syncer, err := Open(options)
		if err == nil {
			defer syncer.Close()
			return syncer.Run(ctx)
		}
		if options.Log != nil {
			options.Log("tree startup: %v", err)
		}
		if Terminal(err) {
			return err
		}
		if err := pause(ctx, delay/2+time.Duration(roll()*float64(delay/2))); err != nil {
			return nil
		}
		delay = min(delay*2, time.Minute)
	}
	return nil
}

func (s *Syncer) Run(ctx context.Context) error {
	if s.options.Principal == "person" && s.options.Wait == nil {
		watcher, err := newTreeWatcher()
		if err != nil {
			return err
		}
		s.watcher = watcher
		defer watcher.close()
	}
	roll := s.options.Random
	if roll == nil {
		roll = rand.Float64
	}
	pause := s.options.Wait
	if s.watcher != nil {
		pause = s.watcher.wait
	}
	if pause == nil {
		pause = wait
	}
	delay := time.Second
	for {
		err := s.Once(ctx)
		if ctx.Err() != nil {
			return nil
		}
		var next time.Duration
		if err != nil {
			s.conditional = false
			if s.options.Log != nil {
				s.options.Log("%v", err)
			}
			s.checkpoint.Failure = err.Error()
			if s.options.Principal == "node" && s.checkpoint.Organization != "" {
				if saveErr := s.save(); saveErr != nil && s.options.Log != nil {
					s.options.Log("tree: cannot save failure: %v", saveErr)
				}
			}
			if Terminal(err) {
				return err
			}
			next = delay/2 + time.Duration(roll()*float64(delay/2))
			delay = min(delay*2, time.Minute)
		} else {
			delay = time.Second
			next = 12*time.Second + time.Duration(roll()*float64(6*time.Second))
		}
		if s.watcher != nil {
			s.watcher.retry = err != nil
		}
		if err := pause(ctx, next); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
	}
}

func keys(values map[string]bool) []string {
	result := make([]string, 0, len(values))
	for key := range values {
		result = append(result, key)
	}
	sort.Strings(result)
	return result
}
