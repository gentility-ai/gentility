package tsnodetest

import (
	"context"
	"github.com/gentility-ai/gentility/connector/internal/tsnode"
	"net"
	"net/http/httptest"
	"net/netip"
	"tailscale.com/derp/derpserver"
	"tailscale.com/net/netns"
	"tailscale.com/net/stun/stuntest"
	"tailscale.com/tailcfg"
	"tailscale.com/tsnet"
	"tailscale.com/tstest/integration/testcontrol"
	"tailscale.com/types/key"
	"tailscale.com/types/logger"
	"testing"
	"time"
)

// PeerNode starts an isolated tsnet node and gives it a synthetic peer through
// a local control plane. Import this fixture package only from _test.go files.
func PeerNode(t *testing.T, ctx context.Context) (*tsnode.Node, string, string) {
	t.Helper()
	netns.SetEnabled(false)
	t.Cleanup(func() { netns.SetEnabled(true) })
	control := &testcontrol.Server{DERPMap: snapshotDERP(t), AllOnline: true, Logf: t.Logf}
	control.HTTPTestServer = httptest.NewServer(control)
	t.Cleanup(control.HTTPTestServer.Close)
	srv := &tsnet.Server{Dir: t.TempDir(), Hostname: "snapshot-writer", ControlURL: control.HTTPTestServer.URL, AuthKey: "test", Logf: t.Logf}
	t.Cleanup(func() { srv.Close() })
	self, err := srv.Up(ctx)
	if err != nil {
		t.Fatal(err)
	}
	node, err := tsnode.New(srv)
	if err != nil {
		t.Fatal(err)
	}
	response, err := control.MapResponse(&tailcfg.MapRequest{NodeKey: self.Self.PublicKey})
	if err != nil {
		t.Fatal(err)
	}
	address := netip.MustParseAddr("fd06:6e7:0:1::b")
	endpoint := netip.MustParseAddrPort("127.0.0.1:54321")
	response.Peers = []*tailcfg.Node{{
		ID: 123, Key: key.NewNode().Public(), DiscoKey: key.NewDisco().Public(),
		Name: "peer.example.", Hostinfo: (&tailcfg.Hostinfo{Hostname: "peer"}).View(),
		Addresses:  []netip.Prefix{netip.PrefixFrom(address, 128)},
		AllowedIPs: []netip.Prefix{netip.PrefixFrom(address, 128)},
		Endpoints:  []netip.AddrPort{endpoint},
	}}
	if !control.AddRawMapResponse(self.Self.PublicKey, response) {
		t.Fatal("node has no map stream")
	}
	// Wait for the ordinary observation to see the peer before testing writers.
	for {
		seen, err := node.Status(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(seen.PeerPaths) == 1 {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("node never observed the control-plane peer")
		case <-time.After(10 * time.Millisecond):
		}
	}
	return node, address.String(), endpoint.String()
}

func snapshotDERP(t *testing.T) *tailcfg.DERPMap {
	t.Helper()
	stun, closeSTUN := stuntest.Serve(t)
	t.Cleanup(closeSTUN)
	relay := derpserver.New(key.NewNode(), logger.Discard)
	server := httptest.NewTLSServer(derpserver.Handler(relay))
	t.Cleanup(func() { relay.Close(); server.Close() })
	return &tailcfg.DERPMap{Regions: map[int]*tailcfg.DERPRegion{
		1: {RegionID: 1, RegionCode: "test", Nodes: []*tailcfg.DERPNode{
			{Name: "test", RegionID: 1, HostName: "127.0.0.1", IPv4: "127.0.0.1", IPv6: "none",
				STUNPort: stun.Port, STUNTestIP: "127.0.0.1", DERPPort: server.Listener.Addr().(*net.TCPAddr).Port, InsecureForTests: true},
		}},
	}}
}
