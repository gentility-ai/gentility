package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// capture records what the server saw, under a lock, so `go test -race` has
// nothing to complain about when the assertions read it back.
type capture struct {
	mu     sync.Mutex
	path   string
	method string
	header http.Header
	body   map[string]any
}

func (c *capture) record(r *http.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.path = r.URL.Path
	c.method = r.Method
	c.header = r.Header.Clone()
	c.body = map[string]any{}
	json.NewDecoder(r.Body).Decode(&c.body)
}

func (c *capture) seen() (string, string, http.Header, map[string]any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.path, c.method, c.header, c.body
}

// serve stands up a server that records the request and replies with a fixed
// status, content type and body.
func serve(t *testing.T, status int, contentType, body string) (*httptest.Server, *capture) {
	t.Helper()
	cap := &capture{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cap.record(r)
		w.Header().Set("Content-Type", contentType)
		w.WriteHeader(status)
		io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv, cap
}

// jsonRPC wraps a result in the envelope the transport sends back, echoing the
// request's own id the way a real server does.
func jsonRPC(result string) string {
	return `{"jsonrpc":"2.0","id":1,"result":` + result + `}`
}

// taskResult is what TaskHelpers.result/2 produces: the payload JSON-encoded
// inside a single text content block.
func taskResult(payload string) string {
	text, _ := json.Marshal(payload)
	return `{"content":[{"type":"text","text":` + string(text) + `}]}`
}

// assertRequestMeta checks the `_meta` every 2026-07-28 request must carry.
//
// RequestParams, PaginatedRequestParams and CallToolRequestParams all declare
// `required: [:_meta]`, and RequestMetaObject in turn requires both
// io.modelcontextprotocol/clientCapabilities and
// io.modelcontextprotocol/protocolVersion. GenMCP.Validator validates the body
// against that schema before any handler is reached, so a request without it
// is refused by the transport, not by a tool.
func assertRequestMeta(t *testing.T, params map[string]any) {
	t.Helper()

	meta, ok := params["_meta"].(map[string]any)
	if !ok {
		t.Fatalf("params._meta = %v — every request must carry one (MCP:RequestParams requires it)", params["_meta"])
	}
	if _, ok := meta["io.modelcontextprotocol/clientCapabilities"].(map[string]any); !ok {
		t.Fatalf("_meta clientCapabilities = %v, want an object", meta["io.modelcontextprotocol/clientCapabilities"])
	}
	// The transport compares this against the MCP-Protocol-Version header and
	// refuses the request if they disagree.
	if got := meta["io.modelcontextprotocol/protocolVersion"]; got != ProtocolVersion {
		t.Fatalf("_meta protocolVersion = %v, want %q — it must match the header", got, ProtocolVersion)
	}
}

func TestCallToolSendsTheRequiredHeaders(t *testing.T) {
	srv, cap := serve(t, 200, "application/json",
		jsonRPC(taskResult(`{"success":true,"task":{"id":"t1"}}`)))

	c := New(srv.URL, "tok-123")
	out, err := c.CallTool(context.Background(), "task_create", map[string]any{
		"organization": "acme",
		"kinds":        []string{"ops"},
		"title":        "Rotate the key",
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}

	path, method, header, body := cap.seen()

	if method != http.MethodPost {
		t.Fatalf("method = %q, want POST", method)
	}
	if path != "/" {
		t.Fatalf("path = %q — the MCP endpoint is the host's root path", path)
	}
	if h := header.Get("Authorization"); h != "Bearer tok-123" {
		t.Fatalf("Authorization = %q", h)
	}
	if h := header.Get("MCP-Protocol-Version"); h != ProtocolVersion {
		t.Fatalf("MCP-Protocol-Version = %q, want %q", h, ProtocolVersion)
	}
	if h := header.Get("Mcp-Method"); h != "tools/call" {
		t.Fatalf("Mcp-Method = %q, want tools/call", h)
	}
	if h := header.Get("Mcp-Name"); h != "task_create" {
		t.Fatalf("Mcp-Name = %q, want task_create", h)
	}
	if h := header.Get("Content-Type"); h != "application/json" {
		t.Fatalf("Content-Type = %q", h)
	}
	if h := header.Get("Accept"); !strings.Contains(h, "application/json") {
		t.Fatalf("Accept = %q, want it to include application/json", h)
	}
	if h := header.Get("Accept"); !strings.Contains(h, "text/event-stream") {
		t.Fatalf("Accept = %q, want it to include text/event-stream", h)
	}

	if body["jsonrpc"] != "2.0" {
		t.Fatalf("body jsonrpc = %v", body["jsonrpc"])
	}
	if body["method"] != "tools/call" {
		t.Fatalf("body method = %v", body["method"])
	}
	if body["id"] == nil {
		t.Fatal("body carries no id; a request without one is a notification")
	}
	params, ok := body["params"].(map[string]any)
	if !ok {
		t.Fatalf("body params = %v", body["params"])
	}
	if params["name"] != "task_create" {
		t.Fatalf("params.name = %v — Mcp-Name must mirror it", params["name"])
	}
	args, ok := params["arguments"].(map[string]any)
	if !ok {
		t.Fatalf("params.arguments = %v", params["arguments"])
	}
	if args["organization"] != "acme" || args["title"] != "Rotate the key" {
		t.Fatalf("arguments = %v", args)
	}
	assertRequestMeta(t, params)

	task, ok := out["task"].(map[string]any)
	if !ok || task["id"] != "t1" {
		t.Fatalf("payload = %v — the inner JSON must be decoded too", out)
	}
}

func TestCallToolSurfacesAToolLevelRefusal(t *testing.T) {
	srv, _ := serve(t, 200, "application/json",
		jsonRPC(taskResult(`{"success":false,"error":"task not found"}`)))

	_, err := New(srv.URL, "tok").CallTool(context.Background(), "task_close", map[string]any{})
	if err == nil {
		t.Fatal("a success:false payload returned no error")
	}

	var refused *ToolRefusedError
	if !errors.As(err, &refused) {
		t.Fatalf("err = %#v, want *ToolRefusedError", err)
	}
	if refused.Tool != "task_close" {
		t.Fatalf("Tool = %q", refused.Tool)
	}
	if refused.Value != "task not found" {
		t.Fatalf("Value = %#v, want the error value verbatim", refused.Value)
	}
	if !strings.Contains(err.Error(), "task not found") {
		t.Fatalf("Error() = %q, want it to carry the server's words", err.Error())
	}
}

func TestAStructuredRefusalKeepsItsShape(t *testing.T) {
	srv, _ := serve(t, 200, "application/json",
		jsonRPC(taskResult(`{"success":false,"error":{"code":"unknown_kinds","unknown":[{"slug":"opz"}]}}`)))

	_, err := New(srv.URL, "tok").CallTool(context.Background(), "task_create", map[string]any{})

	var refused *ToolRefusedError
	if !errors.As(err, &refused) {
		t.Fatalf("err = %#v, want *ToolRefusedError", err)
	}
	value, ok := refused.Value.(map[string]any)
	if !ok {
		t.Fatalf("Value = %#v, want the structured error decoded, not flattened", refused.Value)
	}
	if value["code"] != "unknown_kinds" {
		t.Fatalf("Value = %v", value)
	}
	if !strings.Contains(err.Error(), "unknown_kinds") {
		t.Fatalf("Error() = %q", err.Error())
	}
}

func TestCallToolSurfacesABoundaryErrorResult(t *testing.T) {
	// the MCP tool boundary refuses with isError:true and an
	// {"errors":[…]} envelope — no `success` key at all. Reading that as a
	// successful payload is exactly the silent wrong answer to avoid.
	srv, _ := serve(t, 200, "application/json",
		jsonRPC(`{"isError":true,"content":[{"type":"text","text":"{\"errors\":[{\"category\":\"invalid_arguments\",\"message\":\"organization is required\",\"severity\":\"error\"}]}"}]}`))

	_, err := New(srv.URL, "tok").CallTool(context.Background(), "task_list", map[string]any{})
	if err == nil {
		t.Fatal("an isError result returned no error")
	}

	var toolErr *ToolError
	if !errors.As(err, &toolErr) {
		t.Fatalf("err = %#v, want *ToolError", err)
	}
	if toolErr.Tool != "task_list" {
		t.Fatalf("Tool = %q", toolErr.Tool)
	}
	if !strings.Contains(err.Error(), "organization is required") {
		t.Fatalf("Error() = %q, want the boundary's message", err.Error())
	}

	var refused *ToolRefusedError
	if errors.As(err, &refused) {
		t.Fatal("a boundary error is not a tool refusal; the two must not conflate")
	}
}

func TestCallToolSurfacesAJSONRPCError(t *testing.T) {
	srv, _ := serve(t, 200, "application/json",
		`{"jsonrpc":"2.0","id":1,"error":{"code":-32602,"message":"Mcp-Name header missing"}}`)

	_, err := New(srv.URL, "tok").CallTool(context.Background(), "task_list", map[string]any{})
	if err == nil {
		t.Fatal("a JSON-RPC error object returned no error")
	}

	var rpcErr *RPCError
	if !errors.As(err, &rpcErr) {
		t.Fatalf("err = %#v, want *RPCError", err)
	}
	if rpcErr.Code != -32602 {
		t.Fatalf("Code = %d", rpcErr.Code)
	}
	if !strings.Contains(err.Error(), "Mcp-Name header missing") {
		t.Fatalf("Error() = %q", err.Error())
	}
}

func TestCallToolSurfacesA401AsUnauthorized(t *testing.T) {
	srv, _ := serve(t, 401, "application/json", `{"error":"unauthorized"}`)

	_, err := New(srv.URL, "stale-token").CallTool(context.Background(), "task_list", map[string]any{})
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("err = %#v, want ErrUnauthorized so the caller can say `login`", err)
	}

	var httpErr *HTTPError
	if errors.As(err, &httpErr) {
		t.Fatal("a 401 is a login problem, not a generic HTTP failure; the two must not conflate")
	}
}

func TestCallToolSurfacesANon2xxStatus(t *testing.T) {
	srv, _ := serve(t, 503, "text/plain", "upstream is down")

	_, err := New(srv.URL, "tok").CallTool(context.Background(), "task_list", map[string]any{})

	var httpErr *HTTPError
	if !errors.As(err, &httpErr) {
		t.Fatalf("err = %#v, want *HTTPError", err)
	}
	if httpErr.StatusCode != 503 {
		t.Fatalf("StatusCode = %d", httpErr.StatusCode)
	}
	if !strings.Contains(err.Error(), "upstream is down") {
		t.Fatalf("Error() = %q, want the body to be shown", err.Error())
	}
	if errors.Is(err, ErrUnauthorized) {
		t.Fatal("a 503 must not read as a login problem")
	}
}

func TestCallToolReadsAnSSEResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID int64 `json:"id"`
		}
		json.NewDecoder(r.Body).Decode(&req)

		w.Header().Set("Content-Type", "text/event-stream")
		// A server-initiated request rides the same stream; it is not our
		// answer and must be stepped over.
		io.WriteString(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":9001,\"method\":\"ping\"}\n\n")
		// So must somebody else's answer — a complete result, wrong id.
		io.WriteString(w, "event: message\ndata: "+
			`{"jsonrpc":"2.0","id":9002,"result":`+
			taskResult(`{"success":true,"task":{"id":"not-ours"}}`)+"}\n\n")
		io.WriteString(w, "event: message\ndata: "+
			`{"jsonrpc":"2.0","id":`+strconv.FormatInt(req.ID, 10)+`,"result":`+
			taskResult(`{"success":true,"task":{"id":"t2"}}`)+"}\n\n")
	}))
	defer srv.Close()

	out, err := New(srv.URL, "tok").CallTool(context.Background(), "task_get", map[string]any{})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	task, ok := out["task"].(map[string]any)
	if !ok || task["id"] != "t2" {
		t.Fatalf("payload = %v", out)
	}
}

