package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gentility-ai/gentility/connector/internal/enroll"
	"github.com/gentility-ai/gentility/connector/internal/exitcodes"
	"github.com/gentility-ai/gentility/connector/internal/mcp"
	"github.com/gentility-ai/gentility/connector/internal/state"
	"github.com/gentility-ai/gentility/connector/internal/tsnode"
	"github.com/gentility-ai/gentility/connector/internal/version"
)

func TestUsageListsTheDayOneCommands(t *testing.T) {
	var out bytes.Buffer
	usage(&out)

	for _, want := range []string{"install", "join", "login", "logout", "net", "serve", "status", "task", "uninstall", "version", "net join", "net serve ssh"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("usage does not mention %q:\n%s", want, out.String())
		}
	}
}

// One binary, two names: `connector` is what a server installs and
// `gentility` is what a person types, and the help has to read as whichever
// one is in front of the reader.
func TestUsageUsesTheInvokedName(t *testing.T) {
	var out bytes.Buffer
	usageAs("gentility", &out)

	if strings.Contains(out.String(), "connector task") {
		t.Fatalf("usage invoked as gentility should not say 'connector':\n%s", out.String())
	}
	if !strings.Contains(out.String(), "gentility task") {
		t.Fatalf("usage should use the invoked name:\n%s", out.String())
	}
}

func TestTaskUsageUsesTheInvokedName(t *testing.T) {
	var out bytes.Buffer
	taskUsageAs("gentility", &out)

	if strings.Contains(out.String(), "connector task") {
		t.Fatalf("task usage invoked as gentility should not say 'connector':\n%s", out.String())
	}
	for _, want := range []string{"gentility task add", "gentility task list"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("task usage does not show %q:\n%s", want, out.String())
		}
	}
}

// The name comes from argv[0], which is how a symlink renames a command
// without a second build.
func TestCommandNameReadsTheNameItWasInvokedBy(t *testing.T) {
	for _, c := range []struct {
		argv []string
		want string
	}{
		{[]string{"/usr/local/bin/connector"}, "connector"},
		{[]string{"/usr/local/bin/gentility"}, "gentility"},
		{[]string{"./connector"}, "connector"},
		{[]string{"gentility.exe"}, "gentility"},
		{[]string{"gentility.EXE"}, "gentility"},
		{[]string{""}, DefaultName},
		{nil, DefaultName},
	} {
		if got := commandName(c.argv); got != c.want {
			t.Errorf("commandName(%q) = %q, want %q", c.argv, got, c.want)
		}
	}
}

func TestRunWithNoArgumentsIsAUsageError(t *testing.T) {
	var out bytes.Buffer
	if code := run([]string{}, &out, &out); code != 2 {
		t.Fatalf("run() = %d, want 2", code)
	}
}

func TestRunVersionPrintsAVersion(t *testing.T) {
	var out bytes.Buffer
	if code := run([]string{"version"}, &out, &out); code != 0 {
		t.Fatalf("run(version) = %d, want 0", code)
	}
	if !strings.Contains(out.String(), version.String()) {
		t.Fatalf("version output %q does not contain %q", out.String(), version.String())
	}
}

func TestRunAnUnknownSubcommandIsAUsageError(t *testing.T) {
	var out bytes.Buffer
	if code := run([]string{"frobnicate"}, &out, &out); code != 2 {
		t.Fatalf("run(frobnicate) = %d, want 2", code)
	}
	if !strings.Contains(out.String(), "frobnicate") {
		t.Fatalf("output %q does not name the unknown subcommand", out.String())
	}
}

func TestRunLogoutForgetsTheBearerAndSaysSo(t *testing.T) {
	t.Setenv("GENTILITY_STATE_DIR", t.TempDir())

	var out bytes.Buffer
	if code := run([]string{"logout"}, &out, &out); code != 0 {
		t.Fatalf("run(logout) = %d, want 0", code)
	}
	if !strings.Contains(out.String(), "Signed out.") {
		t.Fatalf("output %q", out.String())
	}

	// Logging out twice is not an error a person should have to think about.
	out.Reset()
	if code := run([]string{"logout"}, &out, &out); code != 0 {
		t.Fatalf("a second run(logout) = %d, want 0", code)
	}
}

func TestRunLoginRejectsAnUnusableURLWithoutTouchingTheNetwork(t *testing.T) {
	t.Setenv("GENTILITY_STATE_DIR", t.TempDir())

	var out bytes.Buffer
	if code := run([]string{"login", "--url", "mcp.example.com"}, &out, &out); code != 1 {
		t.Fatalf("run(login --url mcp.example.com) = %d, want 1", code)
	}
	if !strings.Contains(out.String(), "mcp.example.com") {
		t.Fatalf("output %q does not name the URL it refused", out.String())
	}
}

func TestRunLoginRejectsAnUnknownFlag(t *testing.T) {
	t.Setenv("GENTILITY_STATE_DIR", t.TempDir())

	var out bytes.Buffer
	if code := run([]string{"login", "--frobnicate"}, &out, &out); code != 2 {
		t.Fatalf("run(login --frobnicate) = %d, want 2", code)
	}
}

func TestRunLogoutTakesNoArguments(t *testing.T) {
	t.Setenv("GENTILITY_STATE_DIR", t.TempDir())

	var out bytes.Buffer
	if code := run([]string{"logout", "everywhere"}, &out, &out); code != 2 {
		t.Fatalf("run(logout everywhere) = %d, want 2", code)
	}
	if !strings.Contains(out.String(), "everywhere") {
		t.Fatalf("output %q does not name the unexpected argument", out.String())
	}
}

