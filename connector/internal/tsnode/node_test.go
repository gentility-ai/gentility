package tsnode

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"strings"
	"sync"
	"testing"

	"tailscale.com/client/local"
	"tailscale.com/client/tailscale/apitype"
	"tailscale.com/ipn/ipnstate"
	"tailscale.com/tailcfg"
	"tailscale.com/types/key"
)

// A Node built without a tsnet.Server. Everything below the seams — the
// listener set, the grant set, the org boundary — is bookkeeping, and
// bookkeeping does not need a head, a netstack or a network to be wrong.
func testNode(listen func(network, addr string) (net.Listener, error)) *Node {
	return &Node{listen: listen, listeners: map[netip.Addr]net.Listener{}}
}

type fakeListener struct {
	mu     sync.Mutex
	closed int
}

func (l *fakeListener) Accept() (net.Conn, error) { return nil, errors.New("fakeListener: no accept") }
func (l *fakeListener) Addr() net.Addr            { return nil }

func (l *fakeListener) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.closed++
	return nil
}

func (l *fakeListener) closes() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.closed
}

func TestDialBoundRefusesAnUnsetSource(t *testing.T) {
	n := testNode(nil)
	dst := netip.AddrPortFrom(addr(t, "fd06:6e7:0:1::2"), 80)

	_, err := n.DialBound(context.Background(), netip.Addr{}, dst)
	if err == nil {
		t.Fatal("DialBound accepted the zero Addr; netstack would bind the wildcard and leave from the primary")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "wildcard") {
		t.Fatalf("error %q does not say an empty bind is a wildcard bind", err)
	}
}

func TestDialBoundRefusesASourceNoGrantGaveThisNode(t *testing.T) {
	n := testNode(nil)
	n.SetGrants([]Grant{{Org: "acme", Address: addr(t, "fd06:6e7:0:1::1")}})
	dst := netip.AddrPortFrom(addr(t, "fd06:6e7:0:1::2"), 80)

	// Same org, right shape, but not an address any grant handed this node.
	_, err := n.DialBound(context.Background(), addr(t, "fd06:6e7:0:1::9"), dst)
	if err == nil {
		t.Fatal("DialBound dialled from an address no grant gave this node")
	}
	if !strings.Contains(err.Error(), "fd06:6e7:0:1::9") {
		t.Fatalf("error %q does not name the source it refused", err)
	}
}

func TestDialBoundRefusesASourceOutsideTheOverlay(t *testing.T) {
	n := testNode(nil)
	n.SetGrants([]Grant{{Org: "acme", Address: addr(t, "fd06:6e7:0:1::1")}})
	dst := netip.AddrPortFrom(addr(t, "fd06:6e7:0:1::2"), 80)

	if _, err := n.DialBound(context.Background(), addr(t, "100.64.0.1"), dst); err == nil {
		t.Fatal("DialBound accepted a CGNAT v4 source; the overlay is v6-only")
	}
}

func TestListenAllKeepsEarlierBindsWhenALaterOneFails(t *testing.T) {
	first := addr(t, "fd06:6e7:0:1::1")
	second := addr(t, "fd06:6e7:0:2::1")
	gone := addr(t, "fd06:6e7:0:3::1")

	boom := errors.New("address already in use")
	n := testNode(func(_, address string) (net.Listener, error) {
		if strings.HasPrefix(address, "[fd06:6e7:0:2:") {
			return nil, boom
		}
		return &fakeListener{}, nil
	})

	if _, err := n.ListenAll([]netip.Addr{first, gone}, 8080); err != nil {
		t.Fatalf("first ListenAll: %v", err)
	}
	dropped := n.listeners[gone].(*fakeListener)

	opened, err := n.ListenAll([]netip.Addr{first, second}, 8080)
	if !errors.Is(err, boom) {
		t.Fatalf("ListenAll error = %v, want it to wrap the listen failure", err)
	}
	if len(opened) != 0 {
		t.Fatalf("opened = %v, want nothing new: `first` was already bound", opened)
	}
	if _, held := n.listeners[first]; !held {
		t.Fatal("a failed bind tore down an earlier, healthy listener")
	}
	if _, held := n.listeners[second]; held {
		t.Fatal("the node recorded a listener it never got")
	}
	if dropped.closes() != 1 {
		t.Fatalf("the address the node no longer holds was closed %d times, want 1", dropped.closes())
	}
}

