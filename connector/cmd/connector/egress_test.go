package main

import (
	"net"
	"strconv"
	"testing"
)

// The preflight proxy's loopback bind: ephemeral by default for development
// and live checks, and a fixed port when the Builder pins
// OVERLAY_PROXY_PORT so its HTTPS_PROXY points at a port it chose.

func lookupFrom(env map[string]string) func(string) (string, bool) {
	return func(name string) (string, bool) {
		value, ok := env[name]
		return value, ok
	}
}

func TestProxyListenAddrDefaultsToEphemeral(t *testing.T) {
	addr, err := proxyListenAddr(lookupFrom(nil))
	if err != nil {
		t.Fatalf("proxyListenAddr: %v", err)
	}
	if addr != "127.0.0.1:0" {
		t.Fatalf("unset OVERLAY_PROXY_PORT gave %q, want the ephemeral 127.0.0.1:0", addr)
	}
}

func TestProxyListenAddrHonoursOverlayProxyPort(t *testing.T) {
	// Grab a port the OS will lend, release it, then prove proxyListenAddr binds
	// exactly it rather than an ephemeral one.
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("probing for a free port: %v", err)
	}
	port := probe.Addr().(*net.TCPAddr).Port
	_ = probe.Close()

	addr, err := proxyListenAddr(lookupFrom(map[string]string{
		"OVERLAY_PROXY_PORT": strconv.Itoa(port),
	}))
	if err != nil {
		t.Fatalf("proxyListenAddr: %v", err)
	}
	if want := net.JoinHostPort("127.0.0.1", strconv.Itoa(port)); addr != want {
		t.Fatalf("proxyListenAddr gave %q, want %q", addr, want)
	}

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("binding the pinned port %d: %v", port, err)
	}
	defer ln.Close()
	if got := ln.Addr().(*net.TCPAddr).Port; got != port {
		t.Fatalf("bound port %d, want the pinned %d", got, port)
	}
}

func TestProxyListenAddrRejectsAMalformedPort(t *testing.T) {
	for _, raw := range []string{"not-a-number", "0", "70000", "-1"} {
		if _, err := proxyListenAddr(lookupFrom(map[string]string{"OVERLAY_PROXY_PORT": raw})); err == nil {
			t.Fatalf("OVERLAY_PROXY_PORT=%q was accepted; want a clear error", raw)
		}
	}
}
