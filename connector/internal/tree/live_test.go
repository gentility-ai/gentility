package tree

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

// TestLocalPhoenix is opt-in: its inputs are supplied in memory by the local
// proof driver. It exercises the real listener's development loopback ingress;
// it does not claim TLS or two-host overlay verification.
func TestLocalPhoenix(t *testing.T) {
	base := os.Getenv("TREE_PROOF_URL")
	if base == "" {
		t.Skip("local Phoenix proof only")
	}
	root := os.Getenv("TREE_PROOF_ROOT")
	mode := os.Getenv("TREE_PROOF_MODE")
	transport := &peerTransport{peer: os.Getenv("TREE_PROOF_PEER")}
	client := &http.Client{Transport: transport}
	principal := mode
	if mode == "refused" {
		principal = "node"
	}
	s, err := Open(Options{GroupID: func() (int, error) { return os.Getegid(), nil }, Root: root, StateDir: root + "-state", Principal: principal, Source: func(context.Context) (Source, error) {
		return Source{BaseURL: base, Bearer: os.Getenv("TREE_PROOF_BEARER"), Identity: "local-proof:" + mode, Organization: os.Getenv("TREE_PROOF_ORG"), HTTP: client}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Once(context.Background()); mode == "refused" {
		var response *HTTPError
		if !errors.As(err, &response) || response.Status != http.StatusForbidden {
			t.Fatalf("expected user-node 403, got %v", err)
		}
		return
	} else if err != nil {
		t.Fatal(err)
	}
	if err := s.Once(context.Background()); err != nil {
		t.Fatal(err)
	}
	if transport.status != http.StatusNotModified {
		t.Fatalf("conditional manifest status %d", transport.status)
	}
	name := filepath.Join(root, os.Getenv("TREE_PROOF_ORG"), "notes", os.Getenv("TREE_PROOF_NOTE"))
	body, err := os.ReadFile(name)
	want := os.Getenv("TREE_PROOF_BODY")
	if want == "removed" {
		if !os.IsNotExist(err) {
			t.Fatal("archived file remains")
		}
	} else if err != nil || string(body) != want {
		t.Fatal("unexpected materialization", err)
	}
	private := filepath.Join(root, os.Getenv("TREE_PROOF_ORG"), "me", "notes", os.Getenv("TREE_PROOF_NOTE"))
	_, err = os.Stat(private)
	if mode == "node" && !os.IsNotExist(err) {
		t.Fatal("daemon exposed private note")
	}
	if mode == "person" && err != nil {
		t.Fatal("person missing private note", err)
	}
}

type peerTransport struct {
	peer   string
	status int
}

func (p *peerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("X-Overlay-Peer", p.peer)
	response, err := http.DefaultTransport.RoundTrip(r)
	if err == nil {
		p.status = response.StatusCode
	}
	return response, err
}
