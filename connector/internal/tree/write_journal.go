package tree

import (
	"encoding/json"
	"errors"
	"os"
	"sort"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// Each operation is independently atomic and bounded. A large number of queued
// edits does not enlarge the manifest checkpoint or one JSON read without bound.
// The journal directory lives under the existing private, exclusively locked
// state root. No bearer or gateway credential is stored here.
type writeJournal struct {
	dir          *directory
	identity     string
	organization string
	pending      map[string]pendingWrite
}

type writeRecord struct {
	Identity     string       `json:"identity"`
	Organization string       `json:"organization"`
	Pending      pendingWrite `json:"pending"`
}

func openWriteJournal(state *directory, identity, organization string) (*writeJournal, error) {
	dir, err := state.walk("writes", true)
	if err != nil {
		return nil, err
	}
	journal := &writeJournal{dir: dir, identity: identity, organization: organization, pending: map[string]pendingWrite{}}
	names, err := dir.names()
	if err != nil {
		dir.close()
		return nil, err
	}
	for _, name := range names {
		if !strings.HasSuffix(name, ".json") {
			continue
		}
		body, err := dir.read(name, 4*maxWrite)
		if err != nil {
			dir.close()
			return nil, err
		}
		var record writeRecord
		if json.Unmarshal(body, &record) != nil {
			dir.close()
			return nil, refuse("tree: corrupt pending write; preserve private state")
		}
		p := record.Pending
		if record.Identity != identity || record.Organization != organization {
			dir.close()
			return nil, refuse("tree: pending writes belong to another identity or organization")
		}
		if err := writePath(p.Path); err != nil {
			dir.close()
			return nil, err
		}
		if name != writeRecordName(p.Path) || len(p.Body) > maxWrite || p.LocalHash == "" || p.Started.IsZero() ||
			strings.ContainsAny(p.Revision, "\"\r\n") || (p.Sending && p.SentAt.IsZero()) {
			dir.close()
			return nil, refuse("tree: invalid pending write; preserve private state")
		}
		if p.Receipt != nil {
			receipt, err := parseReceipt(p.Path, "/overlay/self/tree/"+p.Receipt.Path, "\""+p.Receipt.Revision+"\"")
			if err != nil || receipt != *p.Receipt {
				dir.close()
				return nil, refuse("tree: invalid saved write receipt")
			}
		}
		journal.pending[p.Path] = p
	}
	return journal, nil
}

func writeRecordName(name string) string { return digest([]byte(name)) + ".json" }

func (j *writeJournal) save(p pendingWrite) error {
	body, err := json.Marshal(writeRecord{Identity: j.identity, Organization: j.organization, Pending: p})
	if err != nil {
		return err
	}
	if err := j.dir.replace(writeRecordName(p.Path), body, 0600); err != nil {
		return err
	}
	j.pending[p.Path] = p
	return nil
}

func (j *writeJournal) remove(name string) error {
	if err := j.dir.remove(writeRecordName(name)); err != nil {
		return err
	}
	delete(j.pending, name)
	return nil
}

func (j *writeJournal) enqueue(name, revision string, body []byte, now time.Time) error {
	if err := writePath(name); err != nil {
		return err
	}
	if p, exists := j.pending[name]; exists && p.Receipt == nil && p.Rejection == "" {
		return errors.New("tree: write already pending")
	}
	if len(body) > maxWrite {
		return errors.New("tree: local write exceeds size limit")
	}
	p := pendingWrite{Path: name, Revision: revision, Body: body, LocalHash: digest(body), Started: now}
	if revision == "" && postPath(name) {
		key, err := newPostKey(now)
		if err != nil {
			return err
		}
		wire, err := postWithKey(body, key)
		if err != nil {
			p.Rejection = "invalid_post_format"
			return j.save(p)
		}
		if len(wire) > maxWrite {
			p.Rejection = "body_too_large"
			return j.save(p)
		}
		p.PostKey, p.Body = key, wire
	}
	return j.save(p)
}

func (j *writeJournal) paths() []string {
	names := make([]string, 0, len(j.pending))
	for name := range j.pending {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// names enumerates a pinned directory, never an ambient path or symlink.
func (d *directory) names() ([]string, error) {
	fd, err := unix.Openat(d.fd, ".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), "tree directory")
	defer file.Close()
	names, err := file.Readdirnames(-1)
	if err != nil {
		return nil, err
	}
	sort.Strings(names)
	return names, nil
}
