package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gentility-ai/gentility/connector/internal/state"
	"github.com/gentility-ai/gentility/connector/internal/supervise"
)

func TestUninstallRevocationOutcome(t *testing.T) {
	for _, outcome := range []string{"success", "refused", "unreachable", "unreadable"} {
		t.Run(outcome, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if outcome == "refused" {
					w.WriteHeader(500)
				} else {
					w.WriteHeader(200)
				}
			}))
			defer srv.Close()
			if outcome == "unreachable" {
				srv.Close()
			}
			dir := t.TempDir()
			st := &state.Store{Dir: dir}
			if err := st.SaveAuth(state.Auth{BaseURL: srv.URL, AccessToken: "private-test-token"}); err != nil {
				t.Fatal(err)
			}
			if outcome == "unreadable" {
				if err := os.Remove(filepath.Join(dir, "auth.json")); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(filepath.Join(dir, "auth.json"), 0700); err != nil {
					t.Fatal(err)
				}
			}
			var out, warnings bytes.Buffer
			runner := &recorder{}
			code := uninstallWithRunner(context.Background(), supervise.Options{StateDir: dir, Root: t.TempDir(), GOOS: "linux", Label: "test", Out: &out}, runner, &warnings)
			if code != 0 {
				t.Fatalf("uninstall = %d: %s", code, warnings.String())
			}
			if len(runner.calls) == 0 {
				t.Fatal("uninstall did not reach supervisor")
			}
			if outcome == "success" {
				if _, err := st.LoadAuth(); !state.IsNotLoggedIn(err) {
					t.Fatal("dead auth retained", err)
				}
				if warnings.Len() != 0 {
					t.Fatal(warnings.String())
				}
			} else {
				if _, err := os.Stat(filepath.Join(dir, "auth.json")); err != nil {
					t.Fatal("auth removed", err)
				}
				if strings.Count(warnings.String(), "Warning:") != 1 {
					t.Fatal(warnings.String())
				}
				if outcome == "unreadable" && (!strings.Contains(warnings.String(), "could not read") || strings.Contains(warnings.String(), "still live")) {
					t.Fatal(warnings.String())
				}
			}
			if strings.Contains(warnings.String(), "private-test-token") {
				t.Fatal("token leaked")
			}
		})
	}
}