func TestListToolsReturnsSchemas(t *testing.T) {
	srv, cap := serve(t, 200, "application/json", jsonRPC(`{"tools":[
		{"name":"task_create","description":"Create a task","inputSchema":{"type":"object","properties":{"organization":{"type":"string"}},"required":["organization","kinds"]}},
		{"name":"task_list","inputSchema":{"type":"object","properties":{"status":{"type":"string"}},"required":["organization"]}}
	]}`))

	tools, err := New(srv.URL, "tok").ListTools(context.Background())
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(tools) != 2 {
		t.Fatalf("got %d tools, want 2", len(tools))
	}
	if tools[0].Name != "task_create" {
		t.Fatalf("tools[0].Name = %q", tools[0].Name)
	}
	props, ok := tools[0].InputSchema["properties"].(map[string]any)
	if !ok || props["organization"] == nil {
		t.Fatalf("inputSchema = %v", tools[0].InputSchema)
	}

	_, _, header, body := cap.seen()
	if h := header.Get("Mcp-Method"); h != "tools/list" {
		t.Fatalf("Mcp-Method = %q", h)
	}
	if h := header.Get("Mcp-Name"); h != "" {
		t.Fatalf("Mcp-Name = %q — tools/list has no name to mirror, so sending one is a lie", h)
	}
	if body["method"] != "tools/list" {
		t.Fatalf("body method = %v", body["method"])
	}
	params, ok := body["params"].(map[string]any)
	if !ok {
		t.Fatalf("body params = %v — MCP:ListToolsRequest requires params", body["params"])
	}
	assertRequestMeta(t, params)
}

