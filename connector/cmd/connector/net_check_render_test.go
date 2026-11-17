package main

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gentility-ai/gentility/connector/internal/state"
	"github.com/gentility-ai/gentility/connector/internal/tsnode"
)

var updateNetCheck = flag.Bool("update-net-check", false, "update network check CLI fixtures")

func TestNetCheckRenderFixtures(t *testing.T) {
	now := time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC)
	fresh := networkCheck{ReadAt: now, WrittenAt: now.Add(-5 * time.Second), Probe: &state.UDPProbe{
		ObservedAt: now.Add(-20 * time.Second), UDP: true, IPv4: true, GlobalV4: "192.0.2.4:41641",
	}}
	fresh.PeerPaths = []state.PeerPath{
		{Address: "fd06:6e7:0:1::b", Path: "direct"},
		{Address: "fd06:6e7:0:1::c", Path: "derp", Relay: "test"},
	}
	negative := fresh
	negative.Probe = &state.UDPProbe{ObservedAt: fresh.Probe.ObservedAt}
	missing := fresh
	missing.Probe = nil
	missing.PeerPaths = nil
	idle := fresh
	idle.Probe = &state.UDPProbe{ObservedAt: now.Add(-5 * time.Minute), UDP: true}
	stale := fresh
	stale.WrittenAt = now.Add(-5 * time.Minute)
	stale.Stale = true
	stale.Probe = &state.UDPProbe{ObservedAt: now.Add(-5 * time.Minute), UDP: true, GlobalV4: "192.0.2.4:41641"}
	private := fresh
	private.Endpoints = &state.NetCheck{WrittenAt: fresh.WrittenAt, Peers: []tsnode.PeerDiagnostic{
		{PeerPath: tsnode.PeerPath{Address: "fd06:6e7:0:1::b", Path: "direct", Via: "192.0.2.5:41641"}, Endpoints: []string{"192.0.2.5:41641", "192.0.2.6:41641"}},
		{PeerPath: tsnode.PeerPath{Address: "fd06:6e7:0:1::c", Path: "derp", Relay: "test"}, Endpoints: []string{}},
	}}
	unavailableCandidates := private
	unavailableCandidates.Endpoints = &state.NetCheck{WrittenAt: fresh.WrittenAt, Peers: []tsnode.PeerDiagnostic{
		{PeerPath: tsnode.PeerPath{Address: "fd06:6e7:0:1::b", Path: "idle"}, EndpointsUnavailable: true},
	}}
	for name, check := range map[string]networkCheck{"candidates_unavailable": unavailableCandidates, "fresh": fresh, "idle": idle, "negative": negative, "unavailable": missing, "stale": stale, "endpoints": private} {
		t.Run(name, func(t *testing.T) {
			var out bytes.Buffer
			renderNetCheck(&out, check)
			path := filepath.Join("testdata", "net_check_"+name+".txt")
			if *updateNetCheck {
				if err := os.WriteFile(path, out.Bytes(), 0644); err != nil {
					t.Fatal(err)
				}
				return
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if out.String() != string(want) {
				t.Fatalf("render changed:\n%s\nwant:\n%s", &out, want)
			}
		})
	}
}
