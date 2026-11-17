//go:build livemcp

// The live counterpart to schema_test.go.
//
// The checked-in fixture is what makes the argument-mapping tests hermetic,
// and the server's schema drift test is what stops it drifting from the
// schemas this repository's tools declare. Neither of them proves the third
// thing: that a *running* Gentility serves those schemas, over the transport
// and the headers this client speaks, to a real bearer. That is what these
// tests are for, and it is why they are behind a build tag — they need a host
// and a token, and `go test ./...` must stay a thing anyone can run.
//
// Run them against a dev server, with a bearer minted by that same host:
//
//	cd node && go test -tags livemcp ./internal/tasks/ -v \
//	    -url "$WT_MCP_URL" \
//	    -token "$(jq -r .access_token "${GENTILITY_STATE_DIR:-$HOME/.config/gentility}/auth.json")"
//
// $WT_MCP_URL is exported per worktree by direnv, and the token is the one
// `connector login --url "$WT_MCP_URL"` stored (AGENTS.md, "Worktree
// checkouts" — never type a literal port). Against production the host is
// https://mcp.gentility.ai and the bearer must have been minted there: a
// bearer belongs to the origin that issued it and means nothing anywhere
// else. With either flag empty both tests skip, so `go test -tags livemcp
// ./...` on a machine with no server is a pass, not a failure.
package tasks

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gentility-ai/gentility/connector/internal/mcp"
)

var (
	liveURL = flag.String("url", "",
		"MCP host to check the fixture against, e.g. $WT_MCP_URL; empty skips these tests")
	liveToken = flag.String("token", "",
		"a bearer minted by that host (connector login); empty skips these tests")
)

// liveTimeout bounds one call. A tools/list is a few hundred milliseconds
// against a warm dev server and a good deal more against a cold one.
const liveTimeout = 30 * time.Second

// live returns a client for the host under test, or skips.
func live(t *testing.T) (*mcp.Client, context.Context) {
	t.Helper()

	if *liveURL == "" || *liveToken == "" {
		t.Skip("no live host: pass -url and -token (see this file's package comment)")
	}

	ctx, cancel := context.WithTimeout(context.Background(), liveTimeout)
	t.Cleanup(cancel)

	return mcp.New(*liveURL, *liveToken), ctx
}

// fixture is contract/connector/tool_schemas.json decoded the way the wire decodes: into
// plain maps, so numbers are float64 on both sides of the comparison and a
// DeepEqual means what it looks like it means.
func fixture(t *testing.T) map[string]map[string]any {
	t.Helper()

	var schemas map[string]map[string]any
	if err := json.Unmarshal(schemasJSON(t), &schemas); err != nil {
		t.Fatalf("contract/connector/tool_schemas.json is not readable: %v", err)
	}
	return schemas
}

func TestFixtureMatchesLiveToolsList(t *testing.T) {
	client, ctx := live(t)

	tools, err := client.ListTools(ctx)
	if err != nil {
		t.Fatalf("tools/list against %s: %v", client.Endpoint(), err)
	}

	served := make(map[string]map[string]any, len(tools))
	for _, tool := range tools {
		served[tool.Name] = tool.InputSchema
	}

	for name, want := range fixture(t) {
		got, listed := served[name]
		if !listed {
			// tools/list is filtered by what the bearer may call, so a missing
			// tool is as likely to be a membership as a rename.
			t.Errorf("%s does not advertise %s to this bearer — is the membership mcp_enabled, "+
				"and is the plan entitled to it?", client.Endpoint(), name)
			continue
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s: the served schema is not the checked-in one.\n\nserved:\n%s\n\nfixture:\n%s\n\n"+
				"Regenerate contract/connector/tool_schemas.json from the server's tool schemas, "+
				"then re-run the Go tests.",
				name, indent(t, got), indent(t, want))
		}
	}
}

// TestOrganizationIsRequiredOnTheWire proves the CLI's "always send it" rule
// is the server's rule too.
//
// The call must name its organization explicitly. If a task tool answered a call that named no
// organization, that regression would be invisible to every hermetic test in
// this package — the fixture would still say `required`, and the CLI would
// still send it.
//
// The refusal arrives as a *mcp.ToolError, not as a tool's own
// `success:false`: ToolBoundary refuses before the tool is reached
// (`{:error, "organization_not_resolved", message}`), and the boundary's
// refusals come back flagged isError with an {"errors":[…]} envelope. Asserting
// the type is asserting *where* the rule lives.
func TestOrganizationIsRequiredOnTheWire(t *testing.T) {
	client, ctx := live(t)

	payload, err := client.CallTool(ctx, "task_list", map[string]any{"limit": 1})
	if err == nil {
		t.Fatalf("task_list named no organization and was answered anyway: %v", payload)
	}

	var refused *mcp.ToolError
	if !errors.As(err, &refused) {
		t.Fatalf("task_list without an organization failed as %T: %v\nwant a *mcp.ToolError — "+
			"the tool boundary's refusal, raised before the tool runs", err, err)
	}
	if !strings.Contains(strings.ToLower(refused.Message), "organization") {
		t.Fatalf("the refusal does not name the missing argument: %s", refused.Message)
	}
}

func indent(t *testing.T, value any) string {
	t.Helper()

	encoded, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatalf("rendering %v: %v", value, err)
	}
	return string(encoded)
}