func TestInitializeIntroducesTheClient(t *testing.T) {
	srv, cap := serve(t, 200, "application/json",
		jsonRPC(`{"protocolVersion":"2026-07-28","serverInfo":{"name":"Gentility","version":"0.1.0"},"instructions":"read this"}`))

	info, err := New(srv.URL, "tok").Initialize(context.Background())
	if err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if info.ServerInfo.Name != "Gentility" || info.ServerInfo.Version != "0.1.0" {
		t.Fatalf("serverInfo = %+v", info.ServerInfo)
	}
	if info.ProtocolVersion != ProtocolVersion {
		t.Fatalf("protocolVersion = %q", info.ProtocolVersion)
	}
	if info.Instructions != "read this" {
		t.Fatalf("instructions = %q", info.Instructions)
	}

	_, _, header, body := cap.seen()
	if h := header.Get("Mcp-Method"); h != "initialize" {
		t.Fatalf("Mcp-Method = %q", h)
	}
	params, _ := body["params"].(map[string]any)
	clientInfo, ok := params["clientInfo"].(map[string]any)
	if !ok {
		t.Fatalf("params = %v, want clientInfo for the server's telemetry", params)
	}
	if clientInfo["name"] != ClientName {
		t.Fatalf("clientInfo.name = %v, want %q", clientInfo["name"], ClientName)
	}
	if clientInfo["version"] == "" || clientInfo["version"] == nil {
		t.Fatalf("clientInfo.version = %v", clientInfo["version"])
	}
	if params["protocolVersion"] != ProtocolVersion {
		t.Fatalf("params.protocolVersion = %v", params["protocolVersion"])
	}
	// `initialize` is not in the 2026-07-28 request surface at all, so no
	// schema demands a `_meta` here. It carries one anyway: `do` puts it on
	// every request, and a rule with an exception is a rule someone has to
	// remember.
	assertRequestMeta(t, params)
}

