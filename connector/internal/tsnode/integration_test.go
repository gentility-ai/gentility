//go:build overlay_integration

// Two in-process tsnet servers against a real head. NOT run by `go test ./...`
// — it needs a headscale, two pre-auth keys, DERP reachability and ~90 seconds.
// This measures multi-address behaviour again when the library pin moves.
//
//	OVERLAY_CONTROL_URL=… OVERLAY_AUTHKEY_A=… OVERLAY_AUTHKEY_B=… \
//	  go test -tags overlay_integration -timeout 5m ./internal/tsnode/
//
// Multi-org source selection and isolation run without credentials in
// TestMultiOrgConnections (multi_org_test.go), part of the ordinary suite.
package tsnode

import (
	"context"
	"net/netip"
	"os"
	"testing"
	"time"

	"tailscale.com/tsnet"
)

func upNode(t *testing.T, hostname, authKey string) *Node {
	t.Helper()

	srv := &tsnet.Server{
		Dir:        t.TempDir(),
		Hostname:   hostname,
		ControlURL: os.Getenv("OVERLAY_CONTROL_URL"),
		AuthKey:    authKey,
		Ephemeral:  true,
		Logf:       t.Logf,
	}
	t.Cleanup(func() { srv.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	if _, err := srv.Up(ctx); err != nil {
		t.Fatalf("%s: tsnet up: %v", hostname, err)
	}

	node, err := New(srv)
	if err != nil {
		t.Fatalf("%s: %v", hostname, err)
	}
	t.Cleanup(node.Close)

	return node
}

func TestTwoNodesTalkOverTheOverlayWithPinnedSources(t *testing.T) {
	for _, key := range []string{"OVERLAY_CONTROL_URL", "OVERLAY_AUTHKEY_A", "OVERLAY_AUTHKEY_B"} {
		if os.Getenv(key) == "" {
			t.Skipf("%s unset", key)
		}
	}

	ctx := context.Background()
	a := upNode(t, "tsnode-int-a", os.Getenv("OVERLAY_AUTHKEY_A"))
	b := upNode(t, "tsnode-int-b", os.Getenv("OVERLAY_AUTHKEY_B"))

	aAddrs, err := a.Addresses(ctx)
	if err != nil {
		t.Fatalf("a addresses: %v", err)
	}
	bAddrs, err := b.Addresses(ctx)
	if err != nil {
		t.Fatalf("b addresses: %v", err)
	}

	// Indexing these unchecked is how this test turns a control-plane problem
	// into a panic with no diagnosis. An empty set means the head gave the node
	// no address inside fd06:6e7::/48 — a head misconfiguration or a
	// netmap that has not landed yet, not a bug in tsnode.
	if len(aAddrs) == 0 || len(bAddrs) == 0 {
		t.Fatalf("a has %d overlay addresses and b has %d; the head gave at least one node no address in %s", len(aAddrs), len(bAddrs), ula)
	}

	if _, err := b.ListenAll(bAddrs, 8080); err != nil {
		t.Fatalf("b listen: %v", err)
	}

	a.SetGrants([]Grant{{Org: "test", Address: aAddrs[0]}})

	dialCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	conn, err := a.DialInOrg(dialCtx, netip.AddrPortFrom(bAddrs[0], 8080))
	if err != nil {
		t.Fatalf("a -> b: %v", err)
	}
	conn.Close()
}
