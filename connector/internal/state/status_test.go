package state

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSaveStatusAndLoadStatusRoundTrip(t *testing.T) {
	store := &Store{Dir: t.TempDir()}

	want := Status{
		WrittenAt: time.Date(2026, 9, 6, 11, 30, 0, 0, time.UTC),
		Revision:  5,
		Relayed:   true,
		Endpoint:  "lhr",
		Interface: "utun4",
		Addresses: []string{"fd06:6e7:0:1::a"},
		Organizations: []OrgStatus{
			{Org: "acme", Address: "fd06:6e7:0:1::a", Label: "Acme"},
		},
		Services: []ServiceStatus{
			{Kind: "egress", Org: "acme", Port: 8443, State: "approved"},
		},
		Egress:         &EgressStatus{Org: "acme", Via: "fd06:6e7:0:1::a", Port: 8443},
		AdvertisedPort: 8443,
	}

	if err := store.SaveStatus(want); err != nil {
		t.Fatal(err)
	}

	got, err := store.LoadStatus()
	if err != nil {
		t.Fatalf("LoadStatus = %v, want the snapshot", err)
	}
	if !got.WrittenAt.Equal(want.WrittenAt) {
		t.Fatalf("WrittenAt = %v, want %v", got.WrittenAt, want.WrittenAt)
	}
	if got.Revision != 5 || !got.Relayed || got.Endpoint != "lhr" || got.AdvertisedPort != 8443 ||
		got.Interface != "utun4" {
		t.Fatalf("Status = %+v, want the one that was saved", got)
	}
	if len(got.Organizations) != 1 || got.Organizations[0].Address != "fd06:6e7:0:1::a" {
		t.Fatalf("Organizations = %+v, want acme's grant", got.Organizations)
	}
	if len(got.Services) != 1 || got.Services[0].State != "approved" {
		t.Fatalf("Services = %+v, want the approved egress", got.Services)
	}
	if got.Egress == nil || got.Egress.Port != 8443 {
		t.Fatalf("Egress = %+v, want acme's relay", got.Egress)
	}
}

// A userspace node holds no interface, and its snapshot says nothing about one
// rather than naming an empty one.
func TestStatusOmitsTheInterfaceInUserspaceMode(t *testing.T) {
	store := &Store{Dir: t.TempDir()}
	if err := store.SaveStatus(Status{WrittenAt: time.Now(), Revision: 5}); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(filepath.Join(store.Dir, statusFile))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "interface") {
		t.Fatalf("a userspace snapshot names an interface:\n%s", raw)
	}
}

// No snapshot is a state, not a failure: `serve` has never run against this
// directory, so `connector status` says that rather than exiting non-zero.
func TestLoadStatusWithoutASnapshotIsNoStatus(t *testing.T) {
	store := &Store{Dir: t.TempDir()}

	if _, err := store.LoadStatus(); !IsNoStatus(err) {
		t.Fatalf("LoadStatus on an empty directory = %v, want ErrNoStatus", err)
	}
}

// A snapshot with no instant on it is one nothing wrote: its age is the whole
// point, and a zero time would render as fifty-six years stale.
func TestLoadStatusRefusesASnapshotWithNoInstant(t *testing.T) {
	dir := t.TempDir()
	store := &Store{Dir: dir}
	if err := os.WriteFile(filepath.Join(dir, "status.json"), []byte(`{"revision":5}`), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := store.LoadStatus(); !IsNoStatus(err) {
		t.Fatalf("LoadStatus on a snapshot with no written_at = %v, want ErrNoStatus", err)
	}
}

// The three absences are three different answers — log in, enrol, start the
// daemon — so no sentinel may be mistaken for another.
func TestNoStatusIsNotTheOtherAbsences(t *testing.T) {
	for _, other := range []error{ErrNotEnrolled, ErrNoEnrollment, ErrNotLoggedIn} {
		if errors.Is(ErrNoStatus, other) || errors.Is(other, ErrNoStatus) {
			t.Fatalf("ErrNoStatus and %v are not distinct", other)
		}
	}
	if IsNotEnrolled(ErrNoStatus) || IsNoEnrollment(ErrNoStatus) || IsNotLoggedIn(ErrNoStatus) {
		t.Fatalf("ErrNoStatus answers another absence's predicate")
	}
}

// The daemon runs as root and the person reads its status as themselves, so
// status.json is the one file in the directory anyone else may open — and only
// by its exact name, because the directory is traversable but not listable.
func TestStatusIsReadableAndSecretsAreNot(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	s := &Store{Dir: dir}
	if err := s.SaveNode(Node{NodeID: "n1", NodeCredential: "secret"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveStatus(Status{WrittenAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	mode := func(p string) os.FileMode {
		info, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		return info.Mode().Perm()
	}
	if got := mode(dir); got != 0o711 {
		t.Fatalf("state dir %o, want 711", got)
	}
	if got := mode(filepath.Join(dir, "status.json")); got != 0o644 {
		t.Fatalf("status.json %o, want 644", got)
	}
	if got := mode(filepath.Join(dir, "node.json")); got != 0o600 {
		t.Fatalf("node.json %o, want 600", got)
	}
}

// The status is rewritten every thirty seconds; none of those writes may
// widen a secret that was already sitting in the directory.
func TestAStatusWriteDoesNotLoosenAnEarlierSecret(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	s := &Store{Dir: dir}
	if err := s.SaveEnrollment(Enrollment{EnrollmentID: "e", PreauthKey: "k", Nonce: "n"}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := s.SaveStatus(Status{WrittenAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	info, err := os.Stat(filepath.Join(dir, "enrollment.json"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("enrollment.json %o after status writes, want 600", info.Mode().Perm())
	}
}