func TestTheEndpointIsTheHostRootWhateverThePathOnTheBaseURL(t *testing.T) {
	srv, cap := serve(t, 200, "application/json", jsonRPC(taskResult(`{"success":true}`)))

	// A base URL carrying a path is still an MCP host; the endpoint is its
	// root, never `<path>/mcp`.
	c := New(srv.URL+"/some/leftover/path", "tok")
	if _, err := c.CallTool(context.Background(), "task_list", map[string]any{}); err != nil {
		t.Fatalf("CallTool: %v", err)
	}

	if path, _, _, _ := cap.seen(); path != "/" {
		t.Fatalf("path = %q, want /", path)
	}
}

func TestNewRejectsAnUnparseableBaseURL(t *testing.T) {
	c := New("://not a url", "tok")
	if _, err := c.CallTool(context.Background(), "task_list", map[string]any{}); err == nil {
		t.Fatal("an unparseable base URL produced no error")
	}
}

func TestCallToolRefusesAMalformedResult(t *testing.T) {
	srv, _ := serve(t, 200, "application/json", jsonRPC(`{"content":[]}`))

	if _, err := New(srv.URL, "tok").CallTool(context.Background(), "task_list", map[string]any{}); err == nil {
		t.Fatal("a result with no content block produced no error")
	}
}

