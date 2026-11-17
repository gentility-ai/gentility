package tree

import (
	"context"
	"encoding/hex"
	"errors"
	"os"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

func validOrganizationID(id string) bool {
	if len(id) != 36 || id[8] != '-' || id[13] != '-' || id[18] != '-' || id[23] != '-' {
		return false
	}
	bytes, err := hex.DecodeString(strings.ReplaceAll(id, "-", ""))
	return err == nil && len(bytes) == 16 && id != "00000000-0000-0000-0000-000000000000" && strings.ToLower(id) == id
}

// reservedOrganizationSlug names the top-level directories the tree itself
// owns. The server reserves them too; refusing them here does not trust it.
func reservedOrganizationSlug(slug string) bool { return slug == "me" || slug == "skills" }

func validOrganizationSlug(slug string) bool {
	if slug == "" || reservedOrganizationSlug(slug) {
		return false
	}
	for _, c := range slug {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

// Config is the authority for identity; a manifest can only corroborate it.
func (s *Syncer) checkSource(source *Source) error {
	if source.OrganizationID != "" && !validOrganizationID(source.OrganizationID) {
		return refuse("tree: invalid authenticated organization ID")
	}
	if s.checkpoint.OrganizationID != "" && source.OrganizationID != s.checkpoint.OrganizationID {
		return refuse("tree: organization ID missing or changed; restore authenticated config for this root")
	}
	if s.options.Principal == "node" {
		if source.Organization == "" {
			return errors.New("tree: daemon organization is not available yet")
		}
		source.Identity = "node-org:" + source.Organization
		// Keep the historical identity string so legacy state remains readable.
		// After migration the separately checked ID is the organization authority.
		if s.checkpoint.OrganizationID != "" {
			source.Identity = s.checkpoint.Identity
		}
	}
	if source.Identity == "" || source.Bearer == "" {
		return errors.New("tree: missing identity or login")
	}
	if s.checkpoint.OrganizationID == "" && s.checkpoint.Organization != "" && source.Organization != "" && s.checkpoint.Organization != source.Organization {
		return refuse("tree: legacy root slug changed before its ID was learned; preserve tree and private state and restore the old slug to migrate")
	}
	if s.checkpoint.Identity != "" && source.Identity != s.checkpoint.Identity {
		return refuse("tree: root belongs to another identity; preserve the tree and restore its original login")
	}
	return nil
}
func (s *Syncer) manifestOrganization(source Source, m Manifest) (string, error) {
	if m.Organization != nil && source.OrganizationID != "" && m.Organization.ID != source.OrganizationID {
		return "", refuse("tree: manifest organization ID differs from authenticated config")
	}
	if s.checkpoint.OrganizationID != "" {
		if m.Organization == nil || m.Organization.ID != s.checkpoint.OrganizationID {
			return "", refuse("tree: bound organization ID absent or changed in manifest")
		}
		return m.validate(s.options.Principal, m.Organization.Slug)
	}
	org := s.checkpoint.Organization
	if org == "" {
		org = source.Organization
	}
	if m.Organization != nil && org != "" && org != m.Organization.Slug {
		return "", refuse("tree: legacy root slug changed before its ID was learned; preserve tree and private state and restore the old slug to migrate")
	}
	return m.validate(s.options.Principal, org)
}
func (s *Syncer) bindOrganization(source Source, m Manifest, org string) error {
	if s.checkpoint.OrganizationID == "" && source.OrganizationID != "" && m.Organization != nil {
		s.checkpoint.OrganizationID = source.OrganizationID
		s.checkpoint.Identity = source.Identity
		s.checkpoint.Organization = org
		return s.save()
	}
	return nil
}

type rootMove struct {
	OrganizationID string `json:"organization_id"`
	From           string `json:"from"`
	To             string `json:"to"`
	Device         uint64 `json:"device"`
	Inode          uint64 `json:"inode"`
}

func (s *Syncer) directoryIdentity(name string) (unix.Stat_t, error) {
	var stat unix.Stat_t
	err := unix.Fstatat(s.root.fd, name, &stat, unix.AT_SYMLINK_NOFOLLOW)
	if err == nil && stat.Mode&unix.S_IFMT != unix.S_IFDIR {
		return stat, refuse("tree: organization path is not a directory")
	}
	return stat, err
}

// refuseSkillsMove stops a slug change on a layout-2 or layout-3 root, before
// any edit is drained: skills/<slug>/ lies outside the organization directory
// a move renames. Slugs are immutable (T-154), so this guards a case that
// should not arise rather than carrying skills/ through a move.
func (s *Syncer) refuseSkillsMove() error {
	if layout := s.checkpoint.Manifest.layout(); layout >= 2 {
		return refuse("tree: organization slug changed on a layout-%d root; preserve tree and private state", layout)
	}
	return nil
}

func (s *Syncer) moveRoot(to string) error {
	if err := s.refuseSkillsMove(); err != nil {
		return err
	}
	if !validOrganizationSlug(s.checkpoint.Organization) || !validOrganizationSlug(to) {
		return refuse("tree: unsafe organization slug")
	}
	if _, err := s.directoryIdentity(to); !errors.Is(err, os.ErrNotExist) {
		return refuse("tree: rename destination occupied; preserve both directories and move the destination aside deliberately")
	}
	stat, err := s.directoryIdentity(s.checkpoint.Organization)
	if errors.Is(err, os.ErrNotExist) && s.checkpoint.Move == nil {
		// No rename has begun and no directory remains to move. The authenticated
		// ID still binds this root; ordinary reconciliation recreates its files.
		return s.finishRootMove(to)
	}
	if err != nil {
		return err
	}
	s.checkpoint.Move = &rootMove{OrganizationID: s.checkpoint.OrganizationID, From: s.checkpoint.Organization, To: to, Device: uint64(stat.Dev), Inode: uint64(stat.Ino)}
	if err := s.save(); err != nil {
		return err
	}
	return s.resumeMove()
}
func (s *Syncer) resumeMove() error {
	move := s.checkpoint.Move
	if move == nil {
		return nil
	}
	if move.OrganizationID == "" || move.OrganizationID != s.checkpoint.OrganizationID || move.From != s.checkpoint.Organization || move.From == move.To || !validOrganizationSlug(move.From) || !validOrganizationSlug(move.To) {
		return refuse("tree: invalid root move intent; preserve tree and private state")
	}
	if s.options.Principal == "person" {
		if s.journal == nil {
			journal, err := openWriteJournal(s.state, s.checkpoint.Identity, s.checkpoint.Organization)
			if err != nil {
				return err
			}
			s.journal = journal
		}
		if len(s.journal.pending) != 0 {
			return refuse("tree: root move intent has pending writes; preserve tree and private state")
		}
	}
	same := func(stat unix.Stat_t) bool { return uint64(stat.Dev) == move.Device && uint64(stat.Ino) == move.Inode }
	from, err := s.directoryIdentity(move.From)
	switch {
	case err == nil:
		if !same(from) {
			return refuse("tree: source directory changed during root move")
		}
		if _, err := s.directoryIdentity(move.To); !errors.Is(err, os.ErrNotExist) {
			return refuse("tree: rename destination occupied; preserve both directories")
		}
		if err := renameExclusive(s.root.fd, move.From, move.To); err != nil {
			return err
		}
	case errors.Is(err, os.ErrNotExist):
	default:
		return err
	}
	to, err := s.directoryIdentity(move.To)
	if err != nil || !same(to) {
		return refuse("tree: cannot establish ownership of renamed directory; preserve tree and private state")
	}
	if err := unix.Fsync(s.root.fd); err != nil {
		return err
	}
	return s.finishRootMove(move.To)
}

func (s *Syncer) finishRootMove(to string) error {
	previous := s.checkpoint
	next := previous
	next.Organization = to
	next.Manifest = rebaseManifest(previous.Manifest, previous.Organization, to)
	next.Managed = rebasePaths(previous.Managed, previous.Organization, to)
	next.ManagedDirectories = rebasePaths(previous.ManagedDirectories, previous.Organization, to)
	next.ETag = ""
	next.Move = nil
	s.checkpoint = next
	if err := s.save(); err != nil {
		s.checkpoint = previous
		return err
	}
	if s.journal != nil {
		s.journal.organization = to
	}
	s.conditional = false
	return nil
}
func rebasePath(name, from, to string) string {
	if name == from {
		return to
	}
	if strings.HasPrefix(name, from+"/") {
		return to + strings.TrimPrefix(name, from)
	}
	return name
}
func rebasePaths(paths []string, from, to string) []string {
	result := make([]string, len(paths))
	for i, p := range paths {
		result[i] = rebasePath(p, from, to)
	}
	return result
}
func rebaseManifest(m Manifest, from, to string) Manifest {
	m.Directories = append([]Directory(nil), m.Directories...)
	m.Files = append([]Entry(nil), m.Files...)
	m.Indexes = append([]Entry(nil), m.Indexes...)
	for i := range m.Directories {
		m.Directories[i].Path = rebasePath(m.Directories[i].Path, from, to)
	}
	for i := range m.Files {
		m.Files[i].Path = rebasePath(m.Files[i].Path, from, to)
	}
	for i := range m.Indexes {
		m.Indexes[i].Path = rebasePath(m.Indexes[i].Path, from, to)
	}
	if m.Organization != nil {
		m.Organization = &Organization{ID: m.Organization.ID, Slug: to}
	}
	return m
}

func (s *Syncer) drainRename(ctx context.Context, source Source, manifest Manifest, newSlug string) error {
	if err := s.refuseSkillsMove(); err != nil {
		return err
	}
	// This view is for recovery GETs only. A found canonical post is adopted in
	// the old directory, so its ordinary receipt stays valid until the move.
	lookup := rebaseManifest(manifest, newSlug, s.checkpoint.Organization)
	for _, name := range s.journal.paths() {
		if !strings.HasPrefix(name, s.checkpoint.Organization+"/") {
			continue
		}
		p := s.journal.pending[name]
		if p.Rejection == "not_found" || p.Rejection == "readonly_target" {
			p.Rejection = "organization renamed"
			if err := s.journal.save(p); err != nil {
				return err
			}
		}
		if p.Receipt != nil || p.Rejection != "" {
			continue
		}
		if p.PostKey != "" && p.Sending {
			receipt, err := recoverPost(ctx, source, lookup, p)
			if err != nil && !errors.Is(err, errPostGrace) && !errors.Is(err, errPostUnconfirmed) {
				return err
			}
			p.Receipt = receipt
			if receipt == nil {
				since := p.AmbiguousAt
				if since.IsZero() {
					since = p.SentAt
				}
				if time.Since(since) <= 24*time.Hour {
					continue
				}
				p.Rejection = "organization renamed"
			}
		} else {
			p.Rejection = "organization renamed"
		}
		if err := s.journal.save(p); err != nil {
			return err
		}
	}
	// Personal skills at root me/ retain their paths and ordinary write policy.
	if _, err := s.sendPending(ctx, source, manifest); err != nil {
		return err
	}
	return s.finishPending()
}
