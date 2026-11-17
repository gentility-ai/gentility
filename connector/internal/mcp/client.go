// Package mcp is the connector's client for the Gentility MCP endpoint: three
// calls — initialize, tools/list, tools/call — over the stateless streamable
// HTTP transport, and nothing else.
//
// The wire contract is not negotiable and is not guessed. GenMCP's stateless
// transport rejects a request whose headers do not agree with its body: it
// requires MCP-Protocol-Version to be exactly the version it serves, Mcp-Method
// to repeat the body's `method` case-sensitively, and Mcp-Name to repeat
// `params.name` on tools/call. Sending that version is also what selects the
// stateless generation — the MCP transport routes anything >= "2026" to
// it and everything else to the 2025 compatibility shim — so there is no
// session id to carry and no handshake to complete before the first call.
//
// The body has a required part of its own. Every 2026-07-28 request's `params`
// must carry a `_meta` naming the client's capabilities and the protocol
// version (MCP:RequestParams, MCP:RequestMetaObject), and GenMCP.Validator
// checks the whole message against that schema before dispatch — so a request
// missing it is refused by the transport with no tool involved. `do` adds it to
// every request rather than leaving it to each call site.
//
// A tool's answer is JSON twice over. The MCP tool response serializer encodes
// the payload and puts the string in one text content block, so a caller that
// stops at the MCP envelope has read a string, not a result. This client
// decodes both layers and then reports the four things that can go wrong as
// four distinct types, because they call for four different remedies:
//
//	ErrUnauthorized    the bearer is missing, stale or revoked — log in again
//	*HTTPError         the host answered, but not with an MCP response
//	*RPCError          the transport refused the message itself
//	*ToolError         the tool boundary refused the call (isError:true)
//	*ToolRefusedError  the tool ran and said no (success:false)
//
// Conflating any two of them is how "your token expired" turns into a status
// code on someone's terminal.
package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"github.com/gentility-ai/gentility/connector/internal/terminal"
	"github.com/gentility-ai/gentility/connector/internal/version"
)

const (
	// ProtocolVersion is the generation this client speaks. The server refuses
	// unsupported versions; omitting it selects the legacy 2025 protocol.
	ProtocolVersion = "2026-07-28"

	// ClientName is how the connector introduces itself. The server records it
	// against the session, so it is a name in an audit trail, not decoration.
	ClientName = "connector"

	// DefaultTimeout bounds a call when the caller's context does not — a
	// default, never a ceiling. It is applied by `do` rather than by the
	// http.Client, because an http.Client's Timeout is a hard cap that a caller
	// with a longer deadline cannot lift, and a long tool call is a legitimate
	// thing for a caller to wait for.
	DefaultTimeout = 30 * time.Second

	// maxErrorBody caps how much of a non-MCP response is quoted back. Enough
	// to recognise an nginx page or a proxy's complaint; not enough to fill a
	// terminal.
	maxErrorBody = 2048

	// maxFrameSize bounds one SSE line. A task listing is comfortably inside
	// it and bufio's 64 KiB default is not.
	maxFrameSize = 4 << 20
)

// ErrUnauthorized reports that the MCP host refused the bearer: it is absent,
// expired or revoked. The remedy is a login, which is why this is a sentinel
// the command layer can match on rather than a status code it would have to
// interpret.
var ErrUnauthorized = errors.New("mcp: the bearer was refused")

// HTTPError is a reply that never became an MCP response — a proxy error page,
// a 500, a redirect to a login form.
type HTTPError struct {
	StatusCode int
	Status     string
	Body       string
}

func (e *HTTPError) Error() string {
	if e.Body == "" {
		return fmt.Sprintf("mcp: the host answered %s", terminal.Text(e.Status))
	}
	return fmt.Sprintf("mcp: the host answered %s: %s", terminal.Text(e.Status), terminal.Text(e.Body))
}

// RPCError is the JSON-RPC `error` object: the transport refused the message
// before any tool saw it — a header that disagreed with the body, an unknown
// method, malformed params.
type RPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *RPCError) Error() string {
	return fmt.Sprintf("mcp: the server refused the request (%d): %s", e.Code, terminal.Text(e.Message))
}

