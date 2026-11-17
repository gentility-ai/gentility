package tsnode

import (
	"context"
	"net/netip"
	"slices"
	"strings"
	"time"

	"tailscale.com/ipn/ipnstate"
)

// PeerCounter is one peer's cumulative WireGuard byte counters, as the engine
// keeps them: Rx is what this node received from the peer, Tx what it sent.
// They only grow, until the peer is re-added or the engine restarts.
type PeerCounter struct {
	Address       string
	Rx, Tx        int64
	LastHandshake time.Time
}

// PeerCounters reads every peer's counters from the same status read
// peerPaths uses (internal/traffic samples it once a second).
func (n *Node) PeerCounters(ctx context.Context) ([]PeerCounter, error) {
	status, err := n.selfStatus(ctx)
	if err != nil {
		return nil, err
	}
	return peerCounters(status), nil
}

// peerCounters names each peer by one address: the lowest of its overlay
// addresses inside an organization this node is also in. WireGuard keeps one
// counter pair per peer, not per address, so a peer in two organizations this
// node shares has its bytes reported once, under one of them, and never
// twice. A peer sharing no organization with this node is skipped: there is
// no address the mothership would file it under for us.
func peerCounters(status *ipnstate.Status) []PeerCounter {
	shared := map[netip.Prefix]bool{}
	for _, address := range overlayAddresses(status.Self.TailscaleIPs) {
		prefix, err := OrgPrefix(address)
		if err == nil {
			shared[prefix] = true
		}
	}

	counters := make([]PeerCounter, 0, len(status.Peer))
	for _, peer := range status.Peer {
		addresses := overlayAddresses(peer.TailscaleIPs)
		slices.SortFunc(addresses, func(a, b netip.Addr) int { return a.Compare(b) })
		for _, address := range addresses {
			prefix, err := OrgPrefix(address)
			if err != nil || !shared[prefix] {
				continue
			}
			counters = append(counters, PeerCounter{
				Address:       address.String(),
				Rx:            peer.RxBytes,
				Tx:            peer.TxBytes,
				LastHandshake: peer.LastHandshake,
			})
			break
		}
	}
	slices.SortFunc(counters, func(a, b PeerCounter) int { return strings.Compare(a.Address, b.Address) })
	return counters
}
