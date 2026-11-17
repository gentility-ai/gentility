package tsnode

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"sync"
	"time"

	"tailscale.com/client/tailscale/apitype"
	"tailscale.com/ipn"
	"tailscale.com/ipn/ipnstate"
	"tailscale.com/net/netcheck"
	"tailscale.com/tsnet"
	"tailscale.com/types/key"
	"tailscale.com/types/netmap"
)

// ErrCrossOrgPeer is what WhoIs returns when an attributed peer reached a
// listener in an org it holds no grant in. It is a named error because the
// caller's answer is to close the connection and say which two orgs met, not
// to retry.
var ErrCrossOrgPeer = errors.New("tsnode: peer is in another org")

// Peer is what this node knows about the other end of a connection, reduced to
// the fields the connector acts on.
//
// It is tsnode's own type on purpose: ipnstate.PeerStatus is the status page's
// struct, wide and free to move with the library, and a caller that reads it
// is a caller the pin has to carry.
type Peer struct {
	NodeKey   key.NodePublic
	Name      string
	Addresses []netip.Addr
	Login     string
}

// Node is one overlay identity: an engine plus the grants it holds.
//
// The engine is either a tsnet server (userspace mode, Up and New) or the
// engine interface mode assembles around a real TUN (upInterface). Callers
// never touch either directly — that is the whole point of this package. In
// particular nothing outside here may listen or dial with a wildcard address;
// see the package doc for why.
type Node struct {
	// The seams onto the engine. Each constructor sets them; tests replace
	// them. What is left underneath is bookkeeping — the listener set, the
	// grant set, the org boundary — and bookkeeping should not need a head, a
	// netstack and ninety seconds to be proved right.
	listen  func(network, addr string) (net.Listener, error)
	dial    func(ctx context.Context, src netip.Addr, dst netip.AddrPort) (net.Conn, error)
	whois   func(ctx context.Context, remoteAddr string) (*apitype.WhoIsResponse, error)
	status  func(ctx context.Context) (*ipnstate.Status, error)
	probe   func(context.Context) *netcheck.Report
	peerMap func(context.Context) (*netmap.NetworkMap, error)

	// close shuts the engine down. It is set only where this Node built the
	// engine (Up, upInterface), and is nil for a Node wrapped around somebody
	// else's server (New): that server stays its owner's to close.
	close func()

	// iface is the kernel interface the overlay is on — utun4, tailscale0 —
	// and empty in userspace mode, where there is none.
	iface string

	mu        sync.Mutex
	grants    []Grant
	listeners map[netip.Addr]net.Listener
}

// New wraps a started tsnet.Server, asserting the netstack escape hatch is
// still there before the node is allowed to serve anything.
func New(srv *tsnet.Server) (*Node, error) {
	if err := AssertNetstack(srv); err != nil {
		return nil, err
	}

	return &Node{
		listen: srv.Listen,
		dial: func(ctx context.Context, src netip.Addr, dst netip.AddrPort) (net.Conn, error) {
			impl, err := netstackImpl(srv)
			if err != nil {
				return nil, err
			}
			// Returned only on success: a nil *gonet.TCPConn in a net.Conn is
			// not a nil net.Conn.
			conn, err := impl.DialContextTCPWithBind(ctx, src, dst)
			if err != nil {
				return nil, err
			}
			return conn, nil
		},
		whois: func(ctx context.Context, remoteAddr string) (*apitype.WhoIsResponse, error) {
			client, err := srv.LocalClient()
			if err != nil {
				return nil, fmt.Errorf("local client: %w", err)
			}
			return client.WhoIs(ctx, remoteAddr)
		},
		status: func(ctx context.Context) (*ipnstate.Status, error) {
			client, err := srv.LocalClient()
			if err != nil {
				return nil, fmt.Errorf("local client: %w", err)
			}
			return client.Status(ctx)
		},
		peerMap: func(ctx context.Context) (*netmap.NetworkMap, error) {
			ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
			defer cancel()
			client, err := srv.LocalClient()
			if err != nil {
				return nil, err
			}
			watcher, err := client.WatchIPNBus(ctx, ipn.NotifyInitialNetMap)
			if err != nil {
				return nil, err
			}
			defer watcher.Close()
			initial, err := watcher.Next()
			if err != nil {
				return nil, err
			}
			return initial.NetMap, nil
		},
		listeners: map[netip.Addr]net.Listener{},
		probe: func(ctx context.Context) *netcheck.Report {
			conn, ok := srv.Sys().MagicSock.GetOK()
			if !ok {
				return nil
			}
			return conn.GetLastNetcheckReport(ctx)
		},
	}, nil
}

// Interface is the kernel interface this node's overlay addresses are on, or
// "" in userspace mode, where they are on no interface at all.
func (n *Node) Interface() string { return n.iface }

