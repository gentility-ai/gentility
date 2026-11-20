package tree

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"sort"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// errLayoutHold stops a root's round before it applies an older layout's
// manifest while edits made on its own layout's paths are pending.
var errLayoutHold = errors.New("tree: holding layout while edits are pending")

type cleanFile struct {
	info os.FileInfo
	hash string
}

func unchangedFile(a, b os.FileInfo) bool {
	return os.SameFile(a, b) && a.Size() == b.Size() && a.Mode() == b.Mode() && a.ModTime() == b.ModTime()
}

func entryMap(m Manifest) map[string]Entry {
	entries := map[string]Entry{}
	for _, e := range m.entries() {
		entries[e.Path] = e
	}
	return entries
}

func (s *Syncer) fetchManifest(ctx context.Context, source Source, conditional bool, layout int) (Manifest, string, error) {
	etag := ""
	if conditional {
		etag = s.checkpoint.ETag
	}
	body, next, status, err := get(ctx, source, layoutRoute("manifest", layout), etag, maxManifest)
	if err != nil {
		return Manifest{}, "", err
	}
	if status == 304 {
		if !conditional {
			return Manifest{}, "", errors.New("tree: unsolicited 304")
		}
		return s.checkpoint.Manifest, etag, nil
	}
	var manifest Manifest
	if err := json.Unmarshal(body, &manifest); err != nil {
		return manifest, "", errors.New("tree: invalid manifest")
	}
	if manifest.layout() > layout {
		return manifest, "", fmt.Errorf("tree: server answered layout %d to a layout %d request", manifest.layout(), layout)
	}
	if _, err := s.manifestOrganization(source, manifest); err != nil {
		return manifest, "", err
	}
	for _, dir := range manifest.Directories {
		if _, valid := decodeWritePolicy(dir.Write); !valid && !s.warnedPolicy {
			s.warnedPolicy = true
			if s.options.Log != nil {
				s.options.Log("tree: unexpected directory write policy; treating it as read-only")
			}
		}
	}
	return manifest, next, nil
}

