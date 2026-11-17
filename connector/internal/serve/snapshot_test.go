package serve

import (
	"encoding/json"
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gentility-ai/gentility/connector/internal/config"
	"github.com/gentility-ai/gentility/connector/internal/report"
	"github.com/gentility-ai/gentility/connector/internal/state"
	"github.com/gentility-ai/gentility/connector/internal/tsnode"
	"github.com/gentility-ai/gentility/connector/internal/version"
)

func TestRecordPeerPathsNeverPublishesEndpoints(t *testing.T) {
	store := &state.Store{Dir: t.TempDir()}
	seen := tsnode.Report{
		PeerPaths: []tsnode.PeerPath{
			{Address: "fd06:6e7:0:1::b", Path: "direct", Via: "192.0.2.7:54321"},
			{Address: "fd06:6e7:0:1::c", Path: "derp", Relay: "lhr"},
			{Address: "fd06:6e7:0:1::d", Path: "idle"},
		},
		ObservationsErr:     errors.New("observation failed for 198.51.100.7"),
		PeerPathsIncomplete: true,
	}
	if err := record(store, fromDocument(applied()), seen, nil, time.Now()); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(store.Dir, "status.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, private := range []string{"192.0.2.7", "198.51.100.7", "54321"} {
		if strings.Contains(string(raw), private) {
			t.Fatalf("public status leaked %s", private)
		}
	}
	got, err := store.LoadStatus()
	if err != nil {
		t.Fatal(err)
	}
	want := []state.PeerPath{
		{Address: "fd06:6e7:0:1::b", Path: "direct"},
		{Address: "fd06:6e7:0:1::c", Path: "derp", Relay: "lhr"},
		{Address: "fd06:6e7:0:1::d", Path: "idle"},
	}
	if !got.PathsIncomplete || !reflect.DeepEqual(got.PeerPaths, want) {
		t.Fatalf("public paths = %+v, incomplete = %v", got.PeerPaths, got.PathsIncomplete)
	}
	if err := record(store, fromDocument(applied()), tsnode.Report{}, nil, time.Now()); err != nil {
		t.Fatal(err)
	}
	got, err = store.LoadStatus()
	if err != nil {
		t.Fatal(err)
	}
	if got.PeerPaths == nil || len(got.PeerPaths) != 0 || got.PathsIncomplete {
		t.Fatalf("empty observation retained old paths or looked like an old writer: %+v", got)
	}
}

func TestRecordSeparatesPublicProbeFromPrivatePeers(t *testing.T) {
	s := &state.Store{Dir: t.TempDir()}
	when := time.Now().Add(-time.Minute)
	seen := tsnode.Report{
		UDPProbe:        &tsnode.UDPProbe{ObservedAt: when, UDP: true, GlobalV4: "192.0.2.1:5678"},
		PeerDiagnostics: []tsnode.PeerDiagnostic{{PeerPath: tsnode.PeerPath{Address: "fd06:6e7:0:1::b", Path: "direct", Via: "198.51.100.7:4321"}, Endpoints: []string{"198.51.100.7:4321"}}},
		ObservationsErr: errors.New("local interfaces unavailable"),
	}
	if err := record(s, fromDocument(applied()), seen, nil, time.Now()); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(s.Dir, "status.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "198.51.100.7") || !strings.Contains(string(raw), "192.0.2.1") {
		t.Fatalf("wrong public summary: %s", raw)
	}
	public, err := s.LoadStatus()
	if err != nil {
		t.Fatal(err)
	}
	if !public.UDPProbe.ObservedAt.Equal(when) {
		t.Fatal("heartbeat refreshed probe timestamp")
	}
	private, err := s.LoadNetCheck()
	if err != nil {
		t.Fatal(err)
	}
	if private.Incomplete || public.PathsIncomplete {
		t.Fatal("interface failure made the peer list incomplete")
	}
	if len(private.Peers) != 1 || private.Peers[0].Via != "198.51.100.7:4321" {
		t.Fatalf("private peer lost: %+v", private)
	}
	seen.PeerPathsIncomplete = true
	if err := record(s, fromDocument(applied()), seen, nil, time.Now()); err != nil {
		t.Fatal(err)
	}
	private, err = s.LoadNetCheck()
	if err != nil || !private.Incomplete {
		t.Fatalf("peer truncation lost: %+v, %v", private, err)
	}
}

