package traffic

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestReporterNeverFollowsRedirect(t *testing.T) {
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
		r := &Reporter{BaseURL: server.URL, Credential: "test-bearer"}
		_ = r.send(context.Background(), Batch{}, []byte("{}"))
		server.Close()
	}
	if calls.Load() != 0 {
		t.Fatalf("redirect target received %d requests", calls.Load())
	}
}