func (s *Syncer) onceWritable(ctx context.Context) (result error) {
	if len(s.checkpoint.Conflicts) > 100 {
		s.checkpoint.Conflicts = s.checkpoint.Conflicts[len(s.checkpoint.Conflicts)-100:]
	}
	s.cleanFiles = map[string]cleanFile{}
	s.finishedWrites = nil
	s.holding = ""
	defer func() {
		if result != nil {
			s.checkpoint.Failure = "tree synchronization incomplete"
			if Terminal(result) {
				s.checkpoint.Failure = "tree synchronization stopped; check login and local path safety"
			}
			var httpErr *HTTPError
			if errors.As(result, &httpErr) {
				s.checkpoint.Failure = httpErr.Error()
			}
		}
		if len(s.checkpoint.Conflicts) > 100 {
			s.checkpoint.Conflicts = s.checkpoint.Conflicts[len(s.checkpoint.Conflicts)-100:]
		}
		if err := s.save(); err != nil {
			if result == nil {
				result = err
			}
		} else if s.journal != nil {
			for _, name := range s.finishedWrites {
				if err := s.journal.remove(name); err != nil && result == nil {
					result = err
				}
			}
		}
		if err := s.writeStatus(); err != nil && result == nil {
			result = err
		}
	}()
	source, err := s.options.Source(ctx)
	if err != nil {
		return fmt.Errorf("tree: source unavailable: %v", err)
	}
	if err := s.checkSource(&source); err != nil {
		return err
	}
	if err := s.resumeMove(); err != nil {
		return err
	}
	if s.journal == nil && s.checkpoint.Identity != "" {
		s.journal, err = openWriteJournal(s.state, s.checkpoint.Identity, s.checkpoint.Organization)
		if err != nil {
			return err
		}
	}
	// Capture edits before a request that may revoke access. Pending text can then
	// be rejected locally even when no server read is permitted anymore.
	if s.journal != nil {
		if err := s.captureLocal(); err != nil {
			return err
		}
	}
	conditional := s.conditional
	if s.journal != nil {
		for _, p := range s.journal.pending {
			if p.Sending {
				conditional = false
			}
		}
	}
	manifest, etag, err := s.fetchManifest(ctx, source, conditional, newestLayout)
	if err == nil {
		manifest, etag, err = s.holdLayout(ctx, source, conditional, manifest, etag)
	}
	if errors.Is(err, errLayoutHold) {
		s.checkpoint.Failure = s.holding
		s.conditional = false
		return nil
	}
	if err != nil {
		var httpErr *HTTPError
		if errors.As(err, &httpErr) && (httpErr.Status == 401 || httpErr.Status == 403) && s.journal != nil {
			if preserveErr := s.rejectAll("access_revoked"); preserveErr != nil {
				return preserveErr
			}
		}
		return err
	}
	org, err := s.manifestOrganization(source, manifest)
	if err != nil {
		return err
	}
	if err := s.bindOrganization(source, manifest, org); err != nil {
		return err
	}
	if s.checkpoint.Organization != "" && s.checkpoint.Organization != org {
		if s.journal != nil && len(s.journal.pending) != 0 {
			s.checkpoint.Failure = "organization rename waiting for pending writes to drain"
			s.conditional = false
			return s.drainRename(ctx, source, manifest, org)
		}
		if err := s.moveRoot(org); err != nil {
			return err
		}
	}

	s.checkpoint.Identity, s.checkpoint.Organization = source.Identity, org
	if s.journal == nil {
		s.journal, err = openWriteJournal(s.state, source.Identity, org)
		if err != nil {
			return err
		}
	}
	changed, err := s.sendPending(ctx, source, manifest)
	if err != nil {
		return err
	}
	if changed {
		manifest, etag, err = s.fetchManifest(ctx, source, false, manifest.layout())
		if err != nil {
			return err
		}
		if manifest.Organization != nil && manifest.Organization.Slug != s.checkpoint.Organization {
			s.checkpoint.Failure = "organization rename waiting for pending writes to drain"
			s.conditional = false
			return s.drainRename(ctx, source, manifest, manifest.Organization.Slug)
		}
	}
	if err := s.finishPending(); err != nil {
		return err
	}
	if err := s.applyWritable(ctx, source, manifest, etag); err != nil {
		return err
	}
	if s.holding != "" {
		s.checkpoint.Failure = s.holding
	}
	return nil
}

// holdLayout keeps a root on its layout while its write journal holds edits,
// so a pending edit is sent at the path it was made on and never re-pathed
// (T-155's rule for root moves). Nothing about the hold is persisted: each
// round asks for the newest layout and decides again, so a crash cannot
// strand it.
// Offered a newer layout, a root asks again for its own and syncs on it while
// the edits drain; offered an older one (a server rolled back), it applies
// nothing until then.
func (s *Syncer) holdLayout(ctx context.Context, source Source, conditional bool, manifest Manifest, etag string) (Manifest, string, error) {
	current := s.checkpoint.Manifest.layout()
	pending := 0
	if s.journal != nil {
		pending = len(s.journal.pending)
	}
	if s.watcher != nil {
		pending += s.watcher.settling() // saved, but not yet captured
	}
	if manifest.layout() == current || pending == 0 {
		return manifest, etag, nil
	}
	s.holding = fmt.Sprintf("holding for layout %d: %d pending edits", manifest.layout(), pending)
	if manifest.layout() < current {
		return manifest, etag, errLayoutHold
	}
	return s.fetchManifest(ctx, source, conditional, current)
}