func TestRunTaskWithNoVerbIsAUsageError(t *testing.T) {
	var out bytes.Buffer
	if code := run([]string{"task"}, &out, &out); code != 2 {
		t.Fatalf("run(task) = %d, want 2", code)
	}
	if !strings.Contains(out.String(), "add") {
		t.Fatalf("task usage does not list its commands:\n%s", out.String())
	}
}

func TestRunTaskAnUnknownVerbIsAUsageError(t *testing.T) {
	var out bytes.Buffer
	if code := run([]string{"task", "frobnicate"}, &out, &out); code != 2 {
		t.Fatalf("run(task frobnicate) = %d, want 2", code)
	}
	if !strings.Contains(out.String(), "frobnicate") {
		t.Fatalf("output %q does not name the unknown verb", out.String())
	}
}

// toolCall is one tools/call the fake MCP host saw.
type toolCall struct {
	name string
	args map[string]any
}

// mcpServer enforces the wire contract: the bearer, protocol version, and two
// headers that must repeat the body. It answers with the MCP tool result
// envelope: the payload JSON-encoded inside one text content block.
//
// reply is given the tool name and its arguments and returns that inner JSON.
func mcpServer(t *testing.T, bearer string, reply func(*testing.T, toolCall) string) (*httptest.Server, *[]toolCall) {
	t.Helper()

	var mu sync.Mutex
	seen := []toolCall{}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer "+bearer {
			t.Errorf("Authorization = %q, want %q", got, "Bearer "+bearer)
		}
		if got := r.Header.Get("MCP-Protocol-Version"); got != mcp.ProtocolVersion {
			t.Errorf("MCP-Protocol-Version = %q, want %q", got, mcp.ProtocolVersion)
		}

		var body struct {
			ID     int64  `json:"id"`
			Method string `json:"method"`
			Params struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			} `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decoding the request: %v", err)
		}
		if got := r.Header.Get("Mcp-Method"); got != body.Method {
			t.Errorf("Mcp-Method = %q, body method = %q", got, body.Method)
		}
		if got := r.Header.Get("Mcp-Name"); got != body.Params.Name {
			t.Errorf("Mcp-Name = %q, params.name = %q", got, body.Params.Name)
		}

		observed := toolCall{name: body.Params.Name, args: body.Params.Arguments}
		mu.Lock()
		seen = append(seen, observed)
		mu.Unlock()

		// The server refuses a call that does not name its organization
		// (the server's organization resolution), so the fake does too — a CLI
		// that stopped sending it must fail here, not pass quietly.
		if org, _ := observed.args["organization"].(string); org == "" {
			t.Errorf("%s was called without an organization: %#v", observed.name, observed.args)
		}

		text, err := json.Marshal(reply(t, observed))
		if err != nil {
			t.Fatalf("encoding the reply: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":{"content":[{"type":"text","text":%s}]}}`,
			body.ID, text)
	}))
	t.Cleanup(srv.Close)

	return srv, &seen
}

// signIn points the connector at one host and stores a bearer minted by it.
func signIn(t *testing.T, origin, bearer string) *state.Store {
	t.Helper()

	t.Setenv("GENTILITY_STATE_DIR", t.TempDir())
	t.Setenv("GENTILITY_MCP_URL", origin)

	store := state.New()
	if err := store.SaveAuth(state.Auth{BaseURL: origin, AccessToken: bearer}); err != nil {
		t.Fatalf("SaveAuth: %v", err)
	}
	return store
}

func TestRunTaskAddCreatesATaskAndRemembersTheOrganization(t *testing.T) {
	srv, seen := mcpServer(t, "tok", func(t *testing.T, c toolCall) string {
		if c.name != "task_create" {
			t.Errorf("tool = %q, want task_create", c.name)
		}
		return `{"success":true,"deduplicated":false,"task":{"id":"11111111-1111-1111-1111-111111111111","title":"Rotate the WorkOS key"}}`
	})
	store := signIn(t, srv.URL, "tok")

	var out bytes.Buffer
	code := run([]string{"task", "add", "Rotate the WorkOS key", "--kind", "ops", "--org", "acme"}, &out, &out)
	if code != 0 {
		t.Fatalf("run(task add) = %d, want 0:\n%s", code, out.String())
	}
	if !strings.Contains(out.String(), "created 11111111-1111-1111-1111-111111111111  Rotate the WorkOS key") {
		t.Fatalf("output %q", out.String())
	}

	if len(*seen) != 1 {
		t.Fatalf("the host saw %d calls, want 1", len(*seen))
	}
	if org := (*seen)[0].args["organization"]; org != "acme" {
		t.Fatalf("organization = %v, want acme", org)
	}
	if got := store.RecentOrg(); got != "acme" {
		t.Fatalf("RecentOrg = %q, want acme — a successful call updates the default", got)
	}
}

func TestRunTaskAddDefaultsToTheRememberedOrganization(t *testing.T) {
	srv, seen := mcpServer(t, "tok", func(*testing.T, toolCall) string {
		return `{"success":true,"task":{"id":"t1","title":"Thing"}}`
	})
	store := signIn(t, srv.URL, "tok")
	if err := store.SetRecentOrg("acme"); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if code := run([]string{"task", "add", "Thing", "--kind", "ops"}, &out, &out); code != 0 {
		t.Fatalf("run(task add) = %d, want 0:\n%s", code, out.String())
	}
	if org := (*seen)[0].args["organization"]; org != "acme" {
		t.Fatalf("organization = %v, want the remembered acme", org)
	}
}