// ToolError is a result the server flagged with isError. Gentility's tool
// boundary answers this way when a call is refused before or around the tool —
// a missing required argument, a timeout, a crash — and its text is an
// {"errors":[…]} envelope rather than the tool's own payload.
type ToolError struct {
	Tool    string
	Message string
	Text    string
}

func (e *ToolError) Error() string {
	return fmt.Sprintf("mcp: %s was refused: %s", terminal.Text(e.Tool), terminal.Text(e.Message))
}

// ToolRefusedError is the tool's own no: it ran, decided against the request,
// and said why. Value is the `error` field verbatim, because Gentility's tools
// answer with a structured error where a bare string would leave the caller
// guessing what to do next.
type ToolRefusedError struct {
	Tool  string
	Value any
}

func (e *ToolRefusedError) Error() string {
	return fmt.Sprintf("%s: %s", terminal.Text(e.Tool), terminal.Text(describe(e.Value)))
}

// describe renders an error value for a person.
//
// Gentility's tools refuse in three shapes — TaskHelpers.error_value/1 answers
// with a bare string, with {code, message}, or with a code and structured
// detail — and each deserves to read as what it is. A sentence is printed as a
// sentence; a code leads, because it is the word the reader will search for;
// detail that has no sentence follows the code as the JSON it is.
func describe(value any) string {
	switch v := value.(type) {
	case nil:
		return "refused, with no reason given"
	case string:
		return v
	case map[string]any:
		return describeFields(v)
	default:
		return compact(v)
	}
}

func describeFields(fields map[string]any) string {
	code, _ := fields["code"].(string)
	message, _ := fields["message"].(string)

	switch {
	case code != "" && message != "":
		return code + ": " + message
	case code != "":
		return code + ": " + compact(fields)
	case message != "":
		return message
	default:
		return compact(fields)
	}
}

func compact(value any) string {
	if encoded, err := json.Marshal(value); err == nil {
		return string(encoded)
	}
	return fmt.Sprint(value)
}

// Tool is one entry of tools/list: the name a call names, and the schema the
// arguments must satisfy.
type Tool struct {
	Name        string         `json:"name"`
	Title       string         `json:"title,omitempty"`
	Description string         `json:"description,omitempty"`
	InputSchema map[string]any `json:"inputSchema,omitempty"`
}

// ServerInfo is who answered.
type ServerInfo struct {
	Name    string `json:"name"`
	Title   string `json:"title,omitempty"`
	Version string `json:"version"`
}

// InitializeResult is the handshake's answer. The stateless transport does not
// require the handshake, but it is what `connector status` asks and what tells
// the server which client is calling.
type InitializeResult struct {
	ProtocolVersion string         `json:"protocolVersion"`
	ServerInfo      ServerInfo     `json:"serverInfo"`
	Capabilities    map[string]any `json:"capabilities,omitempty"`
	Instructions    string         `json:"instructions,omitempty"`
}

// NormalizeBaseURL reduces a URL to the origin the connector talks to: scheme
// and host, and nothing else.
//
// Everything the connector reaches on an MCP host hangs off that origin — the
// MCP endpoint at "/", the device flow at "/oauth/…" — so an origin is the only
// thing worth storing, and normalising once is what keeps the login flow and
// the MCP client from ever disagreeing about which host they are on. A path is
// dropped rather than appended to; userinfo is dropped because a credential
// smuggled into a URL is a credential in the next log line.
//
// Only http and https are accepted, and http only to this machine: every call
// this client makes carries a bearer in a header, and a bearer sent in clear
// text over anything but loopback is a bearer somebody else has.
func NormalizeBaseURL(baseURL string) (string, error) {
	trimmed := strings.TrimSpace(baseURL)

	// The error paths quote the input back, and the input may carry a password.
	// url.Parse's own *url.Error embeds the raw string, so it is never wrapped
	// here — the redacted form is the only form that gets printed.
	safe := redactUserinfo(trimmed)

	u, err := url.Parse(trimmed)
	if err != nil {
		return "", fmt.Errorf("mcp: %q is not a URL", safe)
	}
	if u.Scheme == "" || u.Host == "" {
		return "", fmt.Errorf("mcp: %q needs a scheme and a host, like https://mcp.gentility.ai", safe)
	}

	switch u.Scheme {
	case "https":
	case "http":
		if !isLoopback(u.Hostname()) {
			return "", fmt.Errorf(
				"mcp: %q would send the bearer over the network in clear text; use https "+
					"(http is allowed only for localhost, 127.0.0.1 and ::1)", safe)
		}
	default:
		return "", fmt.Errorf(
			"mcp: %q uses the %q scheme; an MCP host is reached over https (or http on localhost)",
			safe, u.Scheme)
	}

	return (&url.URL{Scheme: u.Scheme, Host: u.Host}).String(), nil
}

