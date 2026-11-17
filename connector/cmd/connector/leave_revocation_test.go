package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gentility-ai/gentility/connector/internal/state"
)

func TestLeaveConfirmsNodeRevocationBeforeStoppingOrDeleting(t *testing.T) {
	var out bytes.Buffer
	opts := leaveOptions(t, &out)
	plist := installed(t, opts)
	runner := &recorder{}
	called := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		if r.Method != http.MethodPost || r.URL.Path != "/overlay/self/revoke" || r.URL.RawQuery != "" {
			t.Errorf("unexpected revocation request")
		}
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if body["node_id"] != "node-1" || body["credential"] != "test-node-secret" {
			t.Error("wrong node confirmation")
		}
		if len(runner.calls) != 0 {
			t.Error("daemon stopped before revocation")
		}
		for _, path := range []string{plist, filepath.Join(opts.StateDir, "node.json")} {
			if _, err := os.Stat(path); err != nil {
				t.Error("local identity changed before revocation")
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"revoked"}`))
	}))
	defer server.Close()
	store := &state.Store{Dir: opts.StateDir}
	if err := store.SaveNode(state.Node{Origin: server.URL, NodeID: "node-1", NodeCredential: "test-node-secret"}); err != nil {
		t.Fatal(err)
	}
	if err := forget(context.Background(), opts, runner); err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("server never received revocation")
	}
	if store.HoldsMachine() {
		t.Fatal("identity survived confirmed leave")
	}
}

func TestLeaveRetainsEverythingWhenRevocationIsNotConfirmed(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusInternalServerError, http.StatusFound, http.StatusTemporaryRedirect} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var out bytes.Buffer
			opts := leaveOptions(t, &out)
			plist := installed(t, opts)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Location", "http://127.0.0.1:1/leak")
				w.WriteHeader(status)
			}))
			defer server.Close()
			store := &state.Store{Dir: opts.StateDir}
			if err := store.SaveNode(state.Node{Origin: server.URL, NodeID: "node-1", NodeCredential: "test-node-secret"}); err != nil {
				t.Fatal(err)
			}
			runner := &recorder{}
			err := forget(context.Background(), opts, runner)
			if err == nil {
				t.Fatal("unconfirmed leave succeeded")
			}
			if strings.Contains(err.Error(), "test-node-secret") {
				t.Fatal("credential leaked")
			}
			if len(runner.calls) != 0 {
				t.Fatal("daemon changed on failed revocation")
			}
			for _, path := range []string{plist, filepath.Join(opts.StateDir, "node.json"), filepath.Join(opts.StateDir, "enrollment.json"), filepath.Join(opts.StateDir, state.TSNetDir)} {
				if _, err := os.Stat(path); err != nil {
					t.Errorf("lost local retry state: %s", path)
				}
			}
		})
	}
}

func TestLeaveRetainsIdentityOnUnusableConfirmation(t *testing.T) {
	for _, body := range []string{"", "not JSON", `{}`, `{"status":"pending"}`} {
		t.Run(body, func(t *testing.T) {
			var out bytes.Buffer
			opts := leaveOptions(t, &out)
			plist := installed(t, opts)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(body))
			}))
			defer server.Close()
			store := &state.Store{Dir: opts.StateDir}
			if err := store.SaveNode(state.Node{Origin: server.URL, NodeID: "node-1", NodeCredential: "test-node-secret"}); err != nil {
				t.Fatal(err)
			}
			runner := &recorder{}
			if err := forget(context.Background(), opts, runner); err == nil {
				t.Fatal("unconfirmed leave succeeded")
			}
			if len(runner.calls) != 0 || !store.HoldsMachine() {
				t.Fatal("unconfirmed leave changed local identity or daemon")
			}
			if _, err := os.Stat(plist); err != nil {
				t.Fatal(err)
			}
		})
	}
}