func TestRunTaskAddSaysAlreadyThereWhenTheTaskWasDeduplicated(t *testing.T) {
	srv, _ := mcpServer(t, "tok", func(*testing.T, toolCall) string {
		return `{"success":true,"deduplicated":true,"task":{"id":"t1","title":"Thing"}}`
	})
	signIn(t, srv.URL, "tok")

	var out bytes.Buffer
	if code := run([]string{"task", "add", "Thing", "--kind", "ops", "--org", "acme", "--dedup-key", "k"}, &out, &out); code != 0 {
		t.Fatalf("run(task add) = %d, want 0:\n%s", code, out.String())
	}
	if !strings.Contains(out.String(), "already there t1") {
		t.Fatalf("output %q does not say the task already existed", out.String())
	}
}

func TestRunTaskAddWithNoOrganizationRefusesBeforeTheNetwork(t *testing.T) {
	srv, seen := mcpServer(t, "tok", func(t *testing.T, _ toolCall) string {
		t.Error("the host was called; the organization must be resolved locally first")
		return `{"success":true}`
	})
	signIn(t, srv.URL, "tok")

	var out bytes.Buffer
	if code := run([]string{"task", "add", "Thing", "--kind", "ops"}, &out, &out); code != 2 {
		t.Fatalf("run(task add) = %d, want 2:\n%s", code, out.String())
	}
	if !strings.Contains(out.String(), "--org") {
		t.Fatalf("output %q does not name the flag that fixes it", out.String())
	}
	if len(*seen) != 0 {
		t.Fatalf("the host saw %d calls, want none", len(*seen))
	}
}

func TestRunTaskAddWhenNotSignedInSaysToLogIn(t *testing.T) {
	t.Setenv("GENTILITY_STATE_DIR", t.TempDir())
	t.Setenv("GENTILITY_MCP_URL", "https://mcp.example")

	var out bytes.Buffer
	if code := run([]string{"task", "add", "Thing", "--kind", "ops", "--org", "acme"}, &out, &out); code != 1 {
		t.Fatalf("run(task add) = %d, want 1:\n%s", code, out.String())
	}
	if !strings.Contains(out.String(), invoked+" login") {
		t.Fatalf("output %q does not say how to sign in", out.String())
	}
}

func TestRunTaskAddRefusesABearerFromAnotherHost(t *testing.T) {
	t.Setenv("GENTILITY_STATE_DIR", t.TempDir())
	t.Setenv("GENTILITY_MCP_URL", "https://mcp.example")

	store := state.New()
	if err := store.SaveAuth(state.Auth{BaseURL: "https://mcp.elsewhere", AccessToken: "tok"}); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if code := run([]string{"task", "add", "Thing", "--kind", "ops", "--org", "acme"}, &out, &out); code != 1 {
		t.Fatalf("run(task add) = %d, want 1:\n%s", code, out.String())
	}
	for _, want := range []string{"mcp.elsewhere", "mcp.example", "connector login --url"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("output %q does not name %q", out.String(), want)
		}
	}
}

