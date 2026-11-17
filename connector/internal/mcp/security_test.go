package mcp

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/gentility-ai/gentility/connector/internal/version"
)

func TestTerminalErrorsRefuseControls(t *testing.T) {
	poison := "before\x1b]52;c;clipboard\x07\r\n\t\x7f\u0085\u009bafter"
	for _, err := range []error{
		&HTTPError{Status: poison, Body: poison}, &HTTPError{Status: poison},
		&RPCError{Message: poison}, &ToolError{Tool: poison, Message: poison},
		&ToolRefusedError{Tool: poison, Value: poison},
		&ToolRefusedError{Tool: poison, Value: map[string]any{"code": poison, "message": poison}},
	} {
		for _, r := range err.Error() {
			if r < 32 || r >= 127 && r <= 159 {
				t.Errorf("%T printed U+%04X", err, r)
				break
			}
		}
	}
}

func TestHTTPRequiresLiteralLoopback(t *testing.T) {
	original := version.Connector
	t.Cleanup(func() { version.Connector = original })
	version.Connector = "release"
	for _, host := range []string{"mcp.localhost", "anything.localhost", "127.0.0.2", "localhost.example"} {
		if _, err := NormalizeBaseURL("http://" + host); err == nil {
			t.Errorf("accepted %s", host)
		}
	}
	for _, host := range []string{"localhost", "127.0.0.1", "[::1]"} {
		if _, err := NormalizeBaseURL("http://" + host); err != nil {
			t.Errorf("refused %s: %v", host, err)
		}
	}
}

func TestWorktreeOriginsRequireExplicitDev(t *testing.T) {
	original := version.Connector
	t.Cleanup(func() { version.Connector = original })
	for _, stamp := range []string{"dev", "unreleased", "0.2.0"} {
		version.Connector = stamp
		for _, host := range []string{"mcp.localhost", "worktree.localhost"} {
			_, err := NormalizeBaseURL("http://" + host)
			if (err == nil) != (stamp == "dev") {
				t.Errorf("%s under %s: %v", host, stamp, err)
			}
		}
	}
}

func TestMCPNeverFollowsRedirect(t *testing.T) {
	var calls atomic.Int32
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(200) }))
	defer destination.Close()
	for _, status := range []int{301, 302, 303, 307, 308} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Location", destination.URL)
			w.WriteHeader(status)
		}))
		_, err := New(server.URL, "test-bearer").Initialize(context.Background())
		server.Close()
		var httpErr *HTTPError
		if !errors.As(err, &httpErr) || httpErr.StatusCode != status {
			t.Errorf("redirect %d: got %v", status, err)
		}
	}
	if calls.Load() != 0 {
		t.Fatalf("redirect target received %d requests", calls.Load())
	}
}
