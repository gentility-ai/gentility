//go:build darwin

package tree

import "golang.org/x/sys/unix"

func renameExclusive(fd int, old, next string) error {
	return unix.RenameatxNp(fd, old, fd, next, unix.RENAME_EXCL)
}