func TestRunTaskAddSaysTheSessionExpiredOnA401(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="mcp"`)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(srv.Close)
	signIn(t, srv.URL, "stale")

	var out bytes.Buffer
	if code := run([]string{"task", "add", "Thing", "--kind", "ops", "--org", "acme"}, &out, &out); code != 1 {
		t.Fatalf("run(task add) = %d, want 1:\n%s", code, out.String())
	}
	if !strings.Contains(out.String(), "expired") || !strings.Contains(out.String(), invoked+" login") {
		t.Fatalf("output %q", out.String())
	}
}

func TestRunTaskAddPrintsAToolRefusalAndFails(t *testing.T) {
	srv, _ := mcpServer(t, "tok", func(*testing.T, toolCall) string {
		return `{"success":false,"error":{"code":"unknown_kind","message":"No kind named ops"}}`
	})
	signIn(t, srv.URL, "tok")

	var out bytes.Buffer
	if code := run([]string{"task", "add", "Thing", "--kind", "ops", "--org", "acme"}, &out, &out); code != 1 {
		t.Fatalf("run(task add) = %d, want 1:\n%s", code, out.String())
	}
	if !strings.Contains(out.String(), "No kind named ops") {
		t.Fatalf("output %q does not carry the tool's reason", out.String())
	}
}

func TestRunTaskListRendersTheTasks(t *testing.T) {
	srv, seen := mcpServer(t, "tok", func(t *testing.T, c toolCall) string {
		if c.name != "task_list" {
			t.Errorf("tool = %q, want task_list", c.name)
		}
		return `{"success":true,"tasks":[
			{"id":"11111111-1111-1111-1111-111111111111","title":"Rotate the WorkOS key","status":"open","kinds":["ops"],"unchecked_count":2,"actionable":true},
			{"id":"22222222-2222-2222-2222-222222222222","title":"Ship the overlay","status":"open","kinds":["eng"],"unchecked_count":0,"actionable":true}
		]}`
	})
	signIn(t, srv.URL, "tok")

	var out bytes.Buffer
	if code := run([]string{"task", "list", "--mine", "--status", "open", "--org", "acme"}, &out, &out); code != 0 {
		t.Fatalf("run(task list) = %d, want 0:\n%s", code, out.String())
	}
	for _, want := range []string{"11111111", "Rotate the WorkOS key", "2 left", "Ship the overlay"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("output does not contain %q:\n%s", want, out.String())
		}
	}

	args := (*seen)[0].args
	if args["for"] != "me" || args["status"] != "open" || args["organization"] != "acme" {
		t.Fatalf("arguments = %#v", args)
	}
}

func TestRunTaskListSaysSoWhenThereAreNone(t *testing.T) {
	srv, _ := mcpServer(t, "tok", func(*testing.T, toolCall) string {
		return `{"success":true,"tasks":[]}`
	})
	signIn(t, srv.URL, "tok")

	var out bytes.Buffer
	if code := run([]string{"task", "list", "--org", "acme"}, &out, &out); code != 0 {
		t.Fatalf("run(task list) = %d, want 0:\n%s", code, out.String())
	}
	if !strings.Contains(out.String(), "No tasks.") {
		t.Fatalf("output %q", out.String())
	}
}

func TestRunTaskListJSONPrintsThePayload(t *testing.T) {
	srv, _ := mcpServer(t, "tok", func(*testing.T, toolCall) string {
		return `{"success":true,"tasks":[{"id":"t1","title":"Thing","status":"open"}]}`
	})
	signIn(t, srv.URL, "tok")

	var out bytes.Buffer
	if code := run([]string{"task", "list", "--org", "acme", "--json"}, &out, &out); code != 0 {
		t.Fatalf("run(task list --json) = %d, want 0:\n%s", code, out.String())
	}

	var payload map[string]any
	if err := json.Unmarshal(out.Bytes(), &payload); err != nil {
		t.Fatalf("--json did not print JSON (%v):\n%s", err, out.String())
	}
	if payload["success"] != true {
		t.Fatalf("payload = %#v", payload)
	}
}

func TestRunTaskListRefusesAStatusTheToolDoesNotHave(t *testing.T) {
	srv, seen := mcpServer(t, "tok", func(t *testing.T, _ toolCall) string {
		t.Error("the host was called; an unknown status is refused locally")
		return `{"success":true}`
	})
	signIn(t, srv.URL, "tok")

	var out bytes.Buffer
	if code := run([]string{"task", "list", "--org", "acme", "--status", "weird"}, &out, &out); code != 2 {
		t.Fatalf("run(task list --status weird) = %d, want 2:\n%s", code, out.String())
	}
	if len(*seen) != 0 {
		t.Fatalf("the host saw %d calls, want none", len(*seen))
	}
}

func TestRunTaskCheckChecksAnItemByPosition(t *testing.T) {
	srv, seen := mcpServer(t, "tok", func(t *testing.T, c toolCall) string {
		if c.name != "task_check" {
			t.Errorf("tool = %q, want task_check", c.name)
		}
		return `{"success":true,"task":{"id":"11111111-1111-1111-1111-111111111111","title":"Rotate the WorkOS key","status":"open","kinds":["ops"],"unchecked_count":1,"actionable":true}}`
	})
	signIn(t, srv.URL, "tok")

	var out bytes.Buffer
	code := run([]string{"task", "check", "11111111-1111-1111-1111-111111111111", "2", "--cause", "ran it", "--org", "acme"}, &out, &out)
	if code != 0 {
		t.Fatalf("run(task check) = %d, want 0:\n%s", code, out.String())
	}
	if !strings.Contains(out.String(), "checked") || !strings.Contains(out.String(), "1 left") {
		t.Fatalf("output %q does not say where that leaves the task", out.String())
	}

	args := (*seen)[0].args
	if args["position"] != float64(2) || args["cause"] != "ran it" {
		t.Fatalf("arguments = %#v", args)
	}
	if _, sent := args["item_id"]; sent {
		t.Fatalf("a position was given but an item_id was sent too: %#v", args)
	}
}

func TestRunTaskCheckChecksAnItemByID(t *testing.T) {
	srv, seen := mcpServer(t, "tok", func(*testing.T, toolCall) string {
		return `{"success":true,"task":{"id":"t1","title":"Thing","status":"open"}}`
	})
	signIn(t, srv.URL, "tok")

	var out bytes.Buffer
	code := run([]string{"task", "check",
		"11111111-1111-1111-1111-111111111111", "22222222-2222-2222-2222-222222222222", "--org", "acme"}, &out, &out)
	if code != 0 {
		t.Fatalf("run(task check) = %d, want 0:\n%s", code, out.String())
	}
	if (*seen)[0].args["item_id"] != "22222222-2222-2222-2222-222222222222" {
		t.Fatalf("arguments = %#v", (*seen)[0].args)
	}
}

func TestRunTaskCheckWithNoItemRefusesLocally(t *testing.T) {
	srv, seen := mcpServer(t, "tok", func(t *testing.T, _ toolCall) string {
		t.Error("the host was called; the schema's oneOf is enforced locally")
		return `{"success":true}`
	})
	signIn(t, srv.URL, "tok")

	var out bytes.Buffer
	if code := run([]string{"task", "check", "11111111-1111-1111-1111-111111111111", "--org", "acme"}, &out, &out); code != 2 {
		t.Fatalf("run(task check) = %d, want 2:\n%s", code, out.String())
	}
	if len(*seen) != 0 {
		t.Fatalf("the host saw %d calls, want none", len(*seen))
	}
}

func TestRunTaskCloseClosesATask(t *testing.T) {
	srv, seen := mcpServer(t, "tok", func(t *testing.T, c toolCall) string {
		if c.name != "task_close" {
			t.Errorf("tool = %q, want task_close", c.name)
		}
		return `{"success":true,"task":{"id":"11111111-1111-1111-1111-111111111111","title":"Rotate the WorkOS key","status":"done","kinds":["ops"]}}`
	})
	signIn(t, srv.URL, "tok")

	var out bytes.Buffer
	code := run([]string{"task", "close", "11111111-1111-1111-1111-111111111111", "--cause", "shipped", "--org", "acme"}, &out, &out)
	if code != 0 {
		t.Fatalf("run(task close) = %d, want 0:\n%s", code, out.String())
	}
	if !strings.Contains(out.String(), "closed\n11111111-1111-1111-1111-111111111111") || !strings.Contains(out.String(), "done") {
		t.Fatalf("output %q", out.String())
	}

	args := (*seen)[0].args
	if args["disposition"] != "done" || args["cause"] != "shipped" {
		t.Fatalf("arguments = %#v", args)
	}
}

func TestRunTaskCloseWithoutACauseRefusesLocally(t *testing.T) {
	srv, seen := mcpServer(t, "tok", func(t *testing.T, _ toolCall) string {
		t.Error("the host was called; the schema requires a cause and so does the CLI")
		return `{"success":true}`
	})
	signIn(t, srv.URL, "tok")

	var out bytes.Buffer
	if code := run([]string{"task", "close", "11111111-1111-1111-1111-111111111111", "--org", "acme"}, &out, &out); code != 2 {
		t.Fatalf("run(task close) = %d, want 2:\n%s", code, out.String())
	}
	if !strings.Contains(out.String(), "--cause") {
		t.Fatalf("output %q does not name the flag that fixes it", out.String())
	}
	if len(*seen) != 0 {
		t.Fatalf("the host saw %d calls, want none", len(*seen))
	}
}

func TestRunTaskCloseSurfacesABoundaryRefusal(t *testing.T) {
	// isError:true is how the MCP tool boundary refuses a call around
	// the tool — a bad argument, an org the caller is not in.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{"isError":true,"content":[{"type":"text","text":"{\"errors\":[{\"message\":\"You are not a member of acme\"}]}"}]}}`)
	}))
	t.Cleanup(srv.Close)
	signIn(t, srv.URL, "tok")

	var out bytes.Buffer
	code := run([]string{"task", "close", "11111111-1111-1111-1111-111111111111", "--cause", "x", "--org", "acme"}, &out, &out)
	if code != 1 {
		t.Fatalf("run(task close) = %d, want 1:\n%s", code, out.String())
	}
	if !strings.Contains(out.String(), "You are not a member of acme") {
		t.Fatalf("output %q does not carry the boundary's reason", out.String())
	}
}