func (s *Syncer) captureLocal() error {
	if s.cleanFiles == nil {
		s.cleanFiles = map[string]cleanFile{}
	}
	old := entryMap(s.checkpoint.Manifest)
	inventory := map[string]bool{}
	for _, name := range s.checkpoint.Managed {
		inventory[name] = true
	}
	files, err := s.root.localFiles("")
	if err != nil {
		return err
	}
	present := map[string]bool{}
	for _, name := range files {
		present[name] = true
	}
	missing := map[string]bool{}
	for name := range old {
		if !present[name] && !ignoredLocal(name) {
			if _, pending := s.journal.pending[name]; !pending {
				missing[parentPath(name)] = true
			}
		}
	}
	for _, name := range files {
		if s.watcher != nil && !s.watcher.ready(name) {
			continue
		}
		if _, pending := s.journal.pending[name]; pending {
			continue
		}
		entry, managed := old[name]
		// A prior round recorded ownership before installing this path. Let the
		// fresh manifest reconcile it instead of treating it as a new create.
		if !managed && inventory[name] {
			continue
		}
		if managed {
			before, err := s.root.stat(name)
			if err != nil {
				return err
			}
			// Capture text changes only. A recovered receipt may precede the
			// rendered post (including its size and read-only mode); reconciliation
			// installs those attributes after the pending journal drains.
			same, err := s.root.matches(name, entry.SHA256, nil, before.Mode().Perm())
			if err != nil {
				return err
			}
			if same {
				after, err := s.root.stat(name)
				if err != nil {
					return err
				}
				if unchangedFile(before, after) {
					s.cleanFiles[name] = cleanFile{after, entry.SHA256}
					continue
				}
			}
		}
		// Changed or made by hand, a plugin manifest is refused before it is
		// journaled; the next round restores the server's.
		if pluginManifest(name) {
			if s.options.Log != nil {
				s.options.Log("tree: refusing a local plugin manifest %q", name)
			}
			if err := s.rejectLocal(name, "readonly_target"); err != nil {
				return err
			}
			continue
		}
		if !managed && !strings.HasSuffix(name, ".md") {
			continue
		}
		policy := s.checkpoint.Manifest.policy(name)
		if !managed && !policy.Create {
			// Unknown files outside writable note roots belong to the person. A deep
			// attempted note is rejected explicitly rather than silently ignored.
			deep := false
			for _, dir := range s.checkpoint.Manifest.Directories {
				if notesRoot(dir.Path) && strings.HasPrefix(name, dir.Path+"/") {
					p, _ := decodeWritePolicy(dir.Write)
					deep = p.Create
				}
			}
			if deep {
				if err := s.rejectLocal(name, "invalid_path"); err != nil {
					return err
				}
			}
			continue
		}
		if !managed && missing[parentPath(name)] {
			copy, err := s.root.displace(name)
			if err != nil {
				return err
			}
			s.checkpoint.Conflicts = append(s.checkpoint.Conflicts, name+" -> "+copy+" (ambiguous rename; source restored)")
			continue
		}
		if managed && !editable(entry, s.checkpoint.Manifest) {
			if err := s.rejectLocal(name, "readonly_target"); err != nil {
				return err
			}
			continue
		}
		info, err := s.root.stat(name)
		if err != nil {
			return err
		}
		if info.Size() > maxWrite {
			if err := s.rejectLocal(name, "body_too_large"); err != nil {
				return err
			}
			continue
		}
		body, err := s.root.read(name, maxWrite)
		if err != nil {
			return err
		}
		revision := ""
		if managed {
			revision = entry.Revision
			if revision == "" {
				if err := s.rejectLocal(name, "invalid_revision"); err != nil {
					return err
				}
				continue
			}
		}
		if err := s.journal.enqueue(name, revision, body, time.Now().UTC()); err != nil {
			return err
		}
	}
	return nil
}

func (s *Syncer) rejectLocal(name, code string) error {
	captured, err := s.root.displace(name)
	if err != nil {
		return err
	}
	return s.rejectCaptured(name, captured, code)
}

func (s *Syncer) rejectCaptured(name, captured, code string) error {
	copy, err := s.root.rejectionCopy(captured, name, code)
	if err != nil {
		return err
	}
	if err := s.root.remove(captured); err != nil {
		return err
	}
	s.checkpoint.Conflicts = append(s.checkpoint.Conflicts, name+" -> "+copy)
	return nil
}

func (s *Syncer) rejectAll(code string) error {
	for _, name := range s.journal.paths() {
		p := s.journal.pending[name]
		if p.Receipt != nil {
			continue
		}
		p.Rejection = code
		if err := s.journal.save(p); err != nil {
			return err
		}
	}
	return s.finishPending()
}

