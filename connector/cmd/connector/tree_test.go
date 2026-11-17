package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/gentility-ai/gentility/connector/internal/state"
	"github.com/gentility-ai/gentility/connector/internal/tree"
)

func TestTreeLoginOrigin(t *testing.T) {
	store := &state.Store{Dir: t.TempDir()}
	if err := store.SaveAuth(state.Auth{BaseURL: "https://mcp.first.example", AccessToken: "fixture-secret"}); err != nil {
		t.Fatal(err)
	}
	login, err := treeLogin(store, tree.Discovery{Origin: "https://mcp.second.example"})
	if err == nil || login.AccessToken != "" || !strings.Contains(err.Error(), "connector login --url https://mcp.second.example") || strings.Contains(err.Error(), "fixture-secret") {
		t.Fatalf("origin refusal: %v", err)
	}
}
func TestTreeRunRefusesRoot(t *testing.T) {
	previous := geteuid
	geteuid = func() int { return 0 }
	defer func() { geteuid = previous }()
	var out bytes.Buffer
	if code := treeCommand([]string{"run"}, &out, &out); code != 2 || !strings.Contains(out.String(), "without sudo") {
		t.Fatalf("%d %s", code, out.String())
	}
}
func TestTreeExitCodes(t *testing.T) {
	for _, err := range []error{nil, &tree.HTTPError{Status: 401}, &tree.HTTPError{Status: 403}, &tree.Refusal{Message: "unsafe path"}} {
		if treeExitCode(err) != 0 {
			t.Fatal(err)
		}
	}
	if treeExitCode(errors.New("startup I/O failure")) != 1 {
		t.Fatal("startup failure must restart")
	}
}