func TestRunTaskAddDoesNotRememberTheOrganizationOfAFailedCall(t *testing.T) {
	srv, _ := mcpServer(t, "tok", func(*testing.T, toolCall) string {
		return `{"success":false,"error":"nope"}`
	})
	store := signIn(t, srv.URL, "tok")

	var out bytes.Buffer
	if code := run([]string{"task", "add", "Thing", "--kind", "ops", "--org", "acme"}, &out, &out); code != 1 {
		t.Fatalf("run(task add) = %d, want 1", code)
	}
	if got := store.RecentOrg(); got != "" {
		t.Fatalf("RecentOrg = %q, want empty — a refused call is not a default worth keeping", got)
	}
}

// --help is a question, and the answer is the command's own shape: what it
// takes on one line, then what each flag means. Every verb answers it, on
// stdout, with exit 0 — a person asking what a command does has not failed.
func TestRunHelpAnswersEveryCommand(t *testing.T) {
	cases := []struct {
		argv  []string
		flags []string
	}{
		{[]string{"task", "add"}, []string{"-kind", "-body", "-for", "-dedup-key", "-org"}},
		{[]string{"task", "list"}, []string{"-mine", "-status", "-limit", "-json", "-org"}},
		{[]string{"task", "check"}, []string{"-cause", "-org"}},
		{[]string{"task", "close"}, []string{"-cause", "-dismiss", "-superseded-by", "-org"}},
		{[]string{"join"}, []string{"-verbose"}},
		{[]string{"serve"}, []string{"-state-dir", "-hostname", "-egress-port", "-interface"}},
		{[]string{"status"}, []string{"-state-dir", "-json"}},
		{[]string{"login"}, []string{"-url", "-server"}},
		{[]string{"logout"}, nil},
		{[]string{"install"}, []string{"-state-dir", "-log-dir", "-binary", "-label", "-source-state-dir", "-interface"}},
		{[]string{"uninstall"}, []string{"-state-dir", "-label"}},
		{[]string{"leave"}, nil},
		{[]string{"net", "join"}, []string{"-label", "-url"}},
		{[]string{"net", "leave"}, nil},
		{[]string{"net", "status"}, []string{"-state-dir", "-json"}},
		{[]string{"net", "serve"}, []string{"-port"}},
		{[]string{"net", "unserve"}, []string{"-port"}},
		{[]string{"cleanup"}, []string{"-interface"}},
	}

	for _, c := range cases {
		for _, ask := range []string{"--help", "-h"} {
			name := strings.Join(c.argv, " ") + " " + ask
			t.Run(name, func(t *testing.T) {
				t.Setenv("GENTILITY_STATE_DIR", t.TempDir())

				var stdout, stderr bytes.Buffer
				if code := run(append(c.argv, ask), &stdout, &stderr); code != 0 {
					t.Fatalf("run(%s) = %d, want 0:\n%s%s", name, code, stdout.String(), stderr.String())
				}
				if stderr.Len() != 0 {
					t.Fatalf("help went to stderr: %s", stderr.String())
				}
				synopsis := "usage: " + invoked + " " + strings.Join(c.argv, " ")
				if !strings.HasPrefix(stdout.String(), synopsis) {
					t.Fatalf("output does not open with %q:\n%s", synopsis, stdout.String())
				}
				for _, flag := range c.flags {
					if !strings.Contains(stdout.String(), flag) {
						t.Fatalf("help does not document %s:\n%s", flag, stdout.String())
					}
				}
			})
		}
	}
}

// "not enrolled" is the first step, not a failure: a person who typed `status`
// to find out where this machine stands has done nothing wrong, so the command
// answers with what to type and exits 0. A non-zero here would make the obvious
// health check — `connector status || alert` — fire on every fresh machine.
func TestRunStatusOnAnUnenrolledMachineExitsZeroAndSaysWhatToType(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"status", "--state-dir", t.TempDir()}, &stdout, &stderr)

	if code != 0 {
		t.Fatalf("run(status) = %d, want 0:\n%s%s", code, stdout.String(), stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("status wrote to stderr: %s", stderr.String())
	}
	if !strings.Contains(stdout.String(), "connector login --server") {
		t.Fatalf("status does not say what to type:\n%s", stdout.String())
	}
}

