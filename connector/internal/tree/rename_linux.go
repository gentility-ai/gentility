//go:build linux

package tree

import "golang.org/x/sys/unix"

func renameExclusive(fd int, old, next string) error {
	return unix.Renameat2(fd, old, fd, next, unix.RENAME_NOREPLACE)
}