func (s *Syncer) sendPending(ctx context.Context, source Source, manifest Manifest) (bool, error) {
	changed := false
	entries := entryMap(manifest)
	for _, name := range s.journal.paths() {
		p := s.journal.pending[name]
		if p.Receipt != nil || p.Rejection != "" {
			changed = true
			continue
		}
		if manifest.Organization != nil && manifest.Organization.Slug != s.checkpoint.Organization && strings.HasPrefix(name, s.checkpoint.Organization+"/") {
			// drainRename owns old-path operations; they must never be resent.
			continue
		}
		if p.PostKey != "" && p.Sending {
			receipt, err := recoverPost(ctx, source, manifest, p)
			if errors.Is(err, errPostGrace) {
				continue
			}
			if errors.Is(err, errPostUnconfirmed) {
				p.Rejection = "could not confirm whether this post was created"
			} else if err != nil {
				return changed, err
			} else {
				p.Receipt = receipt
			}
			if p.Rejection != "" || p.Receipt != nil {
				if err := s.journal.save(p); err != nil {
					return changed, err
				}
				changed = true
				continue
			}
		}
		if p.Sending && p.PostKey == "" {
			if entry, ok := entries[name]; ok && entry.SHA256 == p.LocalHash {
				p.Receipt = &writeReceipt{Path: name, Revision: entry.Revision}
				if err := s.journal.save(p); err != nil {
					return changed, err
				}
				changed = true
				continue
			}
		}
		policy := manifest.policy(name)
		if p.Revision != "" {
			entry, exists := entries[name]
			if !exists {
				p.Rejection = "not_found"
			} else if !editable(entry, manifest) {
				p.Rejection = "readonly_target"
			}
		} else if !policy.Create {
			p.Rejection = "readonly_target"
		}
		if p.Rejection != "" {
			if err := s.journal.save(p); err != nil {
				return changed, err
			}
			changed = true
			continue
		}
		p.Sending = true
		p.SentAt = time.Now().UTC()
		if p.AmbiguousAt.IsZero() {
			p.AmbiguousAt = p.SentAt
		}
		if err := s.journal.save(p); err != nil {
			return changed, err
		}
		response, err := put(ctx, source, p)
		if err != nil {
			return changed, err
		}
		switch response.Status {
		case 204:
			p.Receipt = response.Receipt
			changed = true
		case 401, 403:
			if err := s.rejectAll(response.Code); err != nil {
				return changed, err
			}
			return changed, &HTTPError{response.Status}
		case 412, 413, 422:
			p.Rejection = response.Code
			changed = true
		case 404:
			fresh, _, err := s.fetchManifest(ctx, source, false, manifest.layout())
			if err != nil {
				return changed, err
			}
			if _, exists := entryMap(fresh)[name]; exists {
				return changed, &HTTPError{404}
			}
			p.Rejection = "not_found"
			changed = true
		default:
			return changed, &HTTPError{response.Status}
		}
		if err := s.journal.save(p); err != nil {
			return changed, err
		}
	}
	return changed, nil
}