// Addresses returns every overlay address the control plane gave this node.
//
// ipnstate.Status.Self.TailscaleIPs carries all of them;
// tsnet.Server.TailscaleIPs() carries one (its loop overwrites ip6 on each v6
// address, so it returns the LAST). Reading the wrong one is how a node ends
// up serving a single org.
func (n *Node) Addresses(ctx context.Context) ([]netip.Addr, error) {
	status, err := n.selfStatus(ctx)
	if err != nil {
		return nil, err
	}

	return overlayAddresses(status.Self.TailscaleIPs), nil
}

// overlayAddresses keeps the addresses that are this node's to serve.
//
// The filter is OrgPrefix, not Is6: the head hands out a CGNAT v4 alongside
// every overlay address, and a v6 address outside 6net's range has no /64
// this overlay recognises — neither can be an org's address, and ListenAll
// binds exactly what this returns.
func overlayAddresses(ips []netip.Addr) []netip.Addr {
	var out []netip.Addr
	for _, ip := range ips {
		if _, err := OrgPrefix(ip); err != nil {
			continue
		}
		out = append(out, ip)
	}

	return out
}

// SetGrants records which org each of this node's addresses belongs to. The
// config document is where they come from, by way of
// ParseGrants.
func (n *Node) SetGrants(grants []Grant) {
	// Cloned: the caller keeps its slice, and a grant set that can be rewritten
	// from outside the lock is a source selection that races with whoever still
	// holds the config document.
	cloned := slices.Clone(grants)

	n.mu.Lock()
	defer n.mu.Unlock()
	n.grants = cloned
}

// ListenAll binds one explicit listener per address on port, closes listeners
// for addresses this node no longer holds, and returns the newly bound ones.
//
// Explicit per-address is not an optimisation: a wildcard listener on a
// multi-address node answers on exactly one address, and which one depends on
// netmap ordering — i.e. on grant order, not on anything the node chose.
//
// port is a uint16 because that is what a port is. It was an int, narrowed at
// the call to the netstack, which turned 70000 into a listener on 4464 without
// a word.
//
// A bind that fails leaves the binds before it in place and reports the error;
// the node is then serving a subset of its addresses, which is the truth, and
// the caller reconciles again.
func (n *Node) ListenAll(addresses []netip.Addr, port uint16) (map[netip.Addr]net.Listener, error) {
	// The whole reconcile is one critical section: a listener set read, then
	// acted on after the lock was dropped, is a set that can have moved.
	// Holding the mutex across n.listen is accepted — reconciles happen on an
	// address-set change, and the netstack's Listen does not call back in here.
	n.mu.Lock()
	defer n.mu.Unlock()

	bound := make([]netip.Addr, 0, len(n.listeners))
	for a := range n.listeners {
		bound = append(bound, a)
	}
	// Map iteration order is random, so unbind order would be too. Sorting
	// makes what the node closes, and the order it logs, the same every run.
	slices.SortFunc(bound, netip.Addr.Compare)

	bind, unbind := Reconcile(bound, addresses)

	for _, a := range unbind {
		n.listeners[a].Close()
		delete(n.listeners, a)
	}

	opened := make(map[netip.Addr]net.Listener, len(bind))
	for _, a := range bind {
		ln, err := n.listen("tcp", netip.AddrPortFrom(a, port).String())
		if err != nil {
			return opened, fmt.Errorf("tsnode: listen on [%s]:%d: %w", a, port, err)
		}
		n.listeners[a] = ln
		opened[a] = ln
	}

	return opened, nil
}

// Listen binds one explicit listener on addr:port, through the netstack or the
// kernel as the mode has it.
//
// It is ListenAll for a caller that already knows the single address it wants
// and owns the listener's lifecycle itself — the egress relay, which binds on
// its own overlay address in one organization and closes the listener on its
// own shutdown. Unlike ListenAll it does not join the reconciled listener set,
// so Close does not touch it.
//
// A wildcard/zero address is refused for the reason the package exists: a
// wildcard tsnet listener on a multi-address node answers on exactly one of
// them, chosen by netmap order, and refuses every other org's peers in a
// millisecond.
func (n *Node) Listen(addr netip.Addr, port uint16) (net.Listener, error) {
	if !addr.IsValid() || addr.IsUnspecified() {
		return nil, fmt.Errorf(
			"tsnode: Listen needs an explicit address, not %q — a wildcard listener on a "+
				"multi-address node answers on only one of them", addr,
		)
	}
	return n.listen("tcp", netip.AddrPortFrom(addr, port).String())
}

// DialBound opens a TCP connection to dst FROM src: through the netstack in
// userspace mode, and as a kernel socket bound to src in interface mode.
//
// src must be one of this node's own granted addresses; SourceFor picks it.
// Callers that have a destination and no source use DialInOrg.
//
// Both refusals below are the grant model, not defensive tidying. The zero Addr
// makes netstack skip its local-address check and bind the wildcard, which is
// primary-source dialling under another name; an address no grant gave this
// node is a claim to speak into an org from outside it.
func (n *Node) DialBound(ctx context.Context, src netip.Addr, dst netip.AddrPort) (net.Conn, error) {
	if !src.IsValid() {
		return nil, fmt.Errorf(
			"tsnode: dial %s: no source address. An empty bind is a WILDCARD bind — "+
				"netstack skips its local-address check and the connection leaves from "+
				"this node's primary address, which is the primary-source dialling this "+
				"package exists to prevent. Use DialInOrg, or pass the address SourceFor "+
				"returns",
			dst,
		)
	}

	if err := n.granted(src); err != nil {
		return nil, err
	}

	conn, err := n.dial(ctx, src, dst)
	if err != nil {
		return nil, fmt.Errorf("tsnode: dial %s from %s: %w", dst, src, err)
	}

	return conn, nil
}