func TestRecordSurfacesPrivateWriteFailureWithoutSuppressingPublicStatus(t *testing.T) {
	s := &state.Store{Dir: t.TempDir()}
	if err := os.Mkdir(filepath.Join(s.Dir, "net-check.json"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := record(s, fromDocument(applied()), tsnode.Report{}, nil, time.Now()); err == nil {
		t.Fatal("private write failed silently")
	}
	if _, err := s.LoadStatus(); err != nil {
		t.Fatalf("public write suppressed: %v", err)
	}
}

// `connector status` is a separate process: what it can say about this daemon
// is exactly what this daemon wrote down when it applied a document.
func TestRecordWritesDownTheDocumentThatJustApplied(t *testing.T) {
	dir := t.TempDir()
	store := &state.Store{Dir: dir}
	written := time.Date(2026, 9, 6, 11, 30, 0, 0, time.UTC)

	err := record(store, fromDocument(applied()), tsnode.Report{
		Name:      "relay-1.acme.6net.io",
		Endpoint:  "100.64.0.1:41641",
		Interface: "utun4",
		Addresses: []netip.Addr{netip.MustParseAddr("fd06:6e7:0:1::a")},
	}, []report.Service{{Kind: "egress", Port: 8443}}, written)
	if err != nil {
		t.Fatalf("record = %v, want the snapshot written", err)
	}

	got, err := store.LoadStatus()
	if err != nil {
		t.Fatalf("LoadStatus after an apply = %v, want the snapshot", err)
	}
	if got.Revision != 5 {
		t.Fatalf("Revision = %d, want the applied document's 5", got.Revision)
	}
	if !got.WrittenAt.Equal(written) {
		t.Fatalf("WrittenAt = %v, want %v", got.WrittenAt, written)
	}
	if got.Relayed || got.Endpoint != "100.64.0.1:41641" {
		t.Fatalf("path = %+v, want the direct endpoint", got)
	}
	// The device the overlay is on, which is what says this daemon runs in
	// interface mode rather than userspace.
	if got.Interface != "utun4" {
		t.Fatalf("Interface = %q, want the device tsnet reported", got.Interface)
	}
	// The name the netmap gave this node, which is what `net join` announces
	// and `status` leads with.
	if got.Name != "relay-1.acme.6net.io" {
		t.Fatalf("Name = %q, want the name tsnet reported", got.Name)
	}
	if len(got.Addresses) != 1 || got.Addresses[0] != "fd06:6e7:0:1::a" {
		t.Fatalf("Addresses = %v, want the one tsnet reported", got.Addresses)
	}
	if len(got.Organizations) != 1 || got.Organizations[0].Org != "acme" {
		t.Fatalf("Organizations = %+v, want acme's grant", got.Organizations)
	}
	if len(got.Services) != 1 || got.Services[0].State != "approved" {
		t.Fatalf("Services = %+v, want the approved egress", got.Services)
	}
	if got.Egress == nil || got.Egress.Port != 8443 {
		t.Fatalf("Egress = %+v, want acme's relay", got.Egress)
	}
	// The declaration, which is not the permission: what the relay binds is the
	// approved service's port, and the two differing is worth being able to see.
	if got.AdvertisedPort != 8443 {
		t.Fatalf("AdvertisedPort = %d, want 8443", got.AdvertisedPort)
	}
}

// Desk reads status.json, not `connector status`, so the file itself must say
// which connector wrote it: the version -ldflags stamped, "dev" unstamped.
func TestRecordNamesTheConnectorVersionInStatusJSON(t *testing.T) {
	dir := t.TempDir()
	store := &state.Store{Dir: dir}

	if err := record(store, fromDocument(applied()), tsnode.Report{}, nil, time.Now()); err != nil {
		t.Fatalf("record = %v, want the snapshot written", err)
	}

	raw, err := os.ReadFile(filepath.Join(dir, "status.json"))
	if err != nil {
		t.Fatalf("reading status.json = %v", err)
	}
	var file map[string]any
	if err := json.Unmarshal(raw, &file); err != nil {
		t.Fatalf("status.json = %v, want JSON", err)
	}
	if file["connector"] != version.Connector {
		t.Fatalf("connector = %v, want %q", file["connector"], version.Connector)
	}
}

// The heartbeat writes the snapshot too, and it starts beating before a
// document has necessarily been applied. Revision 0 is a real document, so a
// node that holds none must write nothing rather than claim it holds the first.
func TestRecordWritesNothingBeforeTheFirstDocument(t *testing.T) {
	dir := t.TempDir()
	store := &state.Store{Dir: dir}

	if err := record(store, nothingApplied(), tsnode.Report{}, nil, time.Now()); err != nil {
		t.Fatalf("record = %v, want a silent no-op", err)
	}
	if _, err := store.LoadStatus(); !state.IsNoStatus(err) {
		t.Fatalf("LoadStatus = %v, want no snapshot at all", err)
	}
}

// A node with no egress selected does not fall back to one, and its snapshot
// must not invent one either.
func TestFromDocumentKeepsAnAbsentEgressAbsent(t *testing.T) {
	doc := applied()
	doc.Egress = nil

	if got := fromDocument(doc); got.Egress != nil {
		t.Fatalf("Egress = %+v, want nil", got.Egress)
	}
}

func applied() *config.Document {
	return &config.Document{
		Revision: 5,
		Grants: []tsnode.ConfigGrant{
			{Org: "acme", Address: "fd06:6e7:0:1::a", Label: "n1"},
		},
		Services: []config.Service{
			{Kind: "egress", Org: "acme", Port: 8443, State: "approved"},
		},
		Egress: &config.Egress{Org: "acme", Via: "fd06:6e7:0:1::a", Port: 8443},
	}
}

// Desk reads each organization's name from status.json, nested under
// organizations[] as "name" with no trailing dot. The netmap names the node in
// one organization; the base it shows is what every other one is built on.
func TestRecordNamesThisNodeInEveryOrganization(t *testing.T) {
	dir := t.TempDir()
	store := &state.Store{Dir: dir}
	doc := applied()
	doc.Grants = []tsnode.ConfigGrant{
		{Org: "northwind", Address: "fd06:6e7:0:2::4", Label: "driftwood-2.morgan"},
		{Org: "acme", Address: "fd06:6e7:0:1::a", Label: "driftwood.morgan"},
	}
	half := fromDocument(doc)

	err := record(store, half, tsnode.Report{Name: "driftwood-2.morgan.northwind.6net.io"}, nil, time.Now())
	if err != nil {
		t.Fatalf("record = %v, want the snapshot written", err)
	}

	raw, err := os.ReadFile(filepath.Join(dir, "status.json"))
	if err != nil {
		t.Fatalf("reading status.json = %v", err)
	}
	var file struct {
		Name          string `json:"name"`
		Organizations []struct {
			Org  string `json:"org"`
			Name string `json:"name"`
		} `json:"organizations"`
	}
	if err := json.Unmarshal(raw, &file); err != nil {
		t.Fatalf("status.json = %v, want JSON", err)
	}
	if file.Name != "driftwood-2.morgan.northwind.6net.io" {
		t.Fatalf("name = %q, want the netmap's name kept at the top", file.Name)
	}
	want := map[string]string{
		"northwind": "driftwood-2.morgan.northwind.6net.io",
		"acme":      "driftwood.morgan.acme.6net.io",
	}
	if len(file.Organizations) != 2 {
		t.Fatalf("organizations = %+v, want both grants", file.Organizations)
	}
	for _, org := range file.Organizations {
		if org.Name != want[org.Org] {
			t.Fatalf("%s name = %q, want %q", org.Org, org.Name, want[org.Org])
		}
	}
	// The document half is shared with the heartbeat; naming must not write
	// into it.
	for _, org := range half.Organizations {
		if org.Name != "" {
			t.Fatalf("the document half was written to: %+v", half.Organizations)
		}
	}
}

// Where the base cannot be told, no organization is named, and the key is left
// out so Desk falls back to the label.
func TestNamedOrganizationsLeavesTheNameOutWhenTheBaseCannotBeTold(t *testing.T) {
	grants := []state.OrgStatus{
		{Org: "northwind", Address: "fd06:6e7:0:2::4", Label: "cedar.morgan"},
		{Org: "acme", Address: "fd06:6e7:0:1::a"},
	}

	cases := map[string]string{
		"no name yet":            "",
		"a name no grant begins": "relay-1.acme.6net.io",
		"nothing after the org":  "cedar.morgan.northwind.",
		"the org with no base":   "cedar.morgan.northwind",
	}
	for why, self := range cases {
		for _, org := range namedOrganizations(self, grants) {
			if org.Name != "" {
				t.Fatalf("%s: %s name = %q, want none", why, org.Org, org.Name)
			}
		}
	}

	raw, err := json.Marshal(namedOrganizations("", grants)[0])
	if err != nil {
		t.Fatalf("marshal = %v", err)
	}
	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatalf("unmarshal = %v", err)
	}
	if _, present := fields["name"]; present {
		t.Fatalf("organization = %s, want no name key", raw)
	}
}

// A grant with no label has no name to build, even where another grant gave
// the base.
func TestNamedOrganizationsSkipsAGrantWithNoLabel(t *testing.T) {
	got := namedOrganizations("cedar.morgan.northwind.6net.io", []state.OrgStatus{
		{Org: "northwind", Label: "cedar.morgan"},
		{Org: "acme"},
	})
	if got[0].Name != "cedar.morgan.northwind.6net.io" || got[1].Name != "" {
		t.Fatalf("organizations = %+v, want northwind named and acme not", got)
	}
}