// isLoopback reports whether a hostname names this machine. It is a name test,
// not a resolution test: what may be reached in clear text has to be decidable
// from the string a person typed, because a name that resolves to 127.0.0.1
// today can resolve elsewhere tomorrow.
func isLoopback(hostname string) bool {
	switch hostname {
	case "localhost", "127.0.0.1", "::1":
		return true
	}
	// The .localhost TLD is reserved by RFC 6761; only development builds
	// accept its subdomains for clear-text MCP connections.
	return version.Connector == "dev" && strings.HasSuffix(hostname, ".localhost")
}

// redactUserinfo removes the userinfo from a URL string, whether or not that
// string parsed.
//
// url.URL.Redacted only helps once a URL has parsed AND its userinfo landed in
// u.User, and neither holds on the paths that need it most: a URL that failed
// to parse never becomes a *url.URL at all, and "someone:hunter2@host" parses
// as an opaque "someone" scheme with the password in the opaque part. So this
// works on the text: everything up to the last "@" of the authority goes.
func redactUserinfo(raw string) string {
	head, authorityAndRest, found := strings.Cut(raw, "//")
	if found {
		head += "//"
	} else {
		head, authorityAndRest = "", raw
	}

	authority := authorityAndRest
	if end := strings.IndexAny(authorityAndRest, "/?#"); end >= 0 {
		authority = authorityAndRest[:end]
	}

	at := strings.LastIndex(authority, "@")
	if at < 0 {
		return raw
	}
	return head + "…@" + authorityAndRest[at+1:]
}

// Client talks to one MCP host as one bearer.
type Client struct {
	endpoint string
	token    string
	err      error // a base URL that never parsed, reported on first use
	http     *http.Client
	timeout  time.Duration
	nextID   atomic.Int64
}

// New returns a client for the MCP host at baseURL. The endpoint is the host's
// root path — the MCP router forwards "/" to the transport — so any
// path on baseURL is dropped rather than appended to.
func New(baseURL, token string) *Client {
	c := &Client{token: token, http: &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}, timeout: DefaultTimeout}

	origin, err := NormalizeBaseURL(baseURL)
	if err != nil {
		c.err = err
		return c
	}
	c.endpoint = origin + "/"
	return c
}

// Endpoint is the URL every call posts to.
func (c *Client) Endpoint() string { return c.endpoint }

// Initialize introduces the client and reports what the server is.
func (c *Client) Initialize(ctx context.Context) (InitializeResult, error) {
	params := map[string]any{
		"protocolVersion": ProtocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo": map[string]any{
			"name":    ClientName,
			"version": version.Connector,
		},
	}

	raw, err := c.do(ctx, "initialize", params, "")
	if err != nil {
		return InitializeResult{}, err
	}

	var result InitializeResult
	if err := json.Unmarshal(raw, &result); err != nil {
		return InitializeResult{}, fmt.Errorf("mcp: initialize returned something unreadable: %w", err)
	}
	return result, nil
}

