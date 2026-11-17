package tsnode

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http/httptest"
	"net/netip"
	"slices"
	"testing"
	"time"

	"tailscale.com/derp/derpserver"
	"tailscale.com/net/netns"
	"tailscale.com/net/stun/stuntest"
	"tailscale.com/tailcfg"
	"tailscale.com/tsnet"
	"tailscale.com/tstest/integration/testcontrol"
	"tailscale.com/types/key"
	"tailscale.com/types/logger"
)

// Real netstacks and encrypted transport, with a local control plane and DERP.
// This deliberately keeps both addresses on one node throughout the test:
// testing the organizations separately would miss tsnet's primary-address bug.
func TestMultiOrgConnections(t *testing.T) {
	netns.SetEnabled(false)
	t.Cleanup(func() { netns.SetEnabled(true) })
	control := &testcontrol.Server{
		DERPMap:   multiOrgDERP(t),
		AllOnline: true, Logf: t.Logf,
	}
	control.HTTPTestServer = httptest.NewServer(control)
	t.Cleanup(control.HTTPTestServer.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	addresses := [][]netip.Addr{
		{addr(t, "fd06:6e7:0:1::1"), addr(t, "fd06:6e7:0:2::1")},
		{addr(t, "fd06:6e7:0:1::2")},
		{addr(t, "fd06:6e7:0:2::2")},
	}
	nodes := make([]*Node, 3)
	records := make([]*tailcfg.Node, 3)
	for i := range nodes {
		srv := &tsnet.Server{Dir: t.TempDir(), Hostname: fmt.Sprintf("multi-org-%d", i),
			ControlURL: control.HTTPTestServer.URL, AuthKey: "test", Logf: t.Logf}
		t.Cleanup(func() { srv.Close() })
		st, err := srv.Up(ctx)
		if err != nil {
			t.Fatal(err)
		}
		nodes[i], err = New(srv)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(nodes[i].Close)
		records[i] = control.Node(st.Self.PublicKey)
	}
	// Up means registered, not that discovery has advertised a usable path.
	// Raw maps suppress later updates, so wait before freezing the peer map.
	for i, record := range records {
		for {
			current := control.Node(record.Key)
			if current.HomeDERP != 0 && len(current.Endpoints) > 0 {
				records[i] = current
				break
			}
			select {
			case <-ctx.Done():
				t.Fatalf("node %d has not advertised its paths: DERP=%d endpoints=%v", i, current.HomeDERP, current.Endpoints)
			case <-time.After(10 * time.Millisecond):
			}
		}
	}
	// Diagnostics must come from these engines' actual sockets, not merely
	// from a populated fake Report. The local STUN service answered discovery.
	for i, node := range nodes {
		observed, err := node.Status(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if observed.UDPProbe == nil || !observed.UDPProbe.UDP || observed.UDPProbe.ObservedAt.IsZero() {
			t.Fatalf("node %d lost its engine's STUN observation: %+v", i, observed.UDPProbe)
		}
	}
	// Freeze a complete world after registration. Each destination accepts only
	// its own org's source prefix. Peers remain visible so the negative checks
	// exercise the packet filter rather than merely a missing route.
	for i, record := range records {
		record.Addresses = nil
		for _, a := range addresses[i] {
			record.Addresses = append(record.Addresses, netip.PrefixFrom(a, 128))
		}
		record.AllowedIPs = slices.Clone(record.Addresses)
	}
	for i, record := range records {
		response, err := control.MapResponse(&tailcfg.MapRequest{NodeKey: record.Key})
		if err != nil {
			t.Fatal(err)
		}
		// Rewrite only this response. Updating the server with an earlier node
		// snapshot could overwrite newly advertised discovery information.
		for _, peer := range response.Peers {
			for _, granted := range records {
				if peer.Key == granted.Key {
					peer.Addresses = slices.Clone(granted.Addresses)
					peer.AllowedIPs = slices.Clone(granted.AllowedIPs)
				}
			}
		}
		response.Node.Addresses = slices.Clone(record.Addresses)
		response.Node.AllowedIPs = slices.Clone(record.AllowedIPs)
		response.PacketFilter = nil
		var grants []Grant
		for _, a := range addresses[i] {
			prefix, err := OrgPrefix(a)
			if err != nil {
				t.Fatal(err)
			}
			response.PacketFilter = append(response.PacketFilter, tailcfg.FilterRule{
				SrcIPs:   []string{prefix.String()},
				DstPorts: []tailcfg.NetPortRange{{IP: a.String(), Ports: tailcfg.PortRange{First: 8080, Last: 8080}}},
			})
			grants = append(grants, Grant{Org: prefix.String(), Address: a})
		}
		if !control.AddRawMapResponse(record.Key, response) {
			t.Fatal("node has no map stream")
		}
		nodes[i].SetGrants(grants)
	}
	for i, node := range nodes {
		for {
			got, err := node.Addresses(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if slices.Equal(got, addresses[i]) {
				break
			}
			select {
			case <-ctx.Done():
				t.Fatalf("node %d addresses = %v, want %v", i, got, addresses[i])
			case <-time.After(10 * time.Millisecond):
			}
		}
		observed, err := node.DiagnosticStatus(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(observed.PeerDiagnostics) == 0 {
			t.Fatalf("node %d has no peer diagnostics", i)
		}
		for _, peer := range observed.PeerDiagnostics {
			if len(peer.Endpoints) == 0 {
				t.Fatalf("node %d lost advertised candidates for %s", i, peer.Address)
			}
		}
		listeners, err := node.ListenAll(addresses[i], 8080)
		if err != nil {
			t.Fatal(err)
		}
		for _, ln := range listeners {
			go func(ln net.Listener) {
				for {
					conn, err := ln.Accept()
					if err != nil {
						return
					}
					// Report what the receiver actually saw, not what the caller asked for.
					fmt.Fprintln(conn, conn.RemoteAddr().(*net.TCPAddr).AddrPort().Addr())
					conn.Close()
				}
			}(ln)
		}
	}
	check := func(t *testing.T, from int, destination, source netip.Addr) {
		t.Helper()
		dialCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
		defer cancel()
		conn, err := nodes[from].DialInOrg(dialCtx, netip.AddrPortFrom(destination, 8080))
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		conn.SetDeadline(time.Now().Add(5 * time.Second))
		got, err := io.ReadAll(conn)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != source.String()+"\n" {
			t.Fatalf("peer saw source %q, want %s", got, source)
		}
	}
	for org := range 2 {
		t.Run(fmt.Sprintf("org_%d", org), func(t *testing.T) {
			check(t, 0, addresses[org+1][0], addresses[0][org])
			check(t, org+1, addresses[0][org], addresses[org+1][0])
		})
	}
	// DialBound deliberately bypasses source selection using another address
	// the dual-org node really owns. The destination must drop that traffic.
	for org := range 2 {
		t.Run(fmt.Sprintf("wrong_source_%d", org), func(t *testing.T) {
			deniedCtx, cancel := context.WithTimeout(ctx, time.Second)
			defer cancel()
			conn, err := nodes[0].DialBound(deniedCtx, addresses[0][1-org], netip.AddrPortFrom(addresses[org+1][0], 8080))
			if conn != nil {
				conn.Close()
			}
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("cross-org dial = %v, want packet-filter timeout", err)
			}
			check(t, 0, addresses[org+1][0], addresses[0][org])
		})
	}
}

// Keep the test's relay local; no public DERP or external credentials.
func multiOrgDERP(t *testing.T) *tailcfg.DERPMap {
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
