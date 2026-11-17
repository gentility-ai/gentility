package enroll

import (
	"github.com/gentility-ai/gentility/connector/internal/version"
	"net/netip"
	"testing"
)

func developmentBuild(t *testing.T) {
	t.Helper()
	original := version.Connector
	version.Connector = "dev"
	t.Cleanup(func() { version.Connector = original })
}

func TestReleaseRefusesLoopbackGateway(t *testing.T) {
	original := version.Connector
	t.Cleanup(func() { version.Connector = original })
	for _, stamp := range []string{"dev", "0.2.0", "server-test", "phone-test"} {
		version.Connector = stamp
		for _, address := range []string{"127.0.0.1:443", "[::1]:443"} {
			addr := netip.MustParseAddrPort(address)
			_, _, err := Gateway(addr, "", false)
			if (err == nil) != (stamp == "dev") {
				t.Errorf("Gateway(%s) under %s: %v", address, stamp, err)
			}
			if Direct(addr) != (stamp == "dev") {
				t.Errorf("Direct(%s) under %s", address, stamp)
			}
		}
	}
}

func TestUnstampedBuildRefusesLoopbackGateway(t *testing.T) {
	if version.Connector == "dev" {
		t.Fatal("an unstamped build must not opt into development trust")
	}
	addr := netip.MustParseAddrPort("127.0.0.1:443")
	if _, _, err := Gateway(addr, "", false); err == nil {
		t.Fatal("unstamped build accepted a loopback gateway")
	}
	if Direct(addr) {
		t.Fatal("unstamped build accepted a direct hop")
	}
}