// --json is the same snapshot, for the person piping it into jq rather than
// reading it.
func TestRunStatusJSONPrintsTheSnapshot(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"status", "--state-dir", t.TempDir(), "--json"}, &stdout, &stderr)

	if code != 0 {
		t.Fatalf("run(status --json) = %d, want 0:\n%s%s", code, stdout.String(), stderr.String())
	}

	var snapshot map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &snapshot); err != nil {
		t.Fatalf("status --json is not JSON: %v\n%s", err, stdout.String())
	}
	if snapshot["state"] != "not_enrolled" {
		t.Fatalf("state = %v, want not_enrolled:\n%s", snapshot["state"], stdout.String())
	}
}

// A flag with nothing after it is flag's "flag needs an argument: -org",
// which spells the flag the way nobody typed it. The connector says it in the
// words it uses everywhere else.
func TestRunAFlagWithNoValueIsRefusedInTheConnectorsOwnWords(t *testing.T) {
	t.Setenv("GENTILITY_STATE_DIR", t.TempDir())

	var out bytes.Buffer
	if code := run([]string{"task", "list", "--org"}, &out, &out); code != 2 {
		t.Fatalf("run(task list --org) = %d, want 2:\n%s", code, out.String())
	}
	if !strings.Contains(out.String(), "--org needs a value") {
		t.Fatalf("output %q", out.String())
	}
}

func TestRunTaskCheckNamesTheItemItChecked(t *testing.T) {
	srv, _ := mcpServer(t, "tok", func(*testing.T, toolCall) string {
		return `{"success":true,"task":{"id":"11111111-1111-1111-1111-111111111111","title":"Rotate the WorkOS key","status":"open","kinds":["ops"],"unchecked_count":1,"actionable":true,
			"items":[
				{"id":"aaaaaaaa-1111-1111-1111-111111111111","text":"Mint the new key","position":1,"checked_at":"2026-08-28T00:00:00Z"},
				{"id":"bbbbbbbb-2222-2222-2222-222222222222","text":"Rotate the deploy secret","position":2,"checked_at":"2026-08-28T01:00:00Z"},
				{"id":"cccccccc-3333-3333-3333-333333333333","text":"Tell the team","position":3,"checked_at":null}
			]}}`
	})
	signIn(t, srv.URL, "tok")

	var out bytes.Buffer
	code := run([]string{"task", "check", "11111111-1111-1111-1111-111111111111", "2", "--cause", "ran it", "--org", "acme"}, &out, &out)
	if code != 0 {
		t.Fatalf("run(task check) = %d, want 0:\n%s", code, out.String())
	}

	want := "checked 2/3  Rotate the deploy secret\n" +
		"11111111-1111-1111-1111-111111111111  open  ops  1 left  Rotate the WorkOS key\n"
	if out.String() != want {
		t.Fatalf("output =\n%q\nwant\n%q", out.String(), want)
	}
}

// The row `task check` and `task close` print is the row `task list` prints:
// same columns, id at column 0, nothing prefixed onto it. What happened is
// its own line above.
func TestRunTaskCheckAndCloseRowsMatchTaskList(t *testing.T) {
	const task = `{"id":"11111111-1111-1111-1111-111111111111","title":"Rotate the WorkOS key","status":"done","kinds":["ops"],"unchecked_count":0,"actionable":false}`

	srv, _ := mcpServer(t, "tok", func(_ *testing.T, c toolCall) string {
		if c.name == "task_list" {
			return `{"success":true,"tasks":[` + task + `]}`
		}
		return `{"success":true,"task":` + task + `}`
	})
	signIn(t, srv.URL, "tok")

	var listed bytes.Buffer
	if code := run([]string{"task", "list", "--org", "acme"}, &listed, &listed); code != 0 {
		t.Fatalf("run(task list) = %d:\n%s", code, listed.String())
	}

	var closed bytes.Buffer
	code := run([]string{"task", "close", "11111111-1111-1111-1111-111111111111", "--cause", "shipped", "--org", "acme"}, &closed, &closed)
	if code != 0 {
		t.Fatalf("run(task close) = %d:\n%s", code, closed.String())
	}

	want := "closed\n" + listed.String()
	if closed.String() != want {
		t.Fatalf("close printed\n%q\nwant\n%q", closed.String(), want)
	}
}

// The organization is remembered as a convenience. Losing it is worth a line,
// but it is not what the command was for, and prefixing it like a failure
// makes a successful close read as a failed one.
func TestRunTaskAddNotesButDoesNotFailWhenItCannotRememberTheOrganization(t *testing.T) {
	srv, _ := mcpServer(t, "tok", func(*testing.T, toolCall) string {
		return `{"success":true,"task":{"id":"t1","title":"Thing"}}`
	})
	dir := t.TempDir()
	t.Setenv("GENTILITY_STATE_DIR", dir)
	t.Setenv("GENTILITY_MCP_URL", srv.URL)
	if err := state.New().SaveAuth(state.Auth{BaseURL: srv.URL, AccessToken: "tok"}); err != nil {
		t.Fatal(err)
	}
	// A directory where the prefs file goes: the write fails, the command does
	// not.
	if err := os.Mkdir(filepath.Join(dir, "prefs.json"), 0o700); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	if code := run([]string{"task", "add", "Thing", "--kind", "ops", "--org", "acme"}, &stdout, &stderr); code != 0 {
		t.Fatalf("run(task add) = %d, want 0:\n%s%s", code, stdout.String(), stderr.String())
	}
	if !strings.HasPrefix(stderr.String(), "note:") {
		t.Fatalf("the warning does not read as a note: %q", stderr.String())
	}
	if strings.Contains(stderr.String(), invoked+" task add:") {
		t.Fatalf("the warning reads as the command failing: %q", stderr.String())
	}
	if !strings.Contains(stdout.String(), "created t1") {
		t.Fatalf("stdout %q", stdout.String())
	}
}

