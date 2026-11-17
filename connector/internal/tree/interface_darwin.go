package tree

import "golang.org/x/sys/unix"

func bindInterface(fd, index int) error {
	return unix.SetsockoptInt(fd, unix.IPPROTO_IPV6, unix.IPV6_BOUND_IF, index)
}

// Search-only directory descriptors allow reading discovery in the daemon's
// traversable, deliberately non-listable state directory.
const directorySearchAccess = unix.O_SEARCH