func (s *Syncer) finishPending() error {
	for _, name := range s.journal.paths() {
		p := s.journal.pending[name]
		if p.Receipt == nil && p.Rejection == "" {
			continue
		}
		if p.Rejection != "" {
			copy := strings.TrimSuffix(name, ".md") + "-" + digest(p.Body)[:12] + ".rejected.md"
			body := append([]byte("<!-- Shared tree rejected this edit: "+p.Rejection+". Your text is preserved below. -->\n\n"), p.Body...)
			copy, err := s.root.preserveRejection(copy, body)
			if err != nil {
				return err
			}
			s.checkpoint.Conflicts = append(s.checkpoint.Conflicts, name+" -> "+copy)
		}
		localPath := name
		capturedPath := ""
		if p.Receipt != nil && p.Receipt.Path != name {
			if err := s.root.moveCanonical(name, p.Receipt.Path); err != nil && !errors.Is(err, os.ErrNotExist) {
				if !errors.Is(err, unix.EEXIST) {
					return err
				}
				copy, err := s.root.displace(name)
				if err != nil {
					return err
				}
				capturedPath = copy
			}
			localPath = p.Receipt.Path
		}
		if p.Rejection != "" {
			copy, err := s.root.displace(localPath)
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			capturedPath = copy
		}
		readPath := localPath
		if capturedPath != "" {
			readPath = capturedPath
		}
		info, err := s.root.stat(readPath)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		var successor []byte
		if err == nil {
			if info.Size() > maxWrite {
				// The successor itself exceeds the PUT limit (local 413).
				code := p.Rejection
				if code == "" {
					code = "body_too_large"
				}
				if capturedPath != "" {
					if err := s.rejectCaptured(localPath, capturedPath, code); err != nil {
						return err
					}
				} else if err := s.rejectLocal(readPath, code); err != nil {
					return err
				}
			} else {
				local, err := s.root.read(readPath, maxWrite)
				if err != nil {
					return err
				}
				baseline := entryMap(s.checkpoint.Manifest)[localPath]
				if baseline.Path == "" {
					baseline = entryMap(s.checkpoint.Manifest)[name]
				}
				hash := digest(local)
				if p.Receipt != nil && hash != p.LocalHash && hash != baseline.SHA256 {
					successor = local
				} else if p.Rejection != "" {
					if hash != p.LocalHash && hash != baseline.SHA256 {
						if err := s.rejectCaptured(localPath, readPath, p.Rejection); err != nil {
							return err
						}
					}
				}
			}
		}
		if capturedPath != "" {
			if successor != nil {
				s.checkpoint.Conflicts = append(s.checkpoint.Conflicts, name+" -> "+capturedPath)
			} else if err := s.root.remove(capturedPath); err != nil {
				return err
			}
		}
		if p.Receipt != nil {
			s.checkpoint.Managed = append(s.checkpoint.Managed, p.Receipt.Path)
			// Baseline is what the receipt accepted, even if a newer local edit
			// must be sent on the next round. Rendered post bytes arrive via GET.
			updated := false
			for i := range s.checkpoint.Manifest.Files {
				if s.checkpoint.Manifest.Files[i].Path == name {
					s.checkpoint.Manifest.Files[i].Path = p.Receipt.Path
					s.checkpoint.Manifest.Files[i].Revision = p.Receipt.Revision
					s.checkpoint.Manifest.Files[i].SHA256 = p.LocalHash
					size := int64(len(p.Body))
					s.checkpoint.Manifest.Files[i].Size = &size
					updated = true
				}
			}
			if !updated {
				kind := "note"
				if p.PostKey != "" {
					kind = "post"
				}
				size := int64(len(p.Body))
				s.checkpoint.Manifest.Files = append(s.checkpoint.Manifest.Files, Entry{Path: p.Receipt.Path, Revision: p.Receipt.Revision, SHA256: p.LocalHash, Kind: kind, Size: &size})
			}
		}
		if successor != nil {
			// Replace the receipt atomically with the successor; never leave a
			// crash window between retiring the receipt and saving newer bytes.
			next := pendingWrite{Path: localPath, Revision: p.Receipt.Revision, Body: successor, LocalHash: digest(successor), Started: time.Now().UTC()}
			if err := s.journal.save(next); err != nil {
				return err
			}
			if localPath != name {
				if err := s.journal.remove(name); err != nil {
					return err
				}
			}
		} else {
			s.finishedWrites = append(s.finishedWrites, name)
		}
	}
	return nil
}