// Signing out forgets everything this laptop remembered about the session,
// the default organization included: the next person at this terminal is not
// meant to inherit the last one's org.
func TestRunLogoutForgetsTheRememberedOrganization(t *testing.T) {
	t.Setenv("GENTILITY_STATE_DIR", t.TempDir())

	store := state.New()
	if err := store.SetRecentOrg("acme"); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if code := run([]string{"logout"}, &out, &out); code != 0 {
		t.Fatalf("run(logout) = %d, want 0:\n%s", code, out.String())
	}
	if got := store.RecentOrg(); got != "" {
		t.Fatalf("RecentOrg = %q after logout, want empty", got)
	}
}

// `join` refuses before it touches a head: a missing variable will be just as
// missing after a restart, so it exits Usage rather than Temporary.
func TestRunJoinWithoutItsEnvironmentIsAUsageError(t *testing.T) {
	t.Setenv(enroll.VarHeadURL, "")

	var out bytes.Buffer
	if code := run([]string{"join"}, &out, &out); code != exitcodes.Usage {
		t.Fatalf("run(join) = %d, want %d:\n%s", code, exitcodes.Usage, out.String())
	}
	if !strings.Contains(out.String(), enroll.VarHeadURL) {
		t.Fatalf("the refusal does not name the missing variable:\n%s", out.String())
	}
}

func TestRunJoinTakesNoArguments(t *testing.T) {
	var out bytes.Buffer
	if code := run([]string{"join", "somewhere"}, &out, &out); code != exitcodes.Usage {
		t.Fatalf("run(join somewhere) = %d, want %d", code, exitcodes.Usage)
	}
	if !strings.Contains(out.String(), "somewhere") {
		t.Fatalf("output %q does not name the stray argument", out.String())
	}
}

