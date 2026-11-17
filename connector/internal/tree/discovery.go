package tree

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"os/user"
	"runtime"
	"strconv"
	"strings"
	"syscall"

	"github.com/gentility-ai/gentility/connector/internal/enroll"
	"github.com/gentility-ai/gentility/connector/internal/tsnode"
	"golang.org/x/sys/unix"
)

// Discovery is public routing material, not a credential. Interface is included
// so user traffic is pinned to the daemon's kernel overlay, never a default route.
type Discovery struct {
	OrganizationID string `json:"organization_id,omitempty"`
	Origin         string `json:"origin"`
	Version        int    `json:"version"`
	Gateway        string `json:"gateway"`
	CAPEM          string `json:"ca_pem"`
	Organization   string `json:"organization"`
	Interface      string `json:"interface"`
}
type Trust func(os.FileInfo) error

func DaemonTrust(info os.FileInfo) error { return daemonTrust(info, runtime.GOOS, user.Lookup) }

func daemonTrust(info os.FileInfo, goos string, lookup func(string) (*user.User, error)) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return errors.New("tree: cannot verify discovery ownership")
	}
	owner := stat.Uid == 0
	if goos == "linux" && !owner {
		account, err := lookup("gentility")
		owner = err == nil && account.Uid == strconv.FormatUint(uint64(stat.Uid), 10)
	}
	if !owner || info.Mode().Perm()&0022 != 0 || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("tree: unsafe discovery ownership or permissions; reinstall the system connector to restore a daemon-owned, non-writable discovery file and directory")
	}
	return nil
}
func (d Discovery) validate() (netip.AddrPort, error) {
	if (d.OrganizationID != "" && !validOrganizationID(d.OrganizationID)) || d.Version != 1 || d.Organization == "" || reservedOrganizationSlug(d.Organization) || strings.Contains(d.Organization, "/") || safePath(d.Organization, false) != nil {
		return netip.AddrPort{}, errors.New("tree: unsupported discovery document")
	}
	origin, originErr := url.Parse(d.Origin)
	if originErr != nil || origin.Host == "" || origin.User != nil || origin.RawQuery != "" || origin.Fragment != "" || origin.Path != "" || (origin.Scheme != "https" && origin.Scheme != "http") {
		return netip.AddrPort{}, errors.New("tree: discovery has no valid login origin; update the server and system connector and let config apply")
	}
	gateway, err := netip.ParseAddrPort(d.Gateway)
	if err != nil || gateway.Port() == 0 {
		return netip.AddrPort{}, errors.New("tree: invalid discovery gateway")
	}
	if _, err := tsnode.OrgPrefix(gateway.Addr()); err != nil {
		return netip.AddrPort{}, errors.New("tree: discovery gateway is outside 6net")
	}
	if d.CAPEM == "" {
		return netip.AddrPort{}, errors.New("tree: discovery has no pinned CA")
	}
	if _, _, err := enroll.Gateway(gateway, d.CAPEM, false); err != nil {
		return netip.AddrPort{}, err
	}
	return gateway, nil
}

// ReadDiscovery checks the opened descriptors, not a stat followed by a second
// path lookup. Every ancestor is opened without following a symlink.
func ReadDiscovery(directoryPath string, trust Trust) (Discovery, error) {
	var result Discovery
	dir, err := openSearchDirectory(directoryPath)
	if err != nil {
		return result, err
	}
	defer dir.close()
	if trust == nil {
		trust = DaemonTrust
	}
	parentFD, err := unix.Dup(dir.fd)
	if err != nil {
		return result, err
	}
	unix.CloseOnExec(parentFD)
	parent := os.NewFile(uintptr(parentFD), "discovery parent")
	if parent == nil {
		return result, errors.New("tree: cannot inspect discovery parent")
	}
	defer parent.Close()
	info, err := parent.Stat()
	if err != nil {
		return result, err
	}
	if err := trust(info); err != nil {
		return result, err
	}
	fd, err := unix.Openat(dir.fd, "discovery.json", unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return result, fmt.Errorf("tree: cannot open trusted discovery; start or update the system connector; update its server and let config apply if saved node state lacks a login origin: %w", err)
	}
	file := os.NewFile(uintptr(fd), "discovery.json")
	defer file.Close()
	info, err = file.Stat()
	if err != nil {
		return result, err
	}
	if !info.Mode().IsRegular() {
		return result, errors.New("tree: discovery is not a regular file")
	}
	if err := trust(info); err != nil {
		return result, err
	}
	body, err := io.ReadAll(io.LimitReader(file, 65537))
	if err != nil {
		return result, err
	}
	if len(body) > 65536 {
		return result, errors.New("tree: discovery too large")
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return result, err
	}
	_, err = result.validate()
	return result, err
}
func WriteDiscovery(directoryPath string, discovery Discovery, trust Trust) error {
	if _, err := discovery.validate(); err != nil {
		return err
	}
	dir, err := openExistingDirectory(directoryPath)
	if err != nil {
		return err
	}
	defer dir.close()
	if trust == nil {
		trust = DaemonTrust
	}
	fd, err := unix.Dup(dir.fd)
	if err != nil {
		return err
	}
	file := os.NewFile(uintptr(fd), "discovery parent")
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if err := trust(info); err != nil {
		return err
	}
	body, err := json.Marshal(discovery)
	if err != nil {
		return err
	}
	if matches, err := dir.matches("discovery.json", digest(body), nil, 0644); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	} else if matches {
		return nil
	}
	return dir.replace("discovery.json", body, 0644)
}

// Client requires a live interface with an address in the gateway's org. Each
// socket binds both that source and the interface; a default route cannot win.
func (d Discovery) Client() (string, *http.Client, error) {
	gateway, err := d.validate()
	if err != nil {
		return "", nil, err
	}
	if d.Interface == "" {
		return "", nil, errors.New("tree: 6net interface is unavailable; run connector net join")
	}
	base, tlsConfig, err := enroll.Gateway(gateway, d.CAPEM, false)
	if err != nil {
		return "", nil, err
	}
	client := enroll.Client(func(ctx context.Context, network, address string) (net.Conn, error) {
		if address != gateway.String() {
			return nil, errors.New("tree: gateway destination changed")
		}
		iface, err := net.InterfaceByName(d.Interface)
		if err != nil {
			return nil, errors.New("tree: 6net interface is unavailable; run connector net join")
		}
		addresses, err := iface.Addrs()
		if err != nil {
			return nil, err
		}
		prefix, _ := tsnode.OrgPrefix(gateway.Addr())
		var source netip.Addr
		for _, a := range addresses {
			p, e := netip.ParsePrefix(a.String())
			if e == nil && prefix.Contains(p.Addr()) {
				source = p.Addr()
				break
			}
		}
		if !source.IsValid() {
			return nil, errors.New("tree: 6net interface has no address for this organization")
		}
		dial := &net.Dialer{LocalAddr: &net.TCPAddr{IP: net.IP(source.AsSlice())}, Control: func(network, address string, raw syscall.RawConn) error {
			var bindErr error
			if err := raw.Control(func(fd uintptr) { bindErr = bindInterface(int(fd), iface.Index) }); err != nil {
				return err
			}
			return bindErr
		}}
		return dial.DialContext(ctx, "tcp6", address)
	}, tlsConfig, netip.Addr{})
	return base, client, nil
}
