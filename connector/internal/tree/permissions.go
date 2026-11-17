package tree

import (
	"os/user"
	"runtime"
	"strconv"
)

// DaemonGroup is the local operator group allowed to read an organization tree.
func DaemonGroup(goos string) string {
	if goos == "darwin" {
		return "admin"
	}
	return "gentility"
}
func daemonGroupID() (int, error) {
	group, err := user.LookupGroup(DaemonGroup(runtime.GOOS))
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(group.Gid)
}

// DiscoveryDirectory resolves only the known, OS-owned macOS /var alias.
// Arbitrary supplied paths still pass through the no-symlink directory walker.
func DiscoveryDirectory(name string) string {
	if runtime.GOOS == "darwin" && name == "/var/lib/gentility" {
		return "/private/var/lib/gentility"
	}
	return name
}
