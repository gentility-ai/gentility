package state

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func issued() Enrollment {
	return Enrollment{
		EnrollmentID: "0198f0a2-0000-7000-8000-0000000000e1",
		PreauthKey:   "preauth-shhh",
		Nonce:        "nonce-shhh",
		HeadURL:      "https://head.example.test",
		ExpiresAt:    "2026-09-06T12:00:00Z",
		GatewayAddr:  "fd06:6e7:0:1::1",
		GatewayPort:  443,
		GatewayCA:    "-----BEGIN CERTIFICATE-----",
	}
}

func TestSaveEnrollmentRoundTrips(t *testing.T) {
	s := &Store{Dir: filepath.Join(t.TempDir(), "gentility")}
	want := issued()

	if err := s.SaveEnrollment(want); err != nil {
		t.Fatalf("SaveEnrollment: %v", err)
	}
	got, err := s.LoadEnrollment()
	if err != nil {
		t.Fatalf("LoadEnrollment: %v", err)
	}
	if got != want {
		t.Fatalf("LoadEnrollment() = %+v, want %+v", got, want)
	}
}

func TestSaveEnrollmentWritesOwnerOnlyFileInOwnerOnlyDir(t *testing.T) {
	s := &Store{Dir: filepath.Join(t.TempDir(), "gentility")}

	if err := s.SaveEnrollment(issued()); err != nil {
		t.Fatalf("SaveEnrollment: %v", err)
	}

	fi, err := os.Stat(filepath.Join(s.Dir, enrollmentFile))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if got := fi.Mode().Perm(); got != 0o600 {
		t.Fatalf("%s mode = %o, want 600", enrollmentFile, got)
	}

	di, err := os.Stat(s.Dir)
	if err != nil {
		t.Fatalf("stat dir: %v", err)
	}
	if got := di.Mode().Perm(); got != 0o711 {
		t.Fatalf("dir mode = %o, want 711", got)
	}
}

// "No enrollment material" and "no node credential" are different states with
// different answers — log in again versus exchange — so they must not be the
// same sentinel.
func TestLoadEnrollmentMissingIsErrNoEnrollmentAndNotErrNotEnrolled(t *testing.T) {
	s := &Store{Dir: filepath.Join(t.TempDir(), "gentility")}

	_, err := s.LoadEnrollment()
	if !IsNoEnrollment(err) {
		t.Fatalf("want ErrNoEnrollment, got %v", err)
	}
	if IsNotEnrolled(err) {
		t.Fatalf("ErrNoEnrollment must not be ErrNotEnrolled: %v", err)
	}
}

func TestClearEnrollmentRemovesTheFileAndIsIdempotent(t *testing.T) {
	s := &Store{Dir: filepath.Join(t.TempDir(), "gentility")}
	if err := s.SaveEnrollment(issued()); err != nil {
		t.Fatal(err)
	}

	if err := s.ClearEnrollment(); err != nil {
		t.Fatalf("ClearEnrollment: %v", err)
	}
	if _, err := os.Stat(filepath.Join(s.Dir, enrollmentFile)); !os.IsNotExist(err) {
		t.Fatalf("%s survived ClearEnrollment: %v", enrollmentFile, err)
	}
	if err := s.ClearEnrollment(); err != nil {
		t.Fatalf("second ClearEnrollment: %v", err)
	}
}

func TestEnrollmentStringRedactsBothSecrets(t *testing.T) {
	enrollment := issued()

	for _, rendered := range []string{
		fmt.Sprintf("%v", enrollment),
		fmt.Sprintf("%v", &enrollment),
		fmt.Sprintf("%s", enrollment),
		enrollment.String(),
	} {
		if strings.Contains(rendered, enrollment.PreauthKey) {
			t.Fatalf("rendered enrollment leaks the preauth key: %s", rendered)
		}
		if strings.Contains(rendered, enrollment.Nonce) {
			t.Fatalf("rendered enrollment leaks the nonce: %s", rendered)
		}
		if !strings.Contains(rendered, enrollment.EnrollmentID) {
			t.Fatalf("rendered enrollment does not name the enrollment: %s", rendered)
		}
	}
}