func TestListenAllWithTheSameAddressSetBindsNothing(t *testing.T) {
	addresses := []netip.Addr{addr(t, "fd06:6e7:0:1::1"), addr(t, "fd06:6e7:0:2::1")}

	binds := 0
	n := testNode(func(_, _ string) (net.Listener, error) {
		binds++
		return &fakeListener{}, nil
	})

	if _, err := n.ListenAll(addresses, 8080); err != nil {
		t.Fatalf("first ListenAll: %v", err)
	}
	opened, err := n.ListenAll(addresses, 8080)
	if err != nil {
		t.Fatalf("second ListenAll: %v", err)
	}
	if len(opened) != 0 {
		t.Fatalf("second ListenAll opened %v, want nothing", opened)
	}
	if binds != len(addresses) {
		t.Fatalf("bound %d times over two identical calls, want %d", binds, len(addresses))
	}
}

func TestListenAllPassesThePortThrough(t *testing.T) {
	// 65535 does not survive a uint16(int) truncation by accident: it is the
	// port a caller means, spelled out in the address the netstack binds.
	var asked []string
	n := testNode(func(_, address string) (net.Listener, error) {
		asked = append(asked, address)
		return &fakeListener{}, nil
	})

	if _, err := n.ListenAll([]netip.Addr{addr(t, "fd06:6e7:0:1::1")}, 65535); err != nil {
		t.Fatalf("ListenAll: %v", err)
	}
	if len(asked) != 1 || asked[0] != "[fd06:6e7:0:1::1]:65535" {
		t.Fatalf("listened on %v, want [fd06:6e7:0:1::1]:65535", asked)
	}
}

func TestCloseIsSafeTwiceAndClosesEveryListener(t *testing.T) {
	n := testNode(func(_, _ string) (net.Listener, error) { return &fakeListener{}, nil })
	addresses := []netip.Addr{addr(t, "fd06:6e7:0:1::1"), addr(t, "fd06:6e7:0:2::1")}
	if _, err := n.ListenAll(addresses, 8080); err != nil {
		t.Fatalf("ListenAll: %v", err)
	}

	held := make([]*fakeListener, 0, len(addresses))
	for _, a := range addresses {
		held = append(held, n.listeners[a].(*fakeListener))
	}

	n.Close()
	n.Close()

	for i, ln := range held {
		if ln.closes() != 1 {
			t.Fatalf("listener %d closed %d times over two Close calls, want 1", i, ln.closes())
		}
	}
	if len(n.listeners) != 0 {
		t.Fatalf("Close left %d listeners behind", len(n.listeners))
	}
}

func TestSetGrantsClonesTheCallersSlice(t *testing.T) {
	grants := []Grant{{Org: "acme", Address: addr(t, "fd06:6e7:0:1::1")}}
	n := testNode(nil)
	n.SetGrants(grants)

	// The caller still holds its slice. Rewriting it must not re-home the node.
	grants[0] = Grant{Org: "attacker", Address: addr(t, "fd06:6e7:0:1::66")}

	src, err := SourceFor(n.grants, addr(t, "fd06:6e7:0:1::2"))
	if err != nil {
		t.Fatalf("SourceFor: %v", err)
	}
	if src.Org != "acme" || src.Address.String() != "fd06:6e7:0:1::1" {
		t.Fatalf("the node's grant became %+v; SetGrants aliased the caller's slice", src)
	}
}

