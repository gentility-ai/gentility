package tree

import (
	"bytes"
	"crypto/rand"
	"errors"
	"io"
	"os"
	"path"
	"strings"

	"golang.org/x/sys/unix"
)

// preserveRejection makes recovery idempotent without trusting a pre-existing
// file merely because it has our preferred name.
func (d *directory) preserveRejection(name string, body []byte) (string, error) {
	for {
		err := d.install(name, body, 0600)
		if !errors.Is(err, unix.EEXIST) {
			return name, err
		}
		existing, readErr := d.read(name, int64(len(body))+1)
		if readErr == nil && bytes.Equal(existing, body) {
			return name, nil
		}
		name = strings.TrimSuffix(name, ".rejected.md") + "-" + rand.Text() + ".rejected.md"
	}
}

// moveCanonical keeps the local inode intact and never overwrites a collision.
// Receipts are restricted to the same parent by parseReceipt.
func (d *directory) moveCanonical(from, to string) error {
	if parentPath(from) != parentPath(to) {
		return refuse("tree: canonical move escaped its directory")
	}
	parent, base, err := d.parent(from, false)
	if err != nil {
		return err
	}
	defer parent.close()
	if err := renameExclusive(parent.fd, base, path.Base(to)); err != nil {
		return err
	}
	return unix.Fsync(parent.fd)
}

// displace atomically retains the actual inode, including a save that raced our
// earlier read. The exclusive rename cannot overwrite an earlier recovery copy.
func (d *directory) displace(name string) (string, error) {
	parent, base, err := d.parent(name, false)
	if err != nil {
		return "", err
	}
	defer parent.close()
	copyName := strings.TrimSuffix(base, ".md") + "-" + rand.Text() + ".conflict.md"
	if err := renameExclusive(parent.fd, base, copyName); err != nil {
		return "", err
	}
	if err := unix.Fsync(parent.fd); err != nil {
		return "", err
	}
	return path.Join(parentPath(name), copyName), nil
}

// install never replaces an editor's raced-in file. The caller retries after
// capturing it; a crash leaves either the old recovery copy or this new file.
func (d *directory) install(name string, body []byte, mode os.FileMode) error {
	parent, base, err := d.parent(name, true)
	if err != nil {
		return err
	}
	defer parent.close()
	temp := ".tree-" + rand.Text()
	if err := parent.replace(temp, body, mode); err != nil {
		return err
	}
	defer unix.Unlinkat(parent.fd, temp, 0)
	if err := unix.Linkat(parent.fd, temp, parent.fd, base, 0); err != nil {
		return err
	}
	return unix.Fsync(parent.fd)
}

// rejectionCopy streams the complete local file, including oversized edits.
// Nothing truncates the original or removes it before preservation is durable.
func (d *directory) rejectionCopy(name, original, code string) (string, error) {
	parent, base, err := d.parent(name, false)
	if err != nil {
		return "", err
	}
	defer parent.close()
	fd, err := unix.Openat(parent.fd, base, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return "", err
	}
	input := os.NewFile(uintptr(fd), base)
	defer input.Close()
	info, err := input.Stat()
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", refuse("tree: rejection source is not regular")
	}
	rejected := strings.TrimSuffix(path.Base(original), ".md") + ".rejected.md"
	out, err := unix.Openat(parent.fd, rejected, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
	if errors.Is(err, unix.EEXIST) {
		rejected = strings.TrimSuffix(path.Base(original), ".md") + "-" + rand.Text() + ".rejected.md"
		out, err = unix.Openat(parent.fd, rejected, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
	}
	if err != nil {
		return "", err
	}
	output := os.NewFile(uintptr(out), rejected)
	_, err = io.WriteString(output, "<!-- Shared tree rejected this edit: "+code+". Your text is preserved below. -->\n\n")
	if err == nil {
		_, err = io.Copy(output, input)
	}
	if err == nil {
		err = output.Sync()
	}
	closeErr := output.Close()
	if err != nil {
		return "", err
	}
	if closeErr != nil {
		return "", closeErr
	}
	if err := unix.Fsync(parent.fd); err != nil {
		return "", err
	}
	return path.Join(parentPath(name), rejected), nil
}

func (d *directory) localFiles(prefix string) ([]string, error) {
	dir, err := d.walk(prefix, false)
	if err != nil {
		return nil, err
	}
	defer dir.close()
	names, err := dir.names()
	if err != nil {
		return nil, err
	}
	var files []string
	for _, base := range names {
		name := path.Join(prefix, base)
		if ignoredLocal(name) {
			continue
		}
		var st unix.Stat_t
		if err := unix.Fstatat(dir.fd, base, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return nil, err
		}
		if st.Mode&unix.S_IFMT != unix.S_IFREG && st.Mode&unix.S_IFMT != unix.S_IFDIR {
			continue
		}
		info, err := d.stat(name)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if info.IsDir() {
			nested, err := d.localFiles(name)
			if err != nil {
				return nil, err
			}
			files = append(files, nested...)
		} else if info.Mode().IsRegular() {
			files = append(files, name)
		}
	}
	return files, nil
}