// deviceFlowServer serves the two OAuth endpoints `login` talks to. The token
// endpoint answers on the first poll, so the command never waits.
func deviceFlowServer(t *testing.T, enrollment map[string]any) *httptest.Server {
	t.Helper()

	mux := http.NewServeMux()
	mux.HandleFunc("/oauth/device/code", func(w http.ResponseWriter, r *http.Request) {
		writeDeviceJSON(w, map[string]any{
			"device_code":      "dev-1",
			"user_code":        "ABCD-EFGH",
			"verification_uri": "https://mcp.example/device",
			"expires_in":       900,
			"interval":         5,
		})
	})
	mux.HandleFunc("/oauth/token", func(w http.ResponseWriter, r *http.Request) {
		body := map[string]any{"access_token": "tok", "token_type": "Bearer", "expires_in": 60}
		if enrollment != nil {
			body["enrollment"] = enrollment
		}
		writeDeviceJSON(w, body)
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func writeDeviceJSON(w http.ResponseWriter, body map[string]any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(body)
}

func TestRunLoginServerStoresTheEnrollment(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("GENTILITY_STATE_DIR", dir)
	srv := deviceFlowServer(t, map[string]any{
		"enrollment_id": "enr-1",
		"preauth_key":   "tskey-auth-abc",
		"nonce":         "nonce-xyz",
		"head_url":      "https://head.example",
		"expires_at":    "2026-09-06T13:00:00Z",
	})

	var stdout, stderr bytes.Buffer
	if code := run([]string{"login", "--server", "--url", srv.URL}, &stdout, &stderr); code != 0 {
		t.Fatalf("run login --server = %d, want 0:\n%s%s", code, stdout.String(), stderr.String())
	}

	enrollment, err := (&state.Store{Dir: dir}).LoadEnrollment()
	if err != nil {
		t.Fatalf("LoadEnrollment: %v", err)
	}
	if enrollment.PreauthKey != "tskey-auth-abc" {
		t.Fatalf("enrollment = %v, want the one the token carried", enrollment)
	}
}

// The flag never reaches the server: it is what this machine expected of the
// approval, so an approval that named no server has to fail here.
func TestRunLoginServerWithoutAnEnrollmentFails(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("GENTILITY_STATE_DIR", dir)
	srv := deviceFlowServer(t, nil)

	var stdout, stderr bytes.Buffer
	if code := run([]string{"login", "--server", "--url", srv.URL}, &stdout, &stderr); code == 0 {
		t.Fatalf("run login --server = 0, want non-zero:\n%s%s", stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "This is a server for") {
		t.Fatalf("stderr = %q, want the approval to be named", stderr.String())
	}
	if _, err := (&state.Store{Dir: dir}).LoadEnrollment(); !state.IsNoEnrollment(err) {
		t.Fatalf("LoadEnrollment = %v, want no enrollment stored", err)
	}
}

// Installing a service that cannot serve would leave the supervisor
// restarting a refusal forever. The connector says so where the person can
// read it, and exits the way it exits every other "you have not done the
// thing yet".
func TestRunInstallRefusesWhenNothingIsEnrolled(t *testing.T) {
	t.Setenv("GENTILITY_STATE_DIR", t.TempDir())

	var stdout, stderr bytes.Buffer
	code := run([]string{"install", "--state-dir", t.TempDir()}, &stdout, &stderr)
	if code != exitcodes.Usage {
		t.Fatalf("run install = %d, want %d:\n%s%s", code, exitcodes.Usage, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "connector login --server") {
		t.Fatalf("stderr = %q, want it to say what to run", stderr.String())
	}
}

// `serve` on a machine nobody enrolled is the second step asked before the
// first. It refuses the way a usage error refuses — exit 2, which the unit
// files list as one of the codes a supervisor must not restart into — and says
// what to type, because the person reading it is at a terminal and not at a
// log.
func TestRunServeRefusesAMachineThatWasNeverEnrolled(t *testing.T) {
	var stdout, stderr bytes.Buffer

	code := run([]string{"serve", "--state-dir", t.TempDir()}, &stdout, &stderr)

	if code != exitcodes.Usage {
		t.Fatalf("run(serve) = %d, want %d:\n%s%s", code, exitcodes.Usage, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "not enrolled") {
		t.Fatalf("stderr does not say the machine is not enrolled: %q", stderr.String())
	}
	if !strings.Contains(stderr.String(), "login --server") {
		t.Fatalf("stderr does not say what to type: %q", stderr.String())
	}
}

// `install --interface` writes a unit that runs `serve --interface`, so serve
// has to take the flag: refused as unknown, it would exit Usage for the wrong
// reason and the supervisor would never restart it.
func TestRunServeAcceptsInterface(t *testing.T) {
	var stdout, stderr bytes.Buffer

	code := run([]string{"serve", "--state-dir", t.TempDir(), "--interface"}, &stdout, &stderr)

	if code != exitcodes.Usage || !strings.Contains(stderr.String(), "not enrolled") {
		t.Fatalf("run(serve --interface) = %d, want the not-enrolled refusal:\n%s%s",
			code, stdout.String(), stderr.String())
	}
}

// A person who ran serve --interface without sudo is told so and exits Usage,
// which the units forbid restarting into, rather than Temporary, which a
// supervisor would retry every two seconds. The identity is a rejoin's, so the
// refusal is the engine's root check and comes before any network.
func TestRunServeInterfaceWithoutRootIsAUsageError(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("as root, interface mode would try to create a TUN")
	}
	dir := t.TempDir()
	node := state.Node{NodeID: "node-1", NodeCredential: "secret", HeadURL: "https://head.invalid", Hostname: "willow"}
	if err := (&state.Store{Dir: dir}).SaveNode(node); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := run([]string{"serve", "--state-dir", dir, "--interface"}, &stdout, &stderr)

	if code != exitcodes.Usage {
		t.Fatalf("run(serve --interface) = %d, want %d:\n%s%s", code, exitcodes.Usage, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "interface mode needs root") {
		t.Fatalf("stderr does not say root is needed: %q", stderr.String())
	}
}

// The connector's half of a refused rejoin. enroll names the exit code (see
// enroll.upRefusal); this is where the process actually exits it, and where
// the operator reads why — the sentinel's own words and not a summary of them.
//
// serve, join, gateway, egress and proxy all end this way, so narrate is the
// one place it has to be right.
func TestARefusedRejoinExitsEnrollRefusedSayingWhy(t *testing.T) {
	var stdout, stderr bytes.Buffer
	_, complain := narrate("serve", &stdout, &stderr)

	refused := exitcodes.Wrap(exitcodes.EnrollRefused, fmt.Errorf(
		"tsnode: rejoining %s from %q: %w",
		"https://head.example", "/var/lib/gentility/tailscale", tsnode.ErrRejoinRefused,
	))

	if code := complain(refused); code != exitcodes.EnrollRefused {
		t.Fatalf("a refused rejoin exits %d, want %d — a restart would find the same refusal",
			code, exitcodes.EnrollRefused)
	}
	if !strings.Contains(stderr.String(), tsnode.ErrRejoinRefused.Error()) {
		t.Errorf("the operator is not told what to do:\n%s", stderr.String())
	}
	if !strings.Contains(stderr.String(), "/var/lib/gentility/tailscale") {
		t.Errorf("the operator is not told which state directory is stale:\n%s", stderr.String())
	}
}

// And a join that failed for an ordinary reason still exits Temporary, because
// a supervisor restarting IS the retry.
func TestAnOrdinaryJoinFailureExitsTemporary(t *testing.T) {
	var stdout, stderr bytes.Buffer
	_, complain := narrate("serve", &stdout, &stderr)

	if code := complain(errors.New("tsnode: joining https://head.example: dial tcp: connection refused")); code != exitcodes.Temporary {
		t.Fatalf("an ordinary join failure exits %d, want %d", code, exitcodes.Temporary)
	}
}

// --binary supervises a binary where it is and --place-binary copies this one;
// asking for both is a mistake in the command line, not a failed install.
func TestInstallWithBinaryAndPlaceBinaryIsAUsageError(t *testing.T) {
	dir := t.TempDir()
	var stdout, stderr bytes.Buffer
	code := run([]string{"install",
		"--state-dir", filepath.Join(dir, "state"), "--log-dir", filepath.Join(dir, "log"),
		"--binary", "/usr/local/bin/connector", "--place-binary", "/usr/local/bin/gentility",
	}, &stdout, &stderr)
	if code != exitcodes.Usage {
		t.Fatalf("run(install --binary --place-binary) = %d, want %d:\n%s", code, exitcodes.Usage, stderr.String())
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("a refused install wrote %v", entries)
	}
}

func TestServeStampsEachLineWithTheTime(t *testing.T) {
	var out bytes.Buffer
	say, _ := narrate("serve", &out, io.Discard)
	at := time.Date(2026, 10, 27, 9, 30, 0, 0, time.FixedZone("ICT", 7*3600))
	stamped(say, func() time.Time { return at })("config rev %d applied", 7)
	if got, want := out.String(), "2026-10-27T02:30:00Z config rev 7 applied\n"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}