// ListTools returns every tool the bearer may call, with its advertised input
// schema.
func (c *Client) ListTools(ctx context.Context) ([]Tool, error) {
	raw, err := c.do(ctx, "tools/list", map[string]any{}, "")
	if err != nil {
		return nil, err
	}

	var result struct {
		Tools []Tool `json:"tools"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, fmt.Errorf("mcp: tools/list returned something unreadable: %w", err)
	}
	return result.Tools, nil
}

// CallTool runs one tool and returns its payload — the inner JSON, decoded.
//
// A tool that says no returns an error, never an empty map: a refusal that
// reads as an empty result is the failure this whole layer exists to prevent.
func (c *Client) CallTool(ctx context.Context, name string, arguments map[string]any) (map[string]any, error) {
	if arguments == nil {
		arguments = map[string]any{}
	}

	raw, err := c.do(ctx, "tools/call", map[string]any{
		"name":      name,
		"arguments": arguments,
	}, name)
	if err != nil {
		return nil, err
	}

	var result struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, fmt.Errorf("mcp: %s returned something unreadable: %w", name, err)
	}

	text, ok := firstText(result.Content)
	if !ok {
		return nil, fmt.Errorf("mcp: %s returned no text content", name)
	}

	// Best effort: a boundary error and a tool payload are both JSON, but an
	// isError result is worth reporting even when its text is not.
	var payload map[string]any
	decoded := json.Unmarshal([]byte(text), &payload) == nil

	if result.IsError {
		return nil, &ToolError{Tool: name, Message: boundaryMessage(payload, text), Text: text}
	}
	if !decoded {
		return nil, fmt.Errorf("mcp: %s returned a text block that is not JSON: %s", name, truncate(text))
	}
	// `success` is a claim, and only the literal `true` makes it. A "false", a
	// 0 or a null is a refusal, and reading one of those as consent is how a
	// command reports a task it never created.
	if claim, present := payload["success"]; present && claim != true {
		return nil, &ToolRefusedError{Tool: name, Value: payload["error"]}
	}
	return payload, nil
}

func firstText(content []struct {
	Type string `json:"type"`
	Text string `json:"text"`
}) (string, bool) {
	for _, block := range content {
		if block.Type == "text" {
			return block.Text, true
		}
	}
	return "", false
}

// boundaryMessage pulls the human sentence out of the tool boundary's
// {"errors":[{category, message, severity}]} envelope, falling back to the raw
// text when the shape is not that.
func boundaryMessage(payload map[string]any, text string) string {
	entries, ok := payload["errors"].([]any)
	if !ok || len(entries) == 0 {
		return truncate(text)
	}

	messages := make([]string, 0, len(entries))
	for _, entry := range entries {
		if fields, ok := entry.(map[string]any); ok {
			if message, ok := fields["message"].(string); ok && message != "" {
				messages = append(messages, message)
			}
		}
	}
	if len(messages) == 0 {
		return truncate(text)
	}
	return strings.Join(messages, "; ")
}

func truncate(s string) string {
	if len(s) <= maxErrorBody {
		return s
	}
	return s[:maxErrorBody] + "…"
}

// requestMeta is the `_meta` every 2026-07-28 request has to carry.
//
// It is not decoration and it is not optional: MCP:RequestParams and its
// specialisations declare `required: [_meta]`, and MCP:RequestMetaObject
// requires both keys below, so GenMCP.Validator refuses a request without them
// before any handler sees it. The protocol version is also compared against the
// MCP-Protocol-Version header, and a disagreement is a 400 — which is why both
// read from the same constant rather than from two written-out strings.
//
// The capabilities object is empty because the connector has none of them. It
// runs one command and exits: it serves no roots, answers no elicitation, and
// hosts no model to sample from. Claiming otherwise would be a promise the
// server could hold it to.
func requestMeta() map[string]any {
	return map[string]any{
		"io.modelcontextprotocol/clientCapabilities": map[string]any{},
		"io.modelcontextprotocol/protocolVersion":    ProtocolVersion,
	}
}

// do posts one JSON-RPC request and returns its `result`.
//
// params is the method's own params, to which `do` adds the `_meta` the
// protocol requires — every request carries one, so no call site has to
// remember. name is the value Mcp-Name must carry; it is empty for every method
// that has no name to mirror. The server ignores a stray Mcp-Name, but sending
// one we do not mean would be a claim about the body that is not true.
func (c *Client) do(ctx context.Context, method string, params map[string]any, name string) (json.RawMessage, error) {
	if c.err != nil {
		return nil, c.err
	}

	// The default is a floor under a caller who set no deadline, not a ceiling
	// over one who did: a caller waiting on a long tool call has said how long
	// it may take, and this must not overrule them.
	if _, bounded := ctx.Deadline(); !bounded && c.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.timeout)
		defer cancel()
	}

	// A copy, so a caller's map is never mutated under it.
	withMeta := make(map[string]any, len(params)+1)
	for key, value := range params {
		withMeta[key] = value
	}
	withMeta["_meta"] = requestMeta()

	id := c.nextID.Add(1)
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  method,
		"params":  withMeta,
	})
	if err != nil {
		return nil, fmt.Errorf("mcp: encoding the %s request: %w", method, err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("mcp: building the %s request: %w", method, err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("MCP-Protocol-Version", ProtocolVersion)
	req.Header.Set("Mcp-Method", method)
	if name != "" {
		req.Header.Set("Mcp-Name", name)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("mcp: %s: %w", method, err)
	}
	defer resp.Body.Close()

	// A login problem outranks the shape it arrived in: whatever the body says,
	// the remedy for a 401 is a login.
	if resp.StatusCode == http.StatusUnauthorized {
		return nil, ErrUnauthorized
	}

	mediaType, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))

		// GenMCP answers a message it refuses — a protocol version it does not
		// serve, a header disagreeing with the body — with a JSON-RPC error
		// under a 4xx. The sentence in it is the one that says what to fix, so
		// it must not be flattened into a status code.
		if mediaType == "application/json" {
			var response envelope
			if json.Unmarshal(snippet, &response) == nil && response.Error != nil {
				return nil, response.Error
			}
		}

		return nil, &HTTPError{
			StatusCode: resp.StatusCode,
			Status:     resp.Status,
			Body:       strings.TrimSpace(string(snippet)),
		}
	}

	if mediaType == "text/event-stream" {
		return readStream(resp.Body, id, method)
	}
	return readEnvelope(resp.Body, method)
}

// envelope is a JSON-RPC response: exactly one of result and error.
type envelope struct {
	ID     json.RawMessage `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *RPCError       `json:"error"`
}

func (e envelope) unwrap(method string) (json.RawMessage, error) {
	if e.Error != nil {
		return nil, e.Error
	}
	if len(e.Result) == 0 {
		return nil, fmt.Errorf("mcp: the %s response carried neither a result nor an error", method)
	}
	return e.Result, nil
}

func readEnvelope(body io.Reader, method string) (json.RawMessage, error) {
	var response envelope
	if err := json.NewDecoder(body).Decode(&response); err != nil {
		return nil, fmt.Errorf("mcp: reading the %s response: %w", method, err)
	}
	return response.unwrap(method)
}

// readStream reads an SSE body and returns the first frame answering our id.
// The stream can also carry server-initiated messages, which are somebody
// else's conversation and are stepped over.
func readStream(body io.Reader, id int64, method string) (json.RawMessage, error) {
	want := []byte(fmt.Sprint(id))

	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64<<10), maxFrameSize)

	var data []string
	for scanner.Scan() {
		line := scanner.Text()

		if payload, found := strings.CutPrefix(line, "data:"); found {
			data = append(data, strings.TrimPrefix(payload, " "))
			continue
		}
		if line != "" {
			continue // event:, id:, retry:, a comment — none of them ours
		}

		result, err, matched := matchFrame(strings.Join(data, "\n"), want, method)
		data = nil
		if matched {
			return result, err
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("mcp: reading the %s stream: %w", method, err)
	}

	// A stream that ended without our answer: the last frame may not have been
	// followed by the blank line that terminates it.
	if result, err, matched := matchFrame(strings.Join(data, "\n"), want, method); matched {
		return result, err
	}
	return nil, fmt.Errorf("mcp: the %s stream ended without an answer", method)
}

// matchFrame decides whether one SSE frame is the answer to our request, and
// unwraps it if so.
//
// The id is compared as raw JSON bytes, which is exact rather than lenient on
// purpose: this client only ever mints numeric ids (`c.nextID`, an int64
// rendered by fmt), and JSON-RPC requires a response to echo the request's id
// back unchanged. So the only thing that can equal the id we sent is the same
// digits. A frame carrying "1" or 1.0 is not our answer and is stepped over
// with everything else that is somebody else's conversation.
func matchFrame(frame string, wantID []byte, method string) (json.RawMessage, error, bool) {
	if strings.TrimSpace(frame) == "" {
		return nil, nil, false
	}

	var response envelope
	if err := json.Unmarshal([]byte(frame), &response); err != nil {
		return nil, nil, false
	}
	if !bytes.Equal(bytes.TrimSpace(response.ID), wantID) {
		return nil, nil, false
	}
	if response.Error == nil && len(response.Result) == 0 {
		return nil, nil, false
	}

	result, err := response.unwrap(method)
	return result, err, true
}
