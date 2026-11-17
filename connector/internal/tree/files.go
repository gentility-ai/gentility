//go:build darwin || linux

package tree

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// directory pins each component with O_NOFOLLOW. No operation subsequently
// resolves a parent path through the ambient filesystem again.
type directory struct {
	fd   int
	mode uint32
	gid  *int
}

func (d *directory) close() { unix.Close(d.fd) }
func openDirectory(name string) (*directory, error) {
	return openDirectoryAccess(name, unix.O_RDONLY, true)
}
func openExistingDirectory(name string) (*directory, error) {
	return openDirectoryAccess(name, unix.O_RDONLY, false)
}
func openSearchDirectory(name string) (*directory, error) {
	return openDirectoryAccess(name, directorySearchAccess, false)
}
func openDirectoryAccess(name string, access int, create bool) (*directory, error) {
	absolute, err := filepath.Abs(name)
	if err != nil {
		return nil, err
	}
	fd, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	root := &directory{fd: fd}
	defer root.close()
	return root.walkFlags(strings.TrimPrefix(absolute, "/"), create, access)
}
func (d *directory) walk(name string, create bool) (*directory, error) {
	return d.walkFlags(name, create, unix.O_RDONLY)
}
func (d *directory) walkFlags(name string, create bool, access int) (*directory, error) {
	fd, err := unix.Dup(d.fd)
	if err != nil {
		return nil, err
	}
	unix.CloseOnExec(fd)
	for _, part := range strings.Split(name, "/") {
		if part == "" || part == "." {
			continue
		}
		if part == ".." {
			unix.Close(fd)
			return nil, refuse("tree: parent traversal refused")
		}
		next, e := unix.Openat(fd, part, access|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if errors.Is(e, unix.ENOENT) && create {
			mode := uint32(0700)
			if d.mode != 0 {
				mode = d.mode
			}
			if e = unix.Mkdirat(fd, part, mode); e != nil && !errors.Is(e, unix.EEXIST) {
				unix.Close(fd)
				return nil, e
			}
			next, e = unix.Openat(fd, part, access|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		}
		if e != nil {
			var st unix.Stat_t
			if unix.Fstatat(fd, part, &st, unix.AT_SYMLINK_NOFOLLOW) == nil && st.Mode&unix.S_IFMT == unix.S_IFLNK {
				unix.Close(fd)
				return nil, refuse("tree: symlink directory refused at %q", part)
			}
		}
		unix.Close(fd)
		if e != nil {
			return nil, fmt.Errorf("tree: unsafe or unavailable directory %q: %w", part, e)
		}
		if d.mode != 0 {
			if _, e := setPermissions(next, d.mode, d.gid); e != nil {
				unix.Close(next)
				return nil, e
			}
		}
		fd = next
	}
	return &directory{fd: fd, mode: d.mode, gid: d.gid}, nil
}
func (d *directory) parent(name string, create bool) (*directory, string, error) {
	if err := safePath(name, false); err != nil {
		return nil, "", err
	}
	parent, err := d.walk(filepath.Dir(name), create)
	return parent, filepath.Base(name), err
}
func regularAt(fd int, name string) error {
	var st unix.Stat_t
	err := unix.Fstatat(fd, name, &st, unix.AT_SYMLINK_NOFOLLOW)
	if err != nil {
		return err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG {
		return refuse("tree: %q is not a regular file", name)
	}
	return nil
}
func (d *directory) read(name string, limit int64) ([]byte, error) {
	parent, base, err := d.parent(name, false)
	if err != nil {
		return nil, err
	}
	defer parent.close()
	fd, err := unix.Openat(parent.fd, base, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), base)
	defer file.Close()
	stat, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !stat.Mode().IsRegular() {
		return nil, refuse("tree: non-regular file refused")
	}
	body, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > limit {
		return nil, errors.New("tree: file exceeds limit")
	}
	return body, nil
}
func (d *directory) replace(name string, body []byte, mode os.FileMode) error {
	parent, base, err := d.parent(name, true)
	if err != nil {
		return err
	}
	defer parent.close()
	if err := regularAt(parent.fd, base); err != nil && !errors.Is(err, unix.ENOENT) {
		return err
	}
	temp := ".tree-" + rand.Text()
	fd, err := unix.Openat(parent.fd, temp, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
	if err != nil {
		return err
	}
	defer unix.Unlinkat(parent.fd, temp, 0)
	file := os.NewFile(uintptr(fd), temp)
	_, writeErr := file.Write(body)
	if writeErr == nil {
		_, writeErr = setPermissions(fd, uint32(mode), d.gid)
	}
	if writeErr == nil {
		writeErr = file.Sync()
	}
	closeErr := file.Close()
	if writeErr != nil {
		return writeErr
	}
	if closeErr != nil {
		return closeErr
	}
	// Rename replaces a raced-in symlink itself, never its target.
	if err := regularAt(parent.fd, base); err != nil && !errors.Is(err, unix.ENOENT) {
		return err
	}
	if err := unix.Renameat(parent.fd, temp, parent.fd, base); err != nil {
		return err
	}
	return unix.Fsync(parent.fd)
}
func (d *directory) remove(name string) error {
	parent, base, err := d.parent(name, false)
	if errors.Is(err, unix.ENOENT) {
		return nil
	}
	if err != nil {
		return err
	}
	defer parent.close()
	if err := regularAt(parent.fd, base); errors.Is(err, unix.ENOENT) {
		return nil
	} else if err != nil {
		return err
	}
	// Unlink cannot follow a raced-in symlink.
	if err := unix.Unlinkat(parent.fd, base, 0); err != nil {
		return err
	}
	return unix.Fsync(parent.fd)
}
func (d *directory) prune(name string) error {
	parent, base, err := d.parent(name, false)
	if errors.Is(err, unix.ENOENT) {
		return nil
	}
	if err != nil {
		return err
	}
	defer parent.close()
	child, err := parent.walk(base, false)
	if errors.Is(err, unix.ENOENT) {
		return nil
	}
	if err != nil {
		return err
	}
	child.close()
	err = unix.Unlinkat(parent.fd, base, unix.AT_REMOVEDIR)
	if errors.Is(err, unix.ENOTEMPTY) || errors.Is(err, unix.EEXIST) {
		return nil
	}
	return err
}
func (d *directory) lock() (*os.File, error) {
	fd, err := unix.Openat(d.fd, ".tree-lock", unix.O_RDWR|unix.O_CREAT|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0600)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), "tree lock")
	if info, err := file.Stat(); err != nil || !info.Mode().IsRegular() {
		file.Close()
		return nil, errors.New("tree: unsafe lock file")
	}
	if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		file.Close()
		return nil, fmt.Errorf("tree: another syncer holds the root: %w", err)
	}
	return file, nil
}

