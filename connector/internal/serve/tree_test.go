package serve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"reflect"
	"testing"
	"time"

	"github.com/gentility-ai/gentility/connector/internal/config"
	"github.com/gentility-ai/gentility/connector/internal/state"
	"github.com/gentility-ai/gentility/connector/internal/tree"
	"github.com/gentility-ai/gentility/connector/internal/tsnode"
)

func TestDiscoveryUsesBackfilledIdentityAndRetriesWrite(t *testing.T) {
	store := &state.Store{Dir: t.TempDir()}
	startup := state.Node{NodeID: "node", NodeCredential: "secret", LastRevision: state.NoRevision}
	if err := store.SaveNode(startup); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(nil)
	defer server.Close()
	endpoint, _ := netip.ParseAddrPort(server.Listener.Addr().String())
	document := &config.Document{Origin: "https://mcp.example", Grants: []tsnode.ConfigGrant{{Org: "acme", Address: "fd06:6e7:0:1::a"}}, Gateway: config.Gateway{Address: "fd06:6e7:0:1::1", Port: int(endpoint.Port())}}
	node := new(tsnode.Node)
	attendance := Attendance{Store: store, Identity: startup, Session: Session{Node: node}}
	var published tree.Discovery
	writeErr := errors.New("disk unavailable")
	tracker := treeAttendance{writeDiscovery: func(_ string, d tree.Discovery, _ tree.Trust) error {
		if writeErr != nil {
			return writeErr
		}
		published = d
		return nil
	}}
	// This test exercises discovery independently of the background tree fetch.
	tracker.once.Do(func() {})
	apply := func() error {
		if err := document.Adopt(node, store); err != nil {
			return err
		}
		return tracker.apply(context.Background(), attendance, document, "", nil, func(string, ...any) {})
	}
	if err := apply(); !errors.Is(err, writeErr) {
		t.Fatalf("write failure lost: %v", err)
	}
	saved, _ := store.LoadNode()
	if saved.Origin != document.Origin || published.Origin != "" {
		t.Fatal("backfill or refusal lost")
	}
	writeErr = nil
	document.Grants[0].OrgID = "019a3f00-0000-7000-8000-000000000001"
	if err := apply(); err != nil {
		t.Fatal(err)
	}
	if published.Origin != document.Origin || attendance.Identity.Origin != "" {
		t.Fatal("discovery used startup identity")
	}
	if published.OrganizationID != document.Grants[0].OrgID || tracker.source.OrganizationID != published.OrganizationID {
		t.Fatal("authenticated grant ID did not reach discovery and daemon")
	}
	document.Grants[0].Org = "renamed"
	if err := apply(); err != nil {
		t.Fatal(err)
	}
	if published.Organization != "renamed" || tracker.source.Organization != "renamed" {
		t.Fatal("daemon source froze the first config slug")
	}
	document.Origin = "https://other.example"
	if err := apply(); err == nil {
		t.Fatal("accepted mismatched identity")
	}
	if published.Origin != saved.Origin {
		t.Fatal("mismatch changed discovery")
	}
}

func TestDiscoveryFailureDoesNotBlockReadyAndRetriesUnchanged(t *testing.T) {
	store := &state.Store{Dir: t.TempDir()}
	identity := state.Node{NodeID: "node", NodeCredential: "test-credential", LastRevision: state.NoRevision}
	if err := store.SaveNode(identity); err != nil {
		t.Fatal(err)
	}
	doc := &config.Document{RendererVersion: config.RendererVersion, Revision: 1, NodeID: "node", Origin: "https://mcp.example", Grants: []tsnode.ConfigGrant{{Org: "acme", Address: "fd06:6e7:0:1::a"}}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("since") == "1" {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		_ = json.NewEncoder(w).Encode(doc)
	}))
	defer server.Close()
	endpoint, _ := netip.ParseAddrPort(server.Listener.Addr().String())
	doc.Gateway = config.Gateway{Address: "fd06:6e7:0:1::1", Port: int(endpoint.Port())}
	attendance := Attendance{Store: store, Identity: identity, Session: Session{Node: new(tsnode.Node)}}
	attempts := 0
	tracker := treeAttendance{writeDiscovery: func(string, tree.Discovery, tree.Trust) error { attempts++; return errors.New("untrusted directory") }}
	tracker.once.Do(func() {})
	client := config.New(server.URL, identity, store, server.Client())
	client.Log = t.Logf
	ctx, cancel := context.WithTimeout(context.Background(), time.Second*5)
	defer cancel()
	refresh := func() { _ = tracker.apply(ctx, attendance, doc, server.URL, server.Client(), func(string, ...any) {}) }
	client.OnUnchanged = func() {
		if !client.Ready() {
			t.Error("discovery failure blocked readiness")
		}
		refresh()
		cancel()
	}
	_ = client.Run(ctx, func(d *config.Document) error {
		if err := d.Adopt(attendance.Session.Node, store); err != nil {
			return err
		}
		refresh()
		return nil
	})
	if !client.Ready() || attempts < 2 {
		t.Fatalf("ready=%v attempts=%d", client.Ready(), attempts)
	}
}

func TestDiscoveryLogsOnlyStateChanges(t *testing.T) {
	store := &state.Store{Dir: t.TempDir()}
	identity := state.Node{NodeID: "node", NodeCredential: "test-credential"}
	if err := store.SaveNode(identity); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(nil)
	defer server.Close()
	endpoint, _ := netip.ParseAddrPort(server.Listener.Addr().String())
	doc := &config.Document{Grants: []tsnode.ConfigGrant{{Org: "acme", Address: "fd06:6e7:0:1::a"}}, Gateway: config.Gateway{Address: "fd06:6e7:0:1::1", Port: int(endpoint.Port())}}
	attendance := Attendance{Store: store, Session: Session{Node: new(tsnode.Node)}}
	attempts := 0
	var writeErr error
	tracker := treeAttendance{writeDiscovery: func(string, tree.Discovery, tree.Trust) error { attempts++; return writeErr }}
	tracker.once.Do(func() {})
	var logs []string
	apply := func() {
		_ = tracker.apply(context.Background(), attendance, doc, "", nil, func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) })
	}
	apply()
	apply()
	identity.Origin = "https://mcp.example"
	if err := store.SaveNode(identity); err != nil {
		t.Fatal(err)
	}
	writeErr = errors.New("disk unavailable")
	apply()
	apply()
	writeErr = errors.New("directory untrusted")
	apply()
	apply()
	writeErr = nil
	apply()
	apply()
	writeErr = errors.New("disk unavailable")
	apply()
	want := []string{
		"tree discovery skipped: node node has no login origin yet",
		"tree discovery: disk unavailable; retrying on the next config poll",
		"tree discovery: directory untrusted; retrying on the next config poll",
		"tree discovery: previous refusal cleared",
		"tree discovery: disk unavailable; retrying on the next config poll",
	}
	if !reflect.DeepEqual(logs, want) {
		t.Fatalf("logs = %q, want %q", logs, want)
	}
	if attempts != 7 {
		t.Fatalf("write attempts = %d, want 7", attempts)
	}
}