func (s *Syncer) applyWritable(ctx context.Context, source Source, manifest Manifest, etag string) error {
	old := entryMap(s.checkpoint.Manifest)
	owned := map[string]bool{}
	for name := range old {
		owned[name] = true
	}
	for _, name := range s.checkpoint.Managed {
		owned[name] = true
	}
	desired := entryMap(manifest)
	// Persist the full install intent before touching files. A restart can
	// distinguish partial materialisation from locally created notes.
	inventoryChanged := false
	for name := range desired {
		if !owned[name] {
			inventoryChanged = true
		}
		owned[name] = true
	}
	if inventoryChanged {
		s.checkpoint.Managed = keys(owned)
		if err := s.save(); err != nil {
			return err
		}
	}
	// Materialize each entry after downloading its verified body. The final
	// displacement captures edits made while the network request was running.
	for _, entry := range manifest.entries() {
		if ignoredLocal(entry.Path) {
			return refuse("tree: server advertised a reserved local path")
		}
		if p, pending := s.journal.pending[entry.Path]; pending && p.Receipt == nil && p.Rejection == "" {
			continue
		}
		mode := fileMode(entry, manifest, "person")
		info, err := s.root.stat(entry.Path)
		if err != nil && !errors.Is(err, os.ErrNotExist) && !errors.Is(err, unix.ENOTDIR) {
			return err
		}
		if err == nil && info.Mode().IsRegular() && !pluginManifest(entry.Path) {
			if clean, ok := s.cleanFiles[entry.Path]; ok && clean.hash == entry.SHA256 && info.Mode().Perm() == mode && unchangedFile(clean.info, info) {
				continue
			}
			same, err := s.root.matches(entry.Path, entry.SHA256, entry.Size, mode)
			if err != nil {
				return err
			}
			if same {
				continue
			}
		}
		body, _, _, err := get(ctx, source, layoutRoute("blob/"+entry.SHA256, manifest.layout()), "", maxBlob)
		if err != nil {
			return err
		}
		if digest(body) != entry.SHA256 || entry.Size != nil && int64(len(body)) != *entry.Size {
			return errors.New("tree: blob SHA-256 or size mismatch")
		}
		if err := validatePluginBody(entry.Path, body, manifest.layout()); err != nil {
			return err
		}
		// Handle an unmanaged file at a parent path before creating directories.
		parts := strings.Split(parentPath(entry.Path), "/")
		for i := range parts {
			parent := strings.Join(parts[:i+1], "/")
			if parent == "" {
				continue // The root INDEX has no parent component to inspect.
			}
			info, err := s.root.stat(parent)
			if errors.Is(err, os.ErrNotExist) {
				break
			}
			if err != nil {
				return err
			}
			if !info.IsDir() {
				copy, err := s.root.displace(parent)
				if err != nil {
					return err
				}
				baseline, managed := old[parent]
				if managed {
					body, err := s.root.read(copy, maxBlob)
					if err != nil {
						return err
					}
					if digest(body) == baseline.SHA256 {
						if err := s.root.remove(copy); err != nil {
							return err
						}
						continue
					}
				}
				s.checkpoint.Conflicts = append(s.checkpoint.Conflicts, parent+" -> "+copy)
			}
		}
		info, err = s.root.stat(entry.Path)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err == nil {
			copy, err := s.root.displace(entry.Path)
			if err != nil {
				return err
			}
			keep := true
			if baseline, known := old[entry.Path]; info.Mode().IsRegular() {
				captured, err := s.root.read(copy, maxBlob)
				if err != nil {
					return err
				}
				keep = digest(captured) != baseline.SHA256 && digest(captured) != entry.SHA256
				if known && keep && len(captured) <= maxWrite && editable(baseline, s.checkpoint.Manifest) {
					if err := s.journal.enqueue(entry.Path, baseline.Revision, captured, time.Now().UTC()); err != nil {
						return err
					}
					s.finishedWrites = slices.DeleteFunc(s.finishedWrites, func(name string) bool { return name == entry.Path })
					if err := s.root.install(entry.Path, captured, 0600); err != nil {
						return err
					}
					s.checkpoint.Conflicts = append(s.checkpoint.Conflicts, entry.Path+" -> "+copy)
					continue
				}
			}
			if keep {
				s.checkpoint.Conflicts = append(s.checkpoint.Conflicts, entry.Path+" -> "+copy)
			} else if err := s.root.remove(copy); err != nil {
				return err
			}
		}
		owned[entry.Path] = true
		s.checkpoint.Managed = keys(owned)
		if err := s.root.install(entry.Path, body, mode); err != nil {
			return err
		}
	}
	var leftovers []string
	for name := range owned {
		isDirectory := false
		for _, dir := range manifest.Directories {
			if dir.Path == name {
				isDirectory = true
				break
			}
		}
		if isDirectory {
			continue
		}
		if _, exists := desired[name]; exists || ignoredLocal(name) {
			continue
		}
		// While held on an older layout, an owned path with no baseline is a
		// leftover of an interrupted switch. Keep owning it, untouched: the
		// switch compares it with the newer manifest. Unowned, a pointer INDEX
		// in a writable layout-1 directory would be sent as a new file.
		if _, known := old[name]; !known && s.holding != "" {
			leftovers = append(leftovers, name)
			continue
		}
		if p, pending := s.journal.pending[name]; pending && p.Receipt == nil && p.Rejection == "" {
			continue
		}
		if _, err := s.root.stat(name); errors.Is(err, os.ErrNotExist) || errors.Is(err, unix.ENOTDIR) {
			continue
		} else if err != nil {
			return err
		}
		copy, err := s.root.displace(name)
		if err != nil {
			return err
		}
		baseline, known := old[name]
		if known {
			body, err := s.root.read(copy, maxBlob)
			if err != nil {
				return err
			}
			if digest(body) == baseline.SHA256 {
				if err := s.root.remove(copy); err != nil {
					return err
				}
				continue
			}
		}
		s.checkpoint.Conflicts = append(s.checkpoint.Conflicts, name+" -> "+copy)
	}
	dirs := map[string]bool{}
	for _, dir := range manifest.Directories {
		if dir.Path == "" {
			continue
		}
		handle, err := s.root.walk(dir.Path, true)
		if err != nil {
			return err
		}
		handle.close()
		dirs[dir.Path] = true
	}
	previous := append([]string{}, s.checkpoint.ManagedDirectories...)
	sort.Slice(previous, func(i, j int) bool { return len(previous[i]) > len(previous[j]) })
	for _, name := range previous {
		if !dirs[name] {
			if err := s.root.prune(name); err != nil && !errors.Is(err, unix.ENOTDIR) {
				return err
			}
		}
	}
	// Pending paths retain their old materialized baseline and revision.
	for i, entry := range manifest.Files {
		if p, pending := s.journal.pending[entry.Path]; pending && p.Receipt == nil && p.Rejection == "" {
			if baseline, ok := old[entry.Path]; ok {
				manifest.Files[i] = baseline
			}
		}
	}
	s.checkpoint.Manifest = manifest
	s.checkpoint.ETag = etag
	s.checkpoint.Managed = leftovers
	for name := range desired {
		s.checkpoint.Managed = append(s.checkpoint.Managed, name)
	}
	s.checkpoint.ManagedDirectories = keys(dirs)
	s.checkpoint.InventoryVersion = 1
	s.checkpoint.SyncedAt = time.Now().UTC()
	s.checkpoint.Failure = ""
	for _, pending := range s.journal.pending {
		if pending.Receipt == nil && pending.Rejection == "" {
			s.checkpoint.Failure = "pending writes awaiting retry"
			break
		}
	}
	s.conditional = true
	if s.watcher != nil {
		return s.watcher.refresh(s.root, s.options.Root, manifest)
	}
	return nil
}

func (s *Syncer) writeStatus() error {
	status := struct {
		Principal   string          `json:"principal"`
		LastSuccess time.Time       `json:"last_success"`
		LastError   string          `json:"last_error,omitempty"`
		Cursors     json.RawMessage `json:"cursors"`
		Conflicts   []string        `json:"conflicts,omitempty"`
	}{s.options.Principal, s.checkpoint.SyncedAt, s.checkpoint.Failure, s.checkpoint.Manifest.AsOf.Cursors, s.checkpoint.Conflicts}
	body, err := json.MarshalIndent(status, "", "  ")
	if err != nil {
		return err
	}
	return s.root.replace(".sync-status", body, 0600)
}
