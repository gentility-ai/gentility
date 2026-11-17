package status

import (
	"encoding/json"
	"flag"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gentility-ai/gentility/connector/internal/state"
)

var update = flag.Bool("update", false, "rewrite the golden files from what Render produces")

// The four blocks a person reads. They are byte-exact on purpose: this is the
// one surface of the connector whose whole job is to be read, and a column that
// silently stops lining up is a regression nothing else would catch.
func TestRenderGoldens(t *testing.T) {
	cases := []struct {
		golden   string
		snapshot Snapshot
	}{
		{"direct", live()},
		{"relayed", relayed()},
		{"declared", declared()},
		{"interface", onInterface()},
		{"partial", partial()},
		{"stale", stale()},
		{"joined", joined()},
		{"enrolled", enrolled()},
		{"not_enrolled", notEnrolledSnapshot()},
		{"unreadable", unreadable()},
		{"mixed_paths", mixedPaths()},
		{"older_daemon", olderDaemon()},
		{"no_peers", noPeers()},
	}

	for _, c := range cases {
		t.Run(c.golden, func(t *testing.T) {
			path := filepath.Join("testdata", c.golden+".txt")
			got := Render(c.snapshot)

			if *update {
				if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}

			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if got != string(want) {
				t.Fatalf("Render mismatch for %s\n--- got ---\n%s\n--- want ---\n%s", c.golden, got, want)
			}
		})
	}
}

// A daemon from before the name was written down, or one whose netmap has not
// named it yet, leaves the line out rather than printing an empty one.
func TestRenderLeavesOutANameTheDaemonDidNotWrite(t *testing.T) {
	s := live()
	s.Name = ""
	if got := Render(s); strings.Contains(got, "\nname ") {
		t.Fatalf("a nameless snapshot rendered a name line:\n%s", got)
	}
}

// `--json` prints this same struct, so every field a person can read has to
// survive the trip out and back.
func TestSnapshotJSONRoundTrips(t *testing.T) {
	want := onInterface()

	encoded, err := json.MarshalIndent(want, "", "  ")
	if err != nil {
		t.Fatal(err)
	}

	var got Snapshot
	if err := json.Unmarshal(encoded, &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("round trip = %+v, want %+v\n%s", got, want, encoded)
	}
}

// A machine nobody has enrolled has not failed at anything, so Load answers a
// snapshot rather than an error and the command exits 0 over it.
func TestLoadOnAnEmptyStateDirectoryIsNotEnrolled(t *testing.T) {
	got, err := Load(&state.Store{Dir: t.TempDir()}, now())
	if err != nil {
		t.Fatalf("Load = %v, want a snapshot", err)
	}
	if got.State != NotEnrolled {
		t.Fatalf("State = %q, want %q", got.State, NotEnrolled)
	}
	for _, hint := range []string{"gentility net join", "connector login --server"} {
		if !strings.Contains(Render(got), hint) {
			t.Fatalf("the not-enrolled block does not say %q:\n%s", hint, Render(got))
		}
	}
}

// Enrolled but never joined: the material is on disk and no identity has been
// earned with it yet.
func TestLoadReadsAnEnrollmentThatHasNotBeenSpent(t *testing.T) {
	dir := t.TempDir()
	store := &state.Store{Dir: dir}
	if err := store.SaveEnrollment(state.Enrollment{
		EnrollmentID: "enr-1", PreauthKey: "shhh", Nonce: "shhh",
		HeadURL: "https://head.example",
	}); err != nil {
		t.Fatal(err)
	}

	got, err := Load(store, now())
	if err != nil {
		t.Fatal(err)
	}
	if got.State != Enrolled || got.EnrollmentID != "enr-1" {
		t.Fatalf("Snapshot = %+v, want the unspent enrollment", got)
	}
	if !strings.Contains(Render(got), "connector serve") {
		t.Fatalf("the enrolled block does not say what to type:\n%s", Render(got))
	}
}

