package egress

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strconv"
	"testing"
	"time"

	"github.com/gentility-ai/gentility/connector/internal/version"
)

func TestAdmitDecision(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer node-cred" {
			t.Errorf("Authorization = %q", got)
		}
		if got, want := r.Header.Get("X-Node-Capver"), strconv.Itoa(version.NodeCapver); got != want {
			t.Errorf("X-Node-Capver = %q, want %q", got, want)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"decision":"admit","fact_id":"f-1"}`))
	}))
	defer srv.Close()

	c := NewAdmitClient(srv.Client(), srv.URL, "node-cred")
	d, err := c.Admit(context.Background(), netip.MustParseAddr("fd06:6e7:0:1::b"), "example.com", 443)
	if err != nil {
		t.Fatalf("Admit: %v", err)
	}
	if !d.Admitted || d.FactID != "f-1" {
		t.Fatalf("decision = %+v", d)
	}
}

func TestAdmitRefusal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"decision":"refuse","reason":"not_allowlisted","fact_id":"f-2"}`))
	}))
	defer srv.Close()

	c := NewAdmitClient(srv.Client(), srv.URL, "node-cred")
	d, err := c.Admit(context.Background(), netip.MustParseAddr("fd06:6e7:0:1::b"), "evil.example", 443)
	if err != nil {
		t.Fatalf("a refusal is an answer, not an error: %v", err)
	}
	if d.Admitted || d.Reason != "not_allowlisted" {
		t.Fatalf("decision = %+v", d)
	}
}

// The admit call has a two-second budget. A timeout or unreachable server
// must refuse the connection.
func TestAdmitTimesOutIntoARefusal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(3 * time.Second)
		_, _ = w.Write([]byte(`{"decision":"admit","fact_id":"late"}`))
	}))
	defer srv.Close()

	c := NewAdmitClient(srv.Client(), srv.URL, "node-cred")
	c.Timeout = 200 * time.Millisecond

	start := time.Now()
	d, err := c.Admit(context.Background(), netip.MustParseAddr("fd06:6e7:0:1::b"), "example.com", 443)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("a timed-out admit must be an error the caller turns into a refusal")
	}
	if d.Admitted {
		t.Fatal("a timed-out admit must never be admitted")
	}
	if elapsed > time.Second {
		t.Fatalf("admit waited %v, past its own budget", elapsed)
	}
}

func TestAdmitUnreachableIsNotAdmitted(t *testing.T) {
	c := NewAdmitClient(http.DefaultClient, "http://127.0.0.1:1", "node-cred")
	c.Timeout = 200 * time.Millisecond

	d, err := c.Admit(context.Background(), netip.MustParseAddr("fd06:6e7:0:1::b"), "example.com", 443)
	if err == nil || d.Admitted {
		t.Fatalf("unreachable mothership must refuse: err=%v decision=%+v", err, d)
	}
}

func TestAdmitNon200IsNotAdmitted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"decision":"admit","fact_id":"nope"}`))
	}))
	defer srv.Close()

	c := NewAdmitClient(srv.Client(), srv.URL, "node-cred")
	d, err := c.Admit(context.Background(), netip.MustParseAddr("fd06:6e7:0:1::b"), "example.com", 443)
	if err == nil || d.Admitted {
		t.Fatalf("a non-200 must refuse even if its body says admit: err=%v decision=%+v", err, d)
	}
}

func TestAdmitMalformedBodyIsNotAdmitted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"decision":"adm`)) // truncated JSON
	}))
	defer srv.Close()

	c := NewAdmitClient(srv.Client(), srv.URL, "node-cred")
	d, err := c.Admit(context.Background(), netip.MustParseAddr("fd06:6e7:0:1::b"), "example.com", 443)
	if err == nil || d.Admitted {
		t.Fatalf("an unreadable answer must refuse: err=%v decision=%+v", err, d)
	}
}
