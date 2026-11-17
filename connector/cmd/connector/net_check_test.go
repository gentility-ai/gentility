package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gentility-ai/gentility/connector/internal/state"
)

func TestNetCheckHelpAndAccessPrecedeAnyRead(t *testing.T) {
	asUID(t, 501)
	swap(t, &unitInstalled, func() bool { t.Fatal("help/refusal inspected the machine"); return false })
	for _, tc := range []struct {
		args     []string
		code     int
		contains string
	}{
		{[]string{"--help"}, 0, "net check"},
		{[]string{"--endpoints"}, 1, "private"},
	} {
		var out, err bytes.Buffer
		got := netCheck(tc.args, &out, &err)
		if got != tc.code || !strings.Contains(out.String()+err.String(), tc.contains) {
			t.Fatalf("got %d %s %s", got, &out, &err)
		}
	}
}

func TestNetCheckDoesNotExposePrivatePeersByDefault(t *testing.T) {
	asUID(t, 501)
	swap(t, &unitInstalled, func() bool { return false })
	s := &state.Store{Dir: t.TempDir()}
	if err := s.SaveStatus(state.Status{WrittenAt: time.Now(), UDPProbe: &state.UDPProbe{ObservedAt: time.Now(), UDP: true, GlobalV4: "192.0.2.1:5678"}}); err != nil {
		t.Fatal(err)
	}
	// Deliberately invalid private file: default reads must not even open it.
	if err := os.WriteFile(filepath.Join(s.Dir, "net-check.json"), []byte("not JSON: 198.51.100.7"), 0600); err != nil {
		t.Fatal(err)
	}
	var out, stderr bytes.Buffer
	if code := netCommand([]string{"check", "--state-dir", s.Dir, "--json"}, &out, &stderr); code != 0 {
		t.Fatalf("%d: %s", code, &stderr)
	}
	if strings.Contains(out.String(), "198.51.100.7") {
		t.Fatal("private endpoint exposed")
	}
	var got networkCheck
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Probe == nil || got.Probe.GlobalV4 != "192.0.2.1:5678" || got.Endpoints != nil {
		t.Fatalf("wrong public view: %+v", got)
	}
	// Root still sees the actual file error when explicitly requesting peers.
	asUID(t, 0)
	out.Reset()
	if code := netCheck([]string{"--state-dir", s.Dir, "--endpoints"}, &out, &stderr); code == 0 {
		t.Fatal("invalid private snapshot succeeded")
	}
}

func TestNetCheckOldProbeDoesNotBecomeFreshAtHeartbeat(t *testing.T) {
	swap(t, &unitInstalled, func() bool { return false })
	for _, tc := range []struct {
		probe *state.UDPProbe
		want  string
		code  int
	}{
		{nil, "unavailable", 1},
		{&state.UDPProbe{ObservedAt: time.Now().Add(-time.Hour), UDP: true}, "daemon probes again when it has traffic", 0},
		{&state.UDPProbe{}, "timestamp is invalid", 1},
		{&state.UDPProbe{ObservedAt: time.Now().Add(time.Hour)}, "timestamp is invalid", 1},
	} {
		s := &state.Store{Dir: t.TempDir()}
		if err := s.SaveStatus(state.Status{WrittenAt: time.Now(), UDPProbe: tc.probe}); err != nil {
			t.Fatal(err)
		}
		var out, stderr bytes.Buffer
		if code := netCheck([]string{"--state-dir", s.Dir}, &out, &stderr); code != tc.code || !strings.Contains(out.String(), tc.want) {
			t.Fatalf("%d %s %s", code, &out, &stderr)
		}
	}
}

func TestNetCheckTimestampDoesNotInventAnAge(t *testing.T) {
	now := time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		at   time.Time
		want string
	}{
		{time.Time{}, "timestamp unavailable"},
		{now.Add(time.Second), "2026-11-14T09:00:01Z (invalid future timestamp)"},
		{now.Add(-12 * time.Second), "2026-11-14T08:59:48Z (12s ago)"},
	} {
		if got := checkTime(tc.at, now); got != tc.want {
			t.Fatalf("got %q, want %q", got, tc.want)
		}
	}
}

func TestStaleCheckBoundaries(t *testing.T) {
	now := time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name  string
		at    time.Time
		stale bool
	}{
		{"zero", time.Time{}, true},
		{"future", now.Add(time.Nanosecond), true},
		{"now", now, false},
		{"before cutoff", now.Add(-90*time.Second + time.Nanosecond), false},
		{"at cutoff", now.Add(-90 * time.Second), true},
		{"after cutoff", now.Add(-90*time.Second - time.Nanosecond), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := staleCheck(tc.at, now); got != tc.stale {
				t.Fatalf("got %v, want %v", got, tc.stale)
			}
		})
	}
}

func TestNetCheckStaleWriterFailsEvenWithRecentProbe(t *testing.T) {
	swap(t, &unitInstalled, func() bool { return false })
	s := &state.Store{Dir: t.TempDir()}
	if err := s.SaveStatus(state.Status{WrittenAt: time.Now().Add(-time.Hour), UDPProbe: &state.UDPProbe{ObservedAt: time.Now()}}); err != nil {
		t.Fatal(err)
	}
	var out, stderr bytes.Buffer
	if code := netCheck([]string{"--state-dir", s.Dir}, &out, &stderr); code != 1 || !strings.Contains(out.String(), "Check that the Connector daemon is running") {
		t.Fatalf("%d %s %s", code, &out, &stderr)
	}
}
