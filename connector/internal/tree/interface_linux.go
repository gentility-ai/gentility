package tree

import (
	"encoding/binary"
	"golang.org/x/sys/unix"
)

func bindInterface(fd, index int) error {
	// IPV6_UNICAST_IF takes the interface index in network byte order.
	// Linux net/ipv6/ipv6_sockglue.c applies ntohl before looking it up.
	var value [4]byte
	binary.BigEndian.PutUint32(value[:], uint32(index))
	return unix.SetsockoptInt(fd, unix.IPPROTO_IPV6, unix.IPV6_UNICAST_IF, int(binary.NativeEndian.Uint32(value[:])))
}

// Search-only directory descriptors allow reading discovery in the daemon's
// traversable, deliberately non-listable state directory.
const directorySearchAccess = unix.O_PATH
