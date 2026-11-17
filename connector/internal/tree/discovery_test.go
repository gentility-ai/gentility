package tree

import (
	"encoding/pem"
	"errors"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/gentility-ai/gentility/connector/internal/tsnode"
)

func discoveryFixture(t *testing.T) Discovery {
	t.Helper()
	server := httptest.NewTLSServer(nil)
	defer server.Close()
	addr, err := netip.ParseAddrPort(server.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	return Discovery{Version: 1, Origin: "https://mcp.example", Gateway: netip.AddrPortFrom(tsnode.OverlayRange().Addr().Next(), addr.Port()).String(), CAPEM: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})), Organization: "acme", Interface: "test-overlay"}
}

// Override only the owner for unprivileged filesystem fixtures. Real mode and
// file-type checks still run through the production trust policy.
type ownerInfo struct {
	os.FileInfo
	uid uint32
}

func (i ownerInfo) Sys() any        { return &syscall.Stat_t{Uid: i.uid} }
func testTrust(i os.FileInfo) error { return DaemonTrust(ownerInfo{i, 0}) }
func TestDiscoveryTrust(t *testing.T) {
	for _, scenario := range []string{"valid", "writable file", "writable parent", "wrong owner", "symlink", "off overlay"} {
		t.Run(scenario, func(t *testing.T) {
			dir := testDir(t)
			d := discoveryFixture(t)
			if err := WriteDiscovery(dir, d, testTrust); err != nil {
				t.Fatal(err)
			}
			name := filepath.Join(dir, "discovery.json")
			trust := Trust(testTrust)
			switch scenario {
			case "writable file":
				if err := os.Chmod(name, 0666); err != nil {
					t.Fatal(err)
				}
			case "writable parent":
				if err := os.Chmod(dir, 0777); err != nil {
					t.Fatal(err)
				}
			case "wrong owner":
				trust = func(i os.FileInfo) error { return DaemonTrust(ownerInfo{i, ^uint32(0)}) }
			case "symlink":
				os.Rename(name, name+".original")
				if err := os.Symlink(name+".original", name); err != nil {
					t.Fatal(err)
				}
			case "off overlay":
				a, _ := netip.ParseAddrPort(d.Gateway)
				d.Gateway = netip.AddrPortFrom(netip.IPv6Loopback(), a.Port()).String()
				if _, err := d.validate(); err == nil {
					t.Fatal("accepted off-overlay gateway")
				}
				return
			}
			got, err := ReadDiscovery(dir, trust)
			if scenario == "valid" {
				if err != nil || got != d {
					t.Fatalf("%+v %v", got, err)
				}
			} else if err == nil {
				t.Fatal("trusted unsafe discovery")
			}
		})
	}
}
func TestDiscoveryNoCA(t *testing.T) {
	d := discoveryFixture(t)
	d.CAPEM = ""
	if _, err := d.validate(); err == nil {
		t.Fatal("accepted missing CA")
	}
	d.CAPEM = "invalid"
	if _, err := d.validate(); err == nil {
		t.Fatal("accepted invalid CA")
	}
}
func TestDiscoveryTrustFailureDoesNotReplace(t *testing.T) {
	dir := testDir(t)
	d := discoveryFixture(t)
	if err := WriteDiscovery(dir, d, func(os.FileInfo) error { return errors.New("refused") }); err == nil {
		t.Fatal("ignored trust failure")
	}
	if _, err := os.Stat(filepath.Join(dir, "discovery.json")); !os.IsNotExist(err) {
		t.Fatal(err)
	}
}

func TestDiscoveryTraversableWithoutListing(t *testing.T) {
	dir := testDir(t)
	d := discoveryFixture(t)
	if err := WriteDiscovery(dir, d, testTrust); err != nil {
		t.Fatal(err)
	}
	// Remove read permission even for the fixture owner to model the person
	// traversing the daemon's 0711 directory without requiring root in tests.
	if err := os.Chmod(dir, 0111); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(dir, 0700)
	got, err := ReadDiscovery(dir, testTrust)
	if err != nil || got != d {
		t.Fatalf("discovery in search-only directory: %+v %v", got, err)
	}
}