func TestSetGrantsAndDialInOrgInterleaveSafely(t *testing.T) {
	// Under -race. DialInOrg stops at SourceFor — no grant is ever in the
	// destination's /64 — so nothing reaches the netstack; the point is that
	// the detector sees a read of n.grants against a write of it.
	n := testNode(nil)
	dst := netip.AddrPortFrom(addr(t, "fd06:6e7:0:9::9"), 80)
	sets := [][]Grant{
		{{Org: "acme", Address: addr(t, "fd06:6e7:0:1::1")}},
		{{Org: "beta", Address: addr(t, "fd06:6e7:0:2::1")}},
	}

	var wg sync.WaitGroup
	for w := 0; w < 4; w++ {
		wg.Add(2)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < 300; i++ {
				n.SetGrants(sets[(w+i)%len(sets)])
			}
		}(w)
		go func() {
			defer wg.Done()
			for i := 0; i < 300; i++ {
				if _, err := n.DialInOrg(context.Background(), dst); err == nil {
					panic("DialInOrg found a source for an org this node has no grant in")
				}
			}
		}()
	}
	wg.Wait()
}

func whoIsResponse(name, login string, addresses ...netip.Prefix) *apitype.WhoIsResponse {
	return &apitype.WhoIsResponse{
		Node:        &tailcfg.Node{Key: key.NewNode().Public(), Name: name, Addresses: addresses},
		UserProfile: &tailcfg.UserProfile{LoginName: login},
	}
}

func TestWhoIsAttributesAPeerInTheSameOrg(t *testing.T) {
	want := whoIsResponse("db.acme.example.ts.net.", "svc@acme.example", netip.MustParsePrefix("fd06:6e7:0:1::2/128"))
	n := testNode(nil)
	n.whois = func(_ context.Context, _ string) (*apitype.WhoIsResponse, error) { return want, nil }

	peer, err := n.WhoIs(
		context.Background(),
		netip.AddrPortFrom(addr(t, "fd06:6e7:0:1::2"), 41234),
		addr(t, "fd06:6e7:0:1::1"),
	)
	if err != nil {
		t.Fatalf("WhoIs: %v", err)
	}
	if peer.NodeKey != want.Node.Key {
		t.Fatalf("peer key = %v, want %v", peer.NodeKey, want.Node.Key)
	}
	if peer.Name != "db.acme.example.ts.net." || peer.Login != "svc@acme.example" {
		t.Fatalf("peer = %+v, want the name and login WhoIs gave", peer)
	}
	if len(peer.Addresses) != 1 || peer.Addresses[0].String() != "fd06:6e7:0:1::2" {
		t.Fatalf("peer addresses = %v, want [fd06:6e7:0:1::2]", peer.Addresses)
	}
}

func TestWhoIsRefusesAPeerInAnotherOrg(t *testing.T) {
	n := testNode(nil)
	n.whois = func(_ context.Context, _ string) (*apitype.WhoIsResponse, error) {
		return whoIsResponse("stranger.example.ts.net.", "someone@beta.example"), nil
	}

	// A well-attributed node — it really is on the overlay — that reached a
	// listener in an org it holds no grant in.
	_, err := n.WhoIs(
		context.Background(),
		netip.AddrPortFrom(addr(t, "fd06:6e7:0:2::9"), 41234),
		addr(t, "fd06:6e7:0:1::1"),
	)
	if !errors.Is(err, ErrCrossOrgPeer) {
		t.Fatalf("WhoIs error = %v, want ErrCrossOrgPeer", err)
	}
	if !strings.Contains(err.Error(), "fd06:6e7:0:2::/64") || !strings.Contains(err.Error(), "fd06:6e7:0:1::/64") {
		t.Fatalf("error %q does not name both org prefixes", err)
	}
}

func TestWhoIsWrapsPeerNotFound(t *testing.T) {
	// An unknown peer is a fact about the peer, not about the LocalAPI. A
	// caller that cannot tell them apart closes the wrong thing.
	n := testNode(nil)
	n.whois = func(_ context.Context, _ string) (*apitype.WhoIsResponse, error) {
		return nil, local.ErrPeerNotFound
	}

	_, err := n.WhoIs(
		context.Background(),
		netip.AddrPortFrom(addr(t, "fd06:6e7:0:1::2"), 41234),
		addr(t, "fd06:6e7:0:1::1"),
	)
	if !errors.Is(err, local.ErrPeerNotFound) {
		t.Fatalf("WhoIs error = %v, want it to wrap local.ErrPeerNotFound", err)
	}
}

