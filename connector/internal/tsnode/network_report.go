package tsnode

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sort"

	"tailscale.com/ipn/ipnstate"
)

// PeerPath is this node's observation of one peer address. Relay is a DERP
// region, never a peer's public endpoint or an address in another organization.
// Via is the UDP address a direct path uses, ip:port as magicsock reports it
// (ipnstate.PeerStatus.CurAddr): the one fact that says whether a LAN peer is
// reached over its Wi-Fi, a VM bridge or a router's hairpin. Empty unless Path
// is direct; an idle peer's stale address is not a path.
type PeerPath struct {
	Address string `json:"address"`
	Path    string `json:"path"`
	Relay   string `json:"relay,omitempty"`
	Via     string `json:"via,omitempty"`
}

// LocalAddress describes a private interface address visible to this process:
// RFC 1918 IPv4 or a ULA outside 6net's range. A subnet is local context, not a
// route advertised to 6net. Container and VPN interfaces may appear; equal
// private prefixes on different machines do not imply a LAN.
//
// Public and link-local addresses are never reported. Every member of the
// organization can read these, and a laptop's global IPv6 address names its
// owner's home network, while a link-local one may carry the interface's MAC.
type LocalAddress struct {
	Interface string `json:"interface"`
	Address   string `json:"address"`
	Subnet    string `json:"subnet"`
}

// The heartbeat body is capped at 64 KiB by the mothership, and presence must
// never be refused for its observations. A gateway sees every peer in every
// organization it serves, so both lists are bounded: active peers first, then
// idle ones, and whatever falls past the cap is counted in the error.
const (
	maxPeerPaths      = 256
	maxLocalAddresses = 64
)

func peerPaths(status *ipnstate.Status) ([]PeerPath, error) {
	paths := make([]PeerPath, 0)
	for _, peer := range status.Peer {
		path, relay, via := "unknown", "", ""
		switch {
		case !peer.Active:
			path = "idle"
		case peer.CurAddr != "":
			path, via = "direct", peer.CurAddr
		case peer.PeerRelay != "":
			path = "peer_relay"
		case peer.Relay != "":
			path, relay = "derp", peer.Relay
		}
		for _, address := range overlayAddresses(peer.TailscaleIPs) {
			paths = append(paths, PeerPath{Address: address.String(), Path: path, Relay: relay, Via: via})
		}
	}
	sort.Slice(paths, func(i, j int) bool {
		if idle := paths[i].Path == "idle"; idle != (paths[j].Path == "idle") {
			return !idle
		}
		return paths[i].Address < paths[j].Address
	})
	if len(paths) > maxPeerPaths {
		return paths[:maxPeerPaths], fmt.Errorf("%d peer paths past the cap of %d omitted", len(paths)-maxPeerPaths, maxPeerPaths)
	}
	return paths, nil
}

// localAddresses is best effort: an interface that cannot be read is skipped
// and named in the error, and the rest are still returned. A host that cannot
// list its interfaces at all (a sandbox without netlink) returns none.
func localAddresses(overlayInterface string) ([]LocalAddress, error) {
	interfaces, err := net.Interfaces()
	if err != nil {
		return nil, fmt.Errorf("list local interfaces: %w", err)
	}
	result := make([]LocalAddress, 0)
	var problems []error
	for _, iface := range interfaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 || iface.Name == overlayInterface {
			continue
		}
		addresses, err := iface.Addrs()
		if err != nil {
			problems = append(problems, fmt.Errorf("addresses for %s: %w", iface.Name, err))
			continue
		}
		for _, address := range addresses {
			prefix, err := netip.ParsePrefix(address.String())
			if err != nil {
				problems = append(problems, fmt.Errorf("interface address %s: %w", address, err))
				continue
			}
			if entry, include := localAddress(iface.Name, prefix); include {
				result = append(result, entry)
			}
		}
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Interface != result[j].Interface {
			return result[i].Interface < result[j].Interface
		}
		return result[i].Address < result[j].Address
	})
	if len(result) > maxLocalAddresses {
		problems = append(problems, fmt.Errorf("%d local addresses past the cap of %d omitted", len(result)-maxLocalAddresses, maxLocalAddresses))
		result = result[:maxLocalAddresses]
	}
	return result, errors.Join(problems...)
}

func localAddress(name string, prefix netip.Prefix) (LocalAddress, bool) {
	address := prefix.Addr()
	if !address.IsPrivate() || ula.Contains(address) {
		return LocalAddress{}, false
	}
	return LocalAddress{Interface: name, Address: address.String(), Subnet: prefix.Masked().String()}, true
}
