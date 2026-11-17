package config

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/gentility-ai/gentility/connector/internal/state"
)

func TestConfigNeverFollowsRedirect(t *testing.T) {
	var calls atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer target.Close()
	for _, status := range []int{301, 302, 303, 307, 308} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Location", target.URL)
			w.WriteHeader(status)
		}))
		client := New(server.URL, state.Node{NodeCredential: "test-bearer"}, &state.Store{Dir: t.TempDir()}, nil)
		if _, err := client.Pull(context.Background()); err == nil {
			t.Errorf("accepted redirect %d", status)
		}
		server.Close()
	}
	if calls.Load() != 0 {
		t.Fatalf("redirect target received %d requests", calls.Load())
	}
}