func TestOverlayAddressesKeepsOnlyTheOverlayULA(t *testing.T) {
	got := overlayAddresses([]netip.Addr{
		addr(t, "100.64.0.1"),
		addr(t, "fd06:6e7:0:1::1"),
		addr(t, "2001:db8::1"),
		addr(t, "fd06:6e7:0:2::1"),
	})
	if len(got) != 2 || got[0].String() != "fd06:6e7:0:1::1" || got[1].String() != "fd06:6e7:0:2::1" {
		t.Fatalf("overlayAddresses = %v, want only the two ULA addresses", got)
	}
}

func TestListenBindsOneExplicitAddress(t *testing.T) {
	var bound string
	n := testNode(func(_, address string) (net.Listener, error) {
		bound = address
		return &fakeListener{}, nil
	})

	if _, err := n.Listen(netip.MustParseAddr("fd06:6e7:0:1::a"), 8443); err != nil {
		t.Fatalf("Listen: %v", err)
	}
	if want := "[fd06:6e7:0:1::a]:8443"; bound != want {
		t.Fatalf("bound %q, want %q", bound, want)
	}
	// It is the caller's listener, not the reconciled set's: Close must not find
	// it, so an egress relay closing its own listener is the only owner.
	if len(n.listeners) != 0 {
		t.Fatalf("Listen joined the reconciled set (%d entries); it must not", len(n.listeners))
	}
}

func TestListenRefusesAWildcardAddress(t *testing.T) {
	n := testNode(func(_, _ string) (net.Listener, error) {
		t.Fatal("a wildcard address must be refused before the netstack is asked")
		return nil, nil
	})

	if _, err := n.Listen(netip.Addr{}, 8443); err == nil {
		t.Fatal("Listen accepted the zero address")
	}
	if _, err := n.Listen(netip.IPv6Unspecified(), 8443); err == nil {
		t.Fatal("Listen accepted the unspecified address")
	}
}

// Status names the interface the node was built over, through the status seam,
// so the daemon's snapshot says utun4 in interface mode and nothing in
// userspace mode.
func TestStatusNamesTheInterface(t *testing.T) {
	for _, iface := range []string{"utun4", ""} {
		t.Run("iface="+iface, func(t *testing.T) {
			n := testNode(nil)
			n.iface = iface
			n.status = func(context.Context) (*ipnstate.Status, error) {
				return &ipnstate.Status{Self: &ipnstate.PeerStatus{CurAddr: "203.0.113.7:41641"}}, nil
			}

			got, err := n.Status(context.Background())
			if err != nil {
				t.Fatalf("Status: %v", err)
			}
			if got.Interface != iface {
				t.Fatalf("Report.Interface = %q, want %q", got.Interface, iface)
			}
			if got.Endpoint != "203.0.113.7:41641" {
				t.Fatalf("Report.Endpoint = %q, want the seam's", got.Endpoint)
			}
		})
	}
}

// The netmap names this node fully qualified, with the trailing dot DNS keeps;
// the report carries the name a person types, without it.
func TestStatusCarriesTheNodesNameWithoutTheTrailingDot(t *testing.T) {
	n := testNode(nil)
	n.status = func(context.Context) (*ipnstate.Status, error) {
		return &ipnstate.Status{Self: &ipnstate.PeerStatus{
			DNSName: "cedar.morgan.northwind.6net.io.", Relay: "lhr",
		}}, nil
	}

	got, err := n.Status(context.Background())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if got.Name != "cedar.morgan.northwind.6net.io" {
		t.Fatalf("Report.Name = %q, want the netmap's name without its dot", got.Name)
	}
	if !got.Relayed || got.Endpoint != "lhr" {
		t.Fatalf("Report = %+v, want the relayed path through lhr", got)
	}
}
