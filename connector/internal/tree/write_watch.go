package tree

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/fsnotify/fsnotify"
	"golang.org/x/sys/unix"
)

const writeDebounce = 500 * time.Millisecond
const maxWatchedFiles = 512

type watchedDirectory struct {
	dir   *directory
	alias string
}
type treeWatcher struct {
	native  *fsnotify.Watcher
	dirs    map[string]watchedDirectory
	aliases map[string]string
	due     map[string]time.Time
	retry   bool
}

func newTreeWatcher() (*treeWatcher, error) {
	native, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	return &treeWatcher{native: native, dirs: map[string]watchedDirectory{}, aliases: map[string]string{}, due: map[string]time.Time{}}, nil
}
func (w *treeWatcher) close() {
	w.native.Close()
	for _, d := range w.dirs {
		d.dir.close()
	}
}

func (w *treeWatcher) refresh(root *directory, rootPath string, manifest Manifest) error {
	wanted := map[string]bool{}
	watchFiles := 0
	overLimit := false
	var visit func(string) error
	visit = func(name string) error {
		if wanted[name] || overLimit {
			return nil
		}
		wanted[name] = true
		dir, err := root.walk(name, false)
		if err != nil {
			return err
		}
		defer dir.close()
		names, err := dir.names()
		if err != nil {
			return err
		}
		// kqueue opens one descriptor per directory entry, including ignored files.
		watchFiles += len(names) + 1
		if watchFiles > maxWatchedFiles {
			overLimit = true
			return nil
		}
		for _, base := range names {
			child := path.Join(name, base)
			if ignoredLocal(child) {
				continue
			}
			var st unix.Stat_t
			if err := unix.Fstatat(dir.fd, base, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
				if errors.Is(err, os.ErrNotExist) {
					continue
				}
				return err
			}
			if st.Mode&unix.S_IFMT != unix.S_IFDIR {
				continue
			}
			info, err := root.stat(child)
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				return err
			}
			if info.IsDir() {
				if err := visit(child); err != nil {
					return err
				}
			}
		}
		return nil
	}
	for _, d := range manifest.Directories {
		policy, _ := decodeWritePolicy(d.Write)
		if policy.Edit {
			if err := visit(d.Path); err != nil {
				return err
			}
		}
	}
	if overLimit {
		wanted = map[string]bool{}
	} // Polling covers the entire tree.
	reset := false
	for name, d := range w.dirs {
		same := false
		if wanted[name] {
			current, err := root.walk(name, false)
			if err != nil {
				return err
			}
			var before, after unix.Stat_t
			oldErr := unix.Fstat(d.dir.fd, &before)
			newErr := unix.Fstat(current.fd, &after)
			current.close()
			if oldErr != nil {
				return oldErr
			}
			if newErr != nil {
				return newErr
			}
			same = before.Dev == after.Dev && before.Ino == after.Ino
		}
		if !same {
			reset = true
			_ = w.native.Remove(d.alias)
			d.dir.close()
			delete(w.aliases, d.alias)
			delete(w.dirs, name)
		}
	}
	if reset {
		native, err := fsnotify.NewWatcher()
		if err != nil {
			return err
		}
		// kqueue may still have a queued rename event for the old inode. A
		// fresh watcher prevents it removing a new watch at the reused pathname.
		w.native.Close()
		for _, d := range w.dirs {
			d.dir.close()
		}
		w.dirs = map[string]watchedDirectory{}
		w.aliases = map[string]string{}
		w.native = native
	}
	for name := range wanted {
		if _, ok := w.dirs[name]; ok {
			continue
		}
		dir, err := root.walk(name, false)
		if err != nil {
			return err
		}
		// Linux accepts descriptor aliases; kqueue requires a filesystem path.
		// Events are only rescan hints: reads and writes still use pinned handles.
		alias := filepath.Join(rootPath, filepath.FromSlash(name))
		if runtime.GOOS == "linux" {
			alias = fmt.Sprintf("/proc/self/fd/%d", dir.fd)
		}
		if err := w.native.Add(alias); err != nil {
			dir.close()
			return err
		}
		current, err := root.walk(name, false)
		if err != nil {
			_ = w.native.Remove(alias)
			dir.close()
			return err
		}
		var before, after unix.Stat_t
		oldErr := unix.Fstat(dir.fd, &before)
		newErr := unix.Fstat(current.fd, &after)
		current.close()
		if oldErr != nil || newErr != nil || before.Dev != after.Dev || before.Ino != after.Ino {
			_ = w.native.Remove(alias)
			dir.close()
			return errors.New("tree: directory changed while registering watcher")
		}
		w.dirs[name] = watchedDirectory{dir, alias}
		w.aliases[alias] = name
	}
	return nil
}

// settling counts saved paths still inside their debounce window.
func (w *treeWatcher) settling() int {
	count := 0
	for name := range w.due {
		if !w.ready(name) {
			count++
		}
	}
	return count
}

func (w *treeWatcher) ready(name string) bool {
	due, ok := w.due[name]
	return !ok || !time.Now().Before(due)
}

func (w *treeWatcher) wait(ctx context.Context, poll time.Duration) error {
	deadline := time.Now().Add(poll)
	for {
		next := deadline
		if !w.retry {
			for _, due := range w.due {
				if due.Before(next) {
					next = due
				}
			}
		}
		timer := time.NewTimer(max(0, time.Until(next)))
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
			for name, due := range w.due {
				if !time.Now().Before(due) {
					delete(w.due, name)
				}
			}
			return nil
		case _, ok := <-w.native.Errors:
			timer.Stop()
			if !ok {
				return errors.New("tree: watcher closed")
			}
			// Overflow is a rescan hint. Polling remains the source of truth.
			if !w.retry {
				return nil
			}
		case event, ok := <-w.native.Events:
			timer.Stop()
			if !ok {
				return errors.New("tree: watcher closed")
			}
			if event.Op&(fsnotify.Write|fsnotify.Create|fsnotify.Remove|fsnotify.Rename) == 0 {
				continue
			}
			parent := filepath.Dir(event.Name)
			scope, ok := w.aliases[parent]
			if !ok {
				continue
			}
			name := path.Join(scope, filepath.Base(event.Name))
			if ignoredLocal(name) || strings.Contains(name, "\\") {
				continue
			}
			w.due[name] = time.Now().Add(writeDebounce)
		}
	}
}