func TestCallToolHonoursACancelledContext(t *testing.T) {
	srv, _ := serve(t, 200, "application/json", jsonRPC(taskResult(`{"success":true}`)))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := New(srv.URL, "tok").CallTool(ctx, "task_list", map[string]any{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

// --- review follow-ups -------------------------------------------------

func TestANon2xxJSONRPCErrorIsAnRPCError(t *testing.T) {
	// GenMCP's stateless transport refuses a bad protocol version with a
	// JSON-RPC error object under HTTP 400. Reading that as a bare status code
	// loses the one sentence that says what to fix.
	srv, _ := serve(t, 400, "application/json",
		`{"jsonrpc":"2.0","id":1,"error":{"code":-32004,"message":"Unsupported protocol version"}}`)

	_, err := New(srv.URL, "tok").CallTool(context.Background(), "task_list", map[string]any{})

	var rpcErr *RPCError
	if !errors.As(err, &rpcErr) {
		t.Fatalf("err = %#v, want *RPCError", err)
	}
	if rpcErr.Code != -32004 {
		t.Fatalf("Code = %d, want -32004", rpcErr.Code)
	}
	if !strings.Contains(err.Error(), "Unsupported protocol version") {
		t.Fatalf("Error() = %q, want the server's sentence", err.Error())
	}

	var httpErr *HTTPError
	if errors.As(err, &httpErr) {
		t.Fatal("a 4xx carrying a JSON-RPC error is that error, not a generic HTTP failure")
	}
}

func TestANon2xxJSONBodyThatIsNotJSONRPCStaysAnHTTPError(t *testing.T) {
	srv, _ := serve(t, 400, "application/json", `{"error":"invalid_request"}`)

	_, err := New(srv.URL, "tok").CallTool(context.Background(), "task_list", map[string]any{})

	var httpErr *HTTPError
	if !errors.As(err, &httpErr) {
		t.Fatalf("err = %#v, want *HTTPError", err)
	}
	if httpErr.StatusCode != 400 {
		t.Fatalf("StatusCode = %d", httpErr.StatusCode)
	}
}

func TestA401StaysUnauthorizedEvenCarryingAJSONRPCError(t *testing.T) {
	srv, _ := serve(t, 401, "application/json",
		`{"jsonrpc":"2.0","id":1,"error":{"code":-32001,"message":"unauthorized"}}`)

	_, err := New(srv.URL, "stale").CallTool(context.Background(), "task_list", map[string]any{})
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("err = %#v, want ErrUnauthorized — a login problem outranks its wire shape", err)
	}
}

// slowServer answers every request after delay, so a timeout can be observed
// without guessing at scheduling. A client that gives up first releases the
// handler at once, so a test never waits out a delay nobody is going to see.
func slowServer(t *testing.T, delay time.Duration) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(delay):
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, jsonRPC(taskResult(`{"success":true,"task":{"id":"slow"}}`)))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestALongerCallerDeadlineOutranksTheDefaultTimeout(t *testing.T) {
	srv := slowServer(t, 150*time.Millisecond)

	c := New(srv.URL, "tok")
	c.timeout = 20 * time.Millisecond // stand in for DefaultTimeout

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	out, err := c.CallTool(ctx, "task_list", nil)
	if err != nil {
		t.Fatalf("the caller's deadline must win over the default: %v", err)
	}
	if task, ok := out["task"].(map[string]any); !ok || task["id"] != "slow" {
		t.Fatalf("payload = %v", out)
	}
}

func TestTheDefaultTimeoutAppliesWhenTheCallerSetsNoDeadline(t *testing.T) {
	srv := slowServer(t, 500*time.Millisecond)

	c := New(srv.URL, "tok")
	c.timeout = 20 * time.Millisecond

	_, err := c.CallTool(context.Background(), "task_list", nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
}

func TestARefusalWithACodeAndAMessageReadsAsASentence(t *testing.T) {
	// TaskHelpers.error_value(:run_in_flight).
	srv, _ := serve(t, 200, "application/json", jsonRPC(taskResult(
		`{"success":false,"error":{"code":"run_in_flight","message":"A looper is already working this task"}}`)))

	_, err := New(srv.URL, "tok").CallTool(context.Background(), "task_wake", nil)
	if got, want := err.Error(), "task_wake: run_in_flight: A looper is already working this task"; got != want {
		t.Fatalf("Error() = %q, want %q", got, want)
	}
}

func TestARefusalWithACodeAndNoMessageLeadsWithTheCode(t *testing.T) {
	// TaskHelpers.error_value({:unknown_kinds, …}) — a code and a list, no
	// sentence. Lead with the word that names the problem, then the detail.
	srv, _ := serve(t, 200, "application/json", jsonRPC(taskResult(
		`{"success":false,"error":{"code":"unknown_kinds","unknown":[{"slug":"opz","requested":"opz","suggestions":["ops"]}]}}`)))

	_, err := New(srv.URL, "tok").CallTool(context.Background(), "task_create", nil)
	if !strings.HasPrefix(err.Error(), "task_create: unknown_kinds: ") {
		t.Fatalf("Error() = %q, want it to lead with the code", err.Error())
	}
	if !strings.Contains(err.Error(), `"suggestions":["ops"]`) {
		t.Fatalf("Error() = %q, want the detail kept", err.Error())
	}
}

func TestARefusalWithNoCodeButAMessageIsJustTheMessage(t *testing.T) {
	srv, _ := serve(t, 200, "application/json", jsonRPC(taskResult(
		`{"success":false,"error":{"message":"that task is already closed"}}`)))

	_, err := New(srv.URL, "tok").CallTool(context.Background(), "task_close", nil)
	if got, want := err.Error(), "task_close: that task is already closed"; got != want {
		t.Fatalf("Error() = %q, want %q", got, want)
	}
}

func TestASuccessKeyThatIsNotTrueIsARefusal(t *testing.T) {
	// `success` is a claim, and only `true` makes it. A string "false", a 0 or
	// a null must never read as a successful call.
	for _, tc := range []struct{ name, payload string }{
		{"a string false", `{"success":"false","error":"nope"}`},
		{"a zero", `{"success":0,"error":"nope"}`},
		{"a null", `{"success":null,"error":"nope"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := serve(t, 200, "application/json", jsonRPC(taskResult(tc.payload)))

			_, err := New(srv.URL, "tok").CallTool(context.Background(), "task_list", nil)

			var refused *ToolRefusedError
			if !errors.As(err, &refused) {
				t.Fatalf("err = %#v, want *ToolRefusedError", err)
			}
			if refused.Value != "nope" {
				t.Fatalf("Value = %#v", refused.Value)
			}
		})
	}
}

func TestNormalizeBaseURL(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"a bare origin", "https://mcp.example.com", "https://mcp.example.com"},
		{"a trailing slash", "https://mcp.example.com/", "https://mcp.example.com"},
		{"a path, a query and a fragment", "https://mcp.example.com/mcp?x=1#f", "https://mcp.example.com"},
		{"userinfo", "https://someone:secret@mcp.example.com/mcp", "https://mcp.example.com"},
		{"a port", "http://127.0.0.1:9000/x", "http://127.0.0.1:9000"},
		{"surrounding space", "  https://mcp.example.com  ", "https://mcp.example.com"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NormalizeBaseURL(tc.in)
			if err != nil {
				t.Fatalf("NormalizeBaseURL(%q): %v", tc.in, err)
			}
			if got != tc.want {
				t.Fatalf("NormalizeBaseURL(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}

	for _, tc := range []struct{ name, in string }{
		{"empty", ""},
		{"no scheme", "mcp.example.com"},
		{"no host", "https://"},
		{"unparseable", "://not a url"},
	} {
		t.Run(tc.name+" is refused", func(t *testing.T) {
			if got, err := NormalizeBaseURL(tc.in); err == nil {
				t.Fatalf("NormalizeBaseURL(%q) = %q, want an error", tc.in, got)
			}
		})
	}
}

func TestNewDropsUserinfoSoItNeverReachesTheWire(t *testing.T) {
	srv, cap := serve(t, 200, "application/json", jsonRPC(taskResult(`{"success":true}`)))

	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	u.User = url.UserPassword("someone", "secret")

	c := New(u.String(), "tok")
	if strings.Contains(c.Endpoint(), "secret") || strings.Contains(c.Endpoint(), "@") {
		t.Fatalf("Endpoint() = %q, want the userinfo dropped", c.Endpoint())
	}
	if _, err := c.CallTool(context.Background(), "task_list", nil); err != nil {
		t.Fatalf("CallTool: %v", err)
	}

	_, _, header, _ := cap.seen()
	if got := header.Get("Authorization"); got != "Bearer tok" {
		t.Fatalf("Authorization = %q — the bearer is the only credential we send", got)
	}
}

func TestCallToolReadsSSEFraming(t *testing.T) {
	// A fresh client's first request is id 1, which is what jsonRPC echoes.
	const head = `{"jsonrpc":"2.0","id":1,"result":`
	tail := taskResult(`{"success":true,"task":{"id":"t3"}}`) + `}`

	for _, tc := range []struct{ name, stream string }{
		{"one data line", "event: message\ndata: " + head + tail + "\n\n"},
		{"no space after data:", "event: message\ndata:" + head + tail + "\n\n"},
		{"CRLF, data split over two lines",
			"event: message\r\ndata: " + head + "\r\ndata: " + tail + "\r\n\r\n"},
		{"no trailing blank line", "event: message\ndata: " + head + tail + "\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				io.WriteString(w, tc.stream)
			}))
			defer srv.Close()

			out, err := New(srv.URL, "tok").CallTool(context.Background(), "task_get", nil)
			if err != nil {
				t.Fatalf("CallTool: %v", err)
			}
			task, ok := out["task"].(map[string]any)
			if !ok || task["id"] != "t3" {
				t.Fatalf("payload = %v", out)
			}
		})
	}
}

// A URL that fails to parse is quoted back at the person so they can see what
// they typed — and what they typed may well be a URL with a password in it.
// Go's own *url.Error carries the raw string, so wrapping it is how a
// credential reaches a terminal, a log file and a bug report in one step.
func TestNormalizeBaseURLNeverEchoesUserinfo(t *testing.T) {
	for _, tc := range []struct{ name, in string }{
		{"unparseable", "https://someone:hunter2@mcp.example.com/%zz"},
		{"a control character", "https://someone:hunter2@mcp.example.com/\x7f"},
		{"no scheme", "someone:hunter2@mcp.example.com"},
		{"no host", "https://someone:hunter2@"},
		{"a scheme we do not speak", "ftp://someone:hunter2@mcp.example.com"},
		{"plain http to a remote host", "http://someone:hunter2@mcp.example.com"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NormalizeBaseURL(tc.in)
			if err == nil {
				t.Fatalf("NormalizeBaseURL(%q) = %q, want an error", tc.in, got)
			}
			if strings.Contains(err.Error(), "hunter2") {
				t.Fatalf("the error leaked the password:\n%s", err)
			}
		})
	}
}

// http is a bearer travelling in clear text. It is worth having on a laptop
// talking to its own dev server and nowhere else, so the loopback names are
// the exception and everything beyond them is refused with the reason.
func TestNormalizeBaseURLAllowsHTTPOnlyOnLoopback(t *testing.T) {
	for _, in := range []string{
		"http://localhost:9000",
		"http://127.0.0.1:9000",
		"http://[::1]:9000",
	} {
		t.Run(in, func(t *testing.T) {
			if _, err := NormalizeBaseURL(in); err != nil {
				t.Fatalf("NormalizeBaseURL(%q): %v — a dev server on this machine is the point of allowing http", in, err)
			}
		})
	}

	for _, in := range []string{
		"http://mcp.gentility.ai",
		"http://192.168.1.10:9000",
		"http://localhost.evil.example",
		"http://mcp.localhost:9000",
	} {
		t.Run(in+" is refused", func(t *testing.T) {
			got, err := NormalizeBaseURL(in)
			if err == nil {
				t.Fatalf("NormalizeBaseURL(%q) = %q, want it refused", in, got)
			}
			if !strings.Contains(err.Error(), "https") {
				t.Fatalf("err = %v, want the remedy named", err)
			}
		})
	}
}

func TestNormalizeBaseURLRefusesASchemeItDoesNotSpeak(t *testing.T) {
	for _, in := range []string{
		"ftp://mcp.example.com",
		"ws://mcp.example.com",
		"file://mcp.example.com",
		"javascript://mcp.example.com",
	} {
		t.Run(in, func(t *testing.T) {
			got, err := NormalizeBaseURL(in)
			if err == nil {
				t.Fatalf("NormalizeBaseURL(%q) = %q, want it refused", in, got)
			}
			if !strings.Contains(err.Error(), "https") {
				t.Fatalf("err = %v, want the scheme it does speak named", err)
			}
		})
	}
}