func (d *directory) matches(name, hash string, size *int64, mode os.FileMode) (bool, error) {
	parent, base, err := d.parent(name, false)
	if errors.Is(err, unix.ENOENT) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer parent.close()
	fd, err := unix.Openat(parent.fd, base, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if errors.Is(err, unix.ENOENT) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	file := os.NewFile(uintptr(fd), base)
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return false, err
	}
	if !info.Mode().IsRegular() {
		return false, refuse("tree: non-regular file refused")
	}

	if size != nil && info.Size() != *size {
		return false, nil
	}
	hashState := sha256.New()
	_, err = io.Copy(hashState, file)
	if err != nil {
		return false, err
	}
	if hex.EncodeToString(hashState.Sum(nil)) != hash {
		return false, nil
	}
	changed, err := setPermissions(fd, uint32(mode), d.gid)
	if err != nil {
		return false, err
	}
	if changed {
		if err := file.Sync(); err != nil {
			return false, err
		}
	}
	return true, nil
}

// stat uses the pinned parent and refuses symlinks without following them.
func (d *directory) stat(name string) (os.FileInfo, error) {
	parent, base, err := d.parent(name, false)
	if err != nil {
		return nil, err
	}
	defer parent.close()
	var st unix.Stat_t
	if err := unix.Fstatat(parent.fd, base, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return nil, err
	}
	if st.Mode&unix.S_IFMT == unix.S_IFLNK {
		return nil, refuse("tree: symlink refused at %q", name)
	}
	fd, err := unix.Openat(parent.fd, base, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), base)
	defer file.Close()
	return file.Stat()
}

// cleanTemps walks pinned directories and removes only our abandoned regular
// temporary files. Symlinks are never traversed, even during recovery.
func (d *directory) cleanTemps() error {
	fd, err := unix.Openat(d.fd, ".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	file := os.NewFile(uintptr(fd), "tree")
	defer file.Close()
	entries, err := file.ReadDir(-1)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() {
			child, err := d.walk(entry.Name(), false)
			if err != nil {
				return err
			}
			err = child.cleanTemps()
			child.close()
			if err != nil {
				return err
			}
		} else if strings.HasPrefix(entry.Name(), ".tree-") && entry.Type().IsRegular() {
			if err := d.remove(entry.Name()); err != nil {
				return err
			}
		}
	}
	return nil
}

// setPermissions avoids metadata writes on an unchanged cache. Group changes
// precede mode changes because chown can clear permission bits on some systems.
func setPermissions(fd int, mode uint32, gid *int) (bool, error) {
	var info unix.Stat_t
	if err := unix.Fstat(fd, &info); err != nil {
		return false, err
	}
	changed := false
	if gid != nil && int(info.Gid) != *gid {
		if err := unix.Fchown(fd, -1, *gid); err != nil {
			return false, err
		}
		changed = true
	}
	if uint32(info.Mode)&07777 != mode {
		if err := unix.Fchmod(fd, mode); err != nil {
			return false, err
		}
		changed = true
	}
	return changed, nil
}