// An identity with no snapshot beside it is a node that has joined and whose
// daemon is not running — the one state that looks like a failure and is not.
func TestLoadSaysSoWhenThereIsAnIdentityButNoSnapshot(t *testing.T) {
	dir := t.TempDir()
	store := &state.Store{Dir: dir}
	if err := store.SaveNode(state.Node{
		NodeID: "node-1", NodeCredential: "cred", LastRevision: 5,
		HeadURL: "https://head.example", Hostname: "relay-1",
	}); err != nil {
		t.Fatal(err)
	}

	got, err := Load(store, now())
	if err != nil {
		t.Fatal(err)
	}
	if got.State != Joined || got.NodeID != "node-1" || got.Revision != 5 {
		t.Fatalf("Snapshot = %+v, want the stored identity and no snapshot", got)
	}
	if !strings.Contains(Render(got), "the daemon has not written a status yet") {
		t.Fatalf("the joined block does not say the daemon is absent:\n%s", Render(got))
	}
}

// The live door: identity and snapshot both, and the age measured against the
// clock the caller passed rather than the one on the wall.
func TestLoadComposesTheLiveSnapshot(t *testing.T) {
	dir := t.TempDir()
	store := &state.Store{Dir: dir}
	if err := store.SaveNode(state.Node{
		NodeID: "node-1", NodeCredential: "cred", LastRevision: 4,
		HeadURL: "https://head.example", Hostname: "relay-1",
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveStatus(state.Status{
		WrittenAt: now().Add(-12 * time.Second),
		Revision:  5,
		Name:      "relay-1.acme.6net.io",
		Endpoint:  "100.64.0.1:41641",
		Interface: "utun4",
		Organizations: []state.OrgStatus{
			{Org: "acme", Address: "fd06:6e7:0:1::a", Label: "n1"},
		},
	}); err != nil {
		t.Fatal(err)
	}

	got, err := Load(store, now())
	if err != nil {
		t.Fatal(err)
	}
	if got.State != Live {
		t.Fatalf("State = %q, want %q", got.State, Live)
	}
	// The snapshot's revision, not node.json's cursor: the daemon writes the
	// document it just applied, and the cursor is only what a restart resumes
	// from.
	if got.Revision != 5 {
		t.Fatalf("Revision = %d, want the snapshot's 5", got.Revision)
	}
	if got.Age != 12*time.Second || got.Stale {
		t.Fatalf("Age = %v (stale %v), want 12s and fresh", got.Age, got.Stale)
	}
	if got.Interface != "utun4" {
		t.Fatalf("Interface = %q, want the snapshot's utun4", got.Interface)
	}
	if got.Name != "relay-1.acme.6net.io" {
		t.Fatalf("Name = %q, want the snapshot's 6net name", got.Name)
	}
}

// Older than three heartbeats and the daemon is not writing: the block says so
// rather than presenting a stale document as the current one.
func TestLoadFlagsASnapshotOlderThanTheHeartbeats(t *testing.T) {
	dir := t.TempDir()
	store := &state.Store{Dir: dir}
	if err := store.SaveNode(state.Node{
		NodeID: "node-1", NodeCredential: "cred", HeadURL: "https://head.example",
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveStatus(state.Status{
		WrittenAt: now().Add(-StaleAfter - time.Second), Revision: 5,
	}); err != nil {
		t.Fatal(err)
	}

	got, err := Load(store, now())
	if err != nil {
		t.Fatal(err)
	}
	if !got.Stale {
		t.Fatalf("Snapshot = %+v, want it flagged stale", got)
	}
	if !strings.Contains(Render(got), "stale") {
		t.Fatalf("a stale block does not say so:\n%s", Render(got))
	}
}

// A clock that went backwards is not a snapshot from the future.
func TestLoadClampsASnapshotWrittenAhead(t *testing.T) {
	dir := t.TempDir()
	store := &state.Store{Dir: dir}
	if err := store.SaveNode(state.Node{
		NodeID: "node-1", NodeCredential: "cred", HeadURL: "https://head.example",
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveStatus(state.Status{WrittenAt: now().Add(time.Hour), Revision: 5}); err != nil {
		t.Fatal(err)
	}

	got, err := Load(store, now())
	if err != nil {
		t.Fatal(err)
	}
	if got.Age != 0 || got.Stale {
		t.Fatalf("Age = %v (stale %v), want 0 and fresh", got.Age, got.Stale)
	}
}

func now() time.Time { return time.Date(2026, 9, 6, 11, 30, 0, 0, time.UTC) }

func live() Snapshot {
	return Snapshot{
		State:      Live,
		Connector:  "0.1.0",
		NodeCapver: 1,
		NodeID:     "node-1",
		Hostname:   "relay-1",
		HeadURL:    "https://head.example",
		Revision:   5,
		Age:        12 * time.Second,
		Name:       "cedar.morgan.acme.6net.io",
		Endpoint:   "100.64.0.1:41641",
		PeerPaths:  []state.PeerPath{{Address: "fd06:6e7:0:1::b", Path: "direct"}},
		Organizations: []state.OrgStatus{
			{Org: "acme", Address: "fd06:6e7:0:1::a", Label: "n1"},
			{Org: "widgets", Address: "fd06:6e7:0:2::a", Label: "n1"},
		},
		Services: []state.ServiceStatus{
			{Kind: "egress", Org: "acme", Port: 8443, State: "approved"},
		},
		Egress: &state.EgressStatus{Org: "acme", Via: "fd06:6e7:0:1::a", Port: 8443},
	}
}

func relayed() Snapshot {
	s := live()
	s.Relayed, s.Endpoint = true, "lhr"
	s.PeerPaths = []state.PeerPath{{Address: "fd06:6e7:0:1::b", Path: "derp", Relay: "lhr"}}
	return s
}

func mixedPaths() Snapshot {
	s := live()
	s.PathsIncomplete = true
	s.PeerPaths = []state.PeerPath{
		{Address: "fd06:6e7:0:1::b", Path: "direct"},
		{Address: "fd06:6e7:0:1::c", Path: "derp", Relay: "lhr"},
		{Address: "fd06:6e7:0:1::d", Path: "peer_relay"},
		{Address: "fd06:6e7:0:1::e", Path: "idle"},
		{Address: "fd06:6e7:0:1::f", Path: "unknown"},
	}
	return s
}

func olderDaemon() Snapshot {
	s := relayed()
	s.PeerPaths = nil
	return s
}

func noPeers() Snapshot {
	s := relayed()
	s.PeerPaths = []state.PeerPath{}
	return s
}

func TestPathUsesObservationsNotHomeRelay(t *testing.T) {
	for _, tc := range []struct {
		snapshot Snapshot
		want     string
	}{
		{olderDaemon(), "peer paths unavailable — update the Connector"},
		{noPeers(), "no peers observed"},
		{mixedPaths(), "1 direct, 2 relay, 1 idle, 1 unknown (incomplete)"},
		{stale(), "1 direct (stale observations — is the daemon still running?)"},
	} {
		if got := path(tc.snapshot); got != tc.want {
			t.Fatalf("path = %q, want %q", got, tc.want)
		}
	}
	s := live()
	s.Relayed, s.Endpoint = true, "lhr"
	if got := path(s); got != "1 direct" {
		t.Fatalf("home relay overrode the observed path: %s", got)
	}
}

func TestStaleStatusSaysSoOnceIncludingOlderDaemons(t *testing.T) {
	for _, snapshot := range []Snapshot{stale(), olderDaemon()} {
		snapshot.Stale = true
		if got := Render(snapshot); strings.Count(got, "stale") != 1 {
			t.Fatalf("expected one stale notice:\n%s", got)
		}
	}
}

func TestLoadKeepsPeerObservationsWithoutIdentityAccess(t *testing.T) {
	store := &state.Store{Dir: t.TempDir()}
	want := mixedPaths()
	if err := store.SaveStatus(state.Status{
		WrittenAt: now().Add(-2 * time.Minute), PeerPaths: want.PeerPaths,
		PathsIncomplete: true,
	}); err != nil {
		t.Fatal(err)
	}
	denying(t, true, false)
	got, err := Load(store, now())
	if err != nil {
		t.Fatal(err)
	}
	if !got.Stale || !got.Partial || !got.PathsIncomplete || !reflect.DeepEqual(got.PeerPaths, want.PeerPaths) {
		t.Fatalf("lost public observations or freshness: %+v", got)
	}
}

// A service nobody has approved yet: the node advertises it, the mothership
// records it, and nothing is served over it.
func declared() Snapshot {
	s := live()
	s.Services = []state.ServiceStatus{
		{Kind: "egress", Org: "acme", Port: 8443, State: "declared"},
	}
	s.Egress = nil
	return s
}

// A daemon in interface mode: the overlay is on a real device, and the block
// names it so a person can find it in ifconfig.
func onInterface() Snapshot {
	s := live()
	s.Interface = "utun4"
	return s
}

// What a person without sudo reads off a daemon's directory: status.json alone,
// with the identity it cannot open left out and said to be left out.
func partial() Snapshot {
	s := onInterface()
	s.NodeID, s.Hostname, s.HeadURL = "", "", ""
	s.Partial = true
	return s
}

// What a person without sudo reads off a daemon that has written no status
// they can read: nothing but the note.
func unreadable() Snapshot {
	s := notEnrolledSnapshot()
	s.State, s.Partial = Unreadable, true
	return s
}

func stale() Snapshot {
	s := live()
	s.Age, s.Stale = 4*time.Minute+12*time.Second, true
	return s
}

func joined() Snapshot {
	return Snapshot{
		State:      Joined,
		Connector:  "0.1.0",
		NodeCapver: 1,
		NodeID:     "node-1",
		Hostname:   "relay-1",
		HeadURL:    "https://head.example",
		Revision:   5,
	}
}

func enrolled() Snapshot {
	return Snapshot{
		State:        Enrolled,
		Connector:    "0.1.0",
		NodeCapver:   1,
		EnrollmentID: "enr-1",
		HeadURL:      "https://head.example",
		Revision:     state.NoRevision,
	}
}

func notEnrolledSnapshot() Snapshot {
	return Snapshot{
		State:      NotEnrolled,
		Connector:  "0.1.0",
		NodeCapver: 1,
		Revision:   state.NoRevision,
	}
}

// The age goes out as a number of seconds, because a time.Duration marshals as
// nanoseconds and `"age": 12000000000` is not the second this package measures.
func TestSnapshotJSONAgeIsSeconds(t *testing.T) {
	encoded, err := json.Marshal(Snapshot{State: Live, Revision: 3, Age: 12 * time.Second})
	if err != nil {
		t.Fatal(err)
	}

	var fields map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatal(err)
	}
	if got := string(fields["age_seconds"]); got != "12" {
		t.Fatalf("age_seconds = %s, want 12\n%s", got, encoded)
	}
	if _, ok := fields["age"]; ok {
		t.Fatalf("the nanosecond age is still on the wire:\n%s", encoded)
	}
}

// A snapshot nobody has aged yet says nothing about its age, the way every
// other omitempty field on the struct does.
func TestSnapshotJSONOmitsAZeroAge(t *testing.T) {
	encoded, err := json.Marshal(Snapshot{State: Joined, Revision: 3})
	if err != nil {
		t.Fatal(err)
	}

	var fields map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatal(err)
	}
	if _, ok := fields["age_seconds"]; ok {
		t.Fatalf("a zero age was written down:\n%s", encoded)
	}
}

// denying makes the named loader answer as a root-owned 0600 file answers a
// person: fs.ErrPermission. chmod 000 would not do it — root reads through it,
// and CI may run as root.
func denying(t *testing.T, node, enrollment bool) {
	t.Helper()
	previousNode, previousEnrollment := loadNode, loadEnrollment
	t.Cleanup(func() { loadNode, loadEnrollment = previousNode, previousEnrollment })
	if node {
		loadNode = func(*state.Store) (state.Node, error) {
			return state.Node{}, &fs.PathError{Op: "open", Path: "node.json", Err: fs.ErrPermission}
		}
	}
	if enrollment {
		loadEnrollment = func(*state.Store) (state.Enrollment, error) {
			return state.Enrollment{}, &fs.PathError{Op: "open", Path: "enrollment.json", Err: fs.ErrPermission}
		}
	}
}

// A person reading the daemon's directory cannot open node.json, and that is
// not a failure: status.json is theirs to read, and it is most of the answer.
func TestLoadWithAnUnreadableIdentityReadsTheSnapshotAlone(t *testing.T) {
	store := &state.Store{Dir: t.TempDir()}
	if err := store.SaveStatus(state.Status{
		WrittenAt: now().Add(-12 * time.Second), Revision: 5, Interface: "utun4",
	}); err != nil {
		t.Fatal(err)
	}
	denying(t, true, false)

	got, err := Load(store, now())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.State != Live || !got.Partial || got.Revision != 5 || got.Interface != "utun4" || got.NodeID != "" {
		t.Fatalf("snapshot = %+v, want a partial live one from status.json", got)
	}
	if !strings.Contains(Render(got), "identity details need sudo") {
		t.Fatalf("render does not say why the identity is missing:\n%s", Render(got))
	}
}

// An identity that cannot be read and no snapshot beside it says nothing
// about whether the node joined, so it is not reported as Joined: the status
// could not be read, and the render says so.
func TestLoadWithAnUnreadableIdentityAndNoSnapshotIsUnreadable(t *testing.T) {
	denying(t, true, false)

	got, err := Load(&state.Store{Dir: t.TempDir()}, now())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.State != Unreadable || !got.Partial || got.Revision != state.NoRevision {
		t.Fatalf("snapshot = %+v, want a partial unreadable one", got)
	}
	if !strings.Contains(Render(got), "note       the daemon's status could not be read") {
		t.Fatalf("render does not say the status could not be read:\n%s", Render(got))
	}
}

// The same when status.json is there and this account may not read it.
func TestLoadWithAnUnreadableIdentityAndAnUnreadableSnapshotIsUnreadable(t *testing.T) {
	denying(t, true, false)
	previous := loadStatus
	loadStatus = func(*state.Store) (state.Status, error) {
		return state.Status{}, &fs.PathError{Op: "open", Path: "status.json", Err: fs.ErrPermission}
	}
	t.Cleanup(func() { loadStatus = previous })

	got, err := Load(&state.Store{Dir: t.TempDir()}, now())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.State != Unreadable || !got.Partial {
		t.Fatalf("snapshot = %+v, want a partial unreadable one", got)
	}
}

// The same for an enrollment the person cannot open: it is there, unspent.
func TestLoadWithAnUnreadableEnrollmentIsEnrolled(t *testing.T) {
	denying(t, false, true)

	got, err := Load(&state.Store{Dir: t.TempDir()}, now())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.State != Enrolled || !got.Partial || got.EnrollmentID != "" {
		t.Fatalf("snapshot = %+v, want a partial enrolled one", got)
	}
	if !strings.Contains(Render(got), "identity details need sudo") {
		t.Fatalf("render does not say why the enrollment is missing:\n%s", Render(got))
	}
}

// --json says the snapshot is partial, and only when it is.
func TestSnapshotJSONCarriesPartial(t *testing.T) {
	encoded, err := json.Marshal(partial())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"partial":true`) {
		t.Fatalf("partial missing from %s", encoded)
	}
	var got Snapshot
	if err := json.Unmarshal(encoded, &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, partial()) {
		t.Fatalf("round trip = %+v, want %+v", got, partial())
	}

	whole, err := json.Marshal(live())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(whole), "partial") {
		t.Fatalf("a whole snapshot mentions partial: %s", whole)
	}
}