// granted checks src against this node's grants. DialBound is not a way around
// the grant model: the /64 is what an org is, so a source outside every grant
// is either a mistake or a node reaching for an address it was never given.
func (n *Node) granted(src netip.Addr) error {
	prefix, err := OrgPrefix(src)
	if err != nil {
		return fmt.Errorf("tsnode: source %s: %w", src, err)
	}

	n.mu.Lock()
	grants := n.grants
	n.mu.Unlock()

	for _, grant := range grants {
		// The /64 says the grant is for src's org; the equality says src is the
		// address that grant handed over, not merely a neighbour of it.
		got, err := OrgPrefix(grant.Address)
		if err != nil {
			continue
		}
		if got == prefix && grant.Address == src {
			return nil
		}
	}

	return fmt.Errorf(
		"tsnode: %s is not one of this node's granted addresses, so DialBound will not "+
			"dial from it; %s is granted to whoever holds it, not to this node",
		src, prefix,
	)
}

// DialInOrg is the door every outbound connection in the connector uses: it
// resolves the destination's org, picks this node's own address in that org,
// and dials with the source pinned.
//
// The gateway is the sharpest case — it holds a grant in EVERY org, so a
// primary-source dial would silently fail for all but one of them.
func (n *Node) DialInOrg(ctx context.Context, dst netip.AddrPort) (net.Conn, error) {
	n.mu.Lock()
	grants := n.grants
	n.mu.Unlock()

	src, err := SourceFor(grants, dst.Addr())
	if err != nil {
		return nil, err
	}

	return n.DialBound(ctx, src.Address, dst)
}

// WhoIs attributes a peer by its overlay address and holds the node's own
// second boundary, applied before it trusts anything a connection says about
// itself.
//
// remote is the peer as net.Conn.RemoteAddr gives it; local is the address of
// the listener it reached — that is what names the org the connection is in.
// Attribution comes first, the boundary second: a peer the head does not know
// is refused by tsnet, and a peer it does know but that reached an org it holds
// no grant in is refused with ErrCrossOrgPeer.
//
// An unknown peer wraps tailscale.com/client/local's ErrPeerNotFound, so
// errors.Is(err, local.ErrPeerNotFound) finds it through the wrapping. Callers
// must not read "not found" as the LocalAPI being down — one closes a
// connection, the other closes the node.
func (n *Node) WhoIs(ctx context.Context, remote netip.AddrPort, local netip.Addr) (Peer, error) {
	who, err := n.whois(ctx, remote.String())
	if err != nil {
		return Peer{}, fmt.Errorf("tsnode: whois %s: %w", remote, err)
	}
	if who == nil || who.Node == nil {
		return Peer{}, fmt.Errorf("tsnode: whois %s attributed no node", remote)
	}

	remotePrefix, err := OrgPrefix(remote.Addr())
	if err != nil {
		return Peer{}, err
	}
	localPrefix, err := OrgPrefix(local)
	if err != nil {
		return Peer{}, err
	}
	if remotePrefix != localPrefix {
		return Peer{}, fmt.Errorf(
			"tsnode: peer %s is in %s but reached this node's listener in %s: %w",
			remote.Addr(), remotePrefix, localPrefix, ErrCrossOrgPeer,
		)
	}

	peer := Peer{NodeKey: who.Node.Key, Name: who.Node.Name}
	for _, p := range who.Node.Addresses {
		peer.Addresses = append(peer.Addresses, p.Addr())
	}
	if who.UserProfile != nil {
		peer.Login = who.UserProfile.LoginName
	}

	return peer, nil
}

// selfStatus is the status seam's answer, refused when it has no self — a node
// that is not up has no addresses to report.
func (n *Node) selfStatus(ctx context.Context) (*ipnstate.Status, error) {
	status, err := n.status(ctx)
	if err != nil {
		return nil, fmt.Errorf("tsnode: status: %w", err)
	}
	if status == nil || status.Self == nil {
		return nil, fmt.Errorf("tsnode: status has no self; the node is not up")
	}

	return status, nil
}

// Close releases every explicit listener, and the engine too where this Node
// built it. It is safe to call twice: the second call finds nothing left to
// close. A server the caller passed to New stays the caller's.
func (n *Node) Close() {
	n.mu.Lock()
	for a, ln := range n.listeners {
		ln.Close()
		delete(n.listeners, a)
	}
	closeEngine := n.close
	n.close = nil
	n.mu.Unlock()

	if closeEngine != nil {
		closeEngine()
	}
}
