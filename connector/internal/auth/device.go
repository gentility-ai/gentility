// Package auth signs the connector in to a Gentility MCP host, and out again.
//
// There is no password to type and no API key to paste. The host runs RFC
// 8628's device flow: the connector asks for a code, the person approves it in
// a browser they are already logged in to, and the connector polls until the
// answer changes. What it gets back is the same opaque MCP session bearer the
// web app mints — 30 days, revocable, carrying no authorization snapshot of
// its own, because the organization-scope contract makes every call name its
// organization instead.
//
// Two facts about the server shape this code and are worth stating plainly,
// because both are places a generic OAuth client would go wrong:
//
//   - The token endpoint answers HTTP 400 with an `error` word for every
//     not-yet state — authorization_pending, access_denied, expired_token,
//     invalid_grant. A 400 here is a status report, not a failure.
//   - It does not send slow_down today, so the interval it names is the
//     interval. The arm is here anyway: it costs five lines, and RFC 8628
//     lets any server start sending one.
//
// The other thing that shapes the poll loop is what is NOT an answer. A round
// trip that never reached the host — wifi dropping, a proxy blinking, a dev
// server restarting mid-approval — says nothing about the authorization, and
// the person is still standing at the browser with the code in their hand. So
// only an OAuth answer and the device code's own deadline end the wait; a
// transport failure is remembered and retried until one of those two arrives.
//
// The bearer is written through internal/state, which owns the 0600 file. It
// is never printed: what a person needs to see is the code to approve and the
// host it is for.
package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gentility-ai/gentility/connector/internal/mcp"
	"github.com/gentility-ai/gentility/connector/internal/state"
	"github.com/gentility-ai/gentility/connector/internal/terminal"
)

const (
	// ClientID is how the connector names itself to the device endpoint. The
	// server requires a client_id and records it against the authorization;
	// it is a name in an audit trail, not a secret.
	ClientID = "gentility-connector"

	// GrantType is RFC 8628's device grant, spelled the way the token endpoint
	// matches on it.
	GrantType = "urn:ietf:params:oauth:grant-type:device_code"

	// DefaultBaseURL is the production MCP host, used when nothing else says
	// otherwise.
	DefaultBaseURL = "https://mcp.gentility.ai"

	// EnvBaseURL points the connector at another host — a worktree's
	// $WT_MCP_URL, a staging deployment.
	EnvBaseURL = "GENTILITY_MCP_URL"

	// devicePath and tokenPath hang off the MCP origin, which is why the
	// origin is the only thing worth storing.
	devicePath = "/oauth/device/code"
	tokenPath  = "/oauth/token"

	// requestTimeout bounds one HTTP request. It does not bound the flow: the
	// flow is bounded by the device code's own lifetime, which the server sets
	// and which can legitimately be fifteen minutes of waiting.
	requestTimeout = 30 * time.Second

	// maxBody caps how much of an OAuth answer is read. One is a few hundred
	// bytes; anything larger is a proxy's error page.
	maxBody = 8 << 10

	// maxSnippet caps how much of an unreadable answer is quoted back. Enough
	// to recognise an nginx page or an exception; not enough to scroll.
	maxSnippet = 160

	// expiryLayout renders the token's last day. A date is what a person can
	// act on; a Unix timestamp is not.
	expiryLayout = "2 January 2006"

	// fallbackLifetime and fallbackInterval stand in for a server that omits
	// them. They are the values the device authorization endpoint sends.
	fallbackLifetime = 900 * time.Second
	fallbackInterval = 5 * time.Second

	// slowDownStep is what RFC 8628 section 3.5 adds to the interval when the
	// server answers slow_down.
	slowDownStep = 5 * time.Second

	// pollFloor is the shortest wait between two polls. An interval of zero —
	// sent by a server, or produced by arithmetic — must not turn this loop
	// into a spin against somebody's MCP host.
	pollFloor = time.Second
)

// ErrAccessDenied reports that a person saw the request and said no. Retrying
// is not the remedy, so it is a distinct error rather than another failure.
var ErrAccessDenied = errors.New("the login was refused in the browser")

// ErrExpired reports that the device code ran out — either because the server
// said expired_token, or because its own lifetime elapsed while we waited.
var ErrExpired = errors.New("the device code expired before it was approved")

// ErrNoServerEnrollment reports that a `login --server` was approved as a
// person rather than as a server, so the token carried no enrollment. The
// bearer is real and the approval happened; what did not happen is the thing
// that was asked for, and the remedy is a button in the browser.
var ErrNoServerEnrollment = errors.New(
	"the approval named no server — choose 'This is a server for …' in the browser")

// transportError is a request that never reached the host: DNS, TCP, TLS, a
// connection dropped mid-body. It is wrapped rather than returned bare because
// the poll loop has to tell "the host said no" from "we never asked it", and
// only the first of those is a verdict.
type transportError struct{ err error }

func (e *transportError) Error() string { return e.err.Error() }
func (e *transportError) Unwrap() error { return e.err }

// isTransport reports whether err is a request that never reached the host.
func isTransport(err error) bool {
	var t *transportError
	return errors.As(err, &t)
}

// Origin is the MCP host a command talks to, and where that choice came from.
// The source is carried because a connector silently pointed at a worktree, or
// silently pointed at production, is the same surprise in two directions.
type Origin struct {
	URL    string // scheme and host, normalised
	Source string // "--url", "$GENTILITY_MCP_URL", "the last login", "the default"
}

// ResolveOrigin decides which MCP host to use: the flag, then the environment,
// then the host the last login was against, then production.
func ResolveOrigin(flagURL string, store *state.Store) (Origin, error) {
	raw, source := DefaultBaseURL, "the default"

	switch {
	case flagURL != "":
		raw, source = flagURL, "--url"
	case os.Getenv(EnvBaseURL) != "":
		raw, source = os.Getenv(EnvBaseURL), "$"+EnvBaseURL
	default:
		if stored, err := store.LoadAuth(); err == nil && stored.BaseURL != "" {
			raw, source = stored.BaseURL, "the last login"
		}
	}

	origin, err := mcp.NormalizeBaseURL(raw)
	if err != nil {
		return Origin{}, fmt.Errorf("the host from %s is unusable: %w", source, err)
	}
	return Origin{URL: origin, Source: source}, nil
}

// Options are the seams a test needs and a person never sets.
type Options struct {
	// Interval overrides the wait between polls. Zero means the interval the
	// server named.
	Interval time.Duration

	// SlowDownStep is how much a slow_down adds to the interval. Zero means
	// RFC 8628 section 3.5's five seconds.
	SlowDownStep time.Duration

	// Now is the clock the device code's lifetime is measured against. Zero
	// means time.Now.
	Now func() time.Time

	// Client is the HTTP client. Zero means one bounded by requestTimeout.
	Client *http.Client

	// MachineLabel is a display claim, never an existing machine identity.
	// Empty uses this machine's hostname when it is available.
	MachineLabel string

	// Server says this login is enrolling a server rather than a person. The
	// server never learns about it: the device request and the polls are
	// identical either way, and what names a server is which approval the
	// person chooses in the browser. This is purely what the connector
	// expects back — the guidance it prints, and its refusal to call a login
	// that carried no enrollment a success.
	Server bool
}

// MachineLabelFor is the label a person's machine joins 6net under by default:
// the hostname's first label, because the server slugs what it is sent and
// `willow.local` would fold to `willowlocal`; empty when the hostname has no
// first label.
func MachineLabelFor(hostname string) string {
	label, _, _ := strings.Cut(hostname, ".")
	return strings.TrimSpace(label)
}

func (o Options) withDefaults() Options {
	if o.SlowDownStep == 0 {
		o.SlowDownStep = slowDownStep
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Client == nil {
		o.Client = &http.Client{Timeout: requestTimeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	return o
}

// Login completes the device flow against origin and stores the bearer.
//
// It prints the code and the URL before it starts waiting, because that
// instruction is the entire user interface: a person staring at a silent
// terminal has nothing to approve.
func Login(ctx context.Context, origin Origin, store *state.Store, out io.Writer, opts Options) error {
	// Normalise here as well as in ResolveOrigin: this is the function that
	// must guarantee the stored base URL and the MCP client's endpoint are
	// derived from the same origin, whoever called it.
	base, err := mcp.NormalizeBaseURL(origin.URL)
	if err != nil {
		return err
	}
	opts = opts.withDefaults()

	label := opts.MachineLabel
	if label == "" {
		if hostname, err := os.Hostname(); err == nil {
			label = MachineLabelFor(hostname)
		}
	}
	code, err := requestDeviceCode(ctx, opts.Client, base, label)
	if err != nil {
		return err
	}

	fmt.Fprintf(out, "Host  %s  (from %s)\n\n", terminal.Text(base), terminal.Text(origin.Source))
	fmt.Fprintf(out, "Open  %s\n", terminal.Text(absolute(base, code.VerificationURI)))
	fmt.Fprintf(out, "Code  %s\n", terminal.Text(code.UserCode))
	if code.VerificationURIComplete != "" && !opts.Server {
		fmt.Fprintf(out, "Or    %s\n", terminal.Text(absolute(base, code.VerificationURIComplete)))
	}
	if opts.Server {
		fmt.Fprint(out, "\nApprove it in the browser and choose \"This is a server for <organization>\".\n")
	}
	fmt.Fprint(out, "\nWaiting for approval…\n")

	interval := opts.Interval
	if interval == 0 {
		interval = code.interval()
	}
	deadline := opts.Now().Add(code.lifetime())

	// unreachable remembers the last round trip that never arrived, so a flow
	// that runs out of time because the host was down says so, instead of
	// reporting a bare expiry the person cannot act on.
	var unreachable error

	for {
		token, err := pollToken(ctx, opts.Client, base, code.DeviceCode)

		switch {
		case isTransport(err):
			unreachable = err
		case err != nil:
			return err
		default:
			unreachable = nil

			switch token.Error {
			case "":
				// Refused before anything is written: a half-done server
				// enrolment — a bearer here, no enrollment beside it — is a
				// state `serve` cannot use and nobody asked for.
				if opts.Server && token.Enrollment == nil {
					return ErrNoServerEnrollment
				}
				return save(store, out, base, token, opts.Now(), opts.Server)
			case "authorization_pending":
				// The person has not answered yet. This is the expected case.
			case "slow_down":
				interval += opts.SlowDownStep
			case "access_denied":
				return ErrAccessDenied
			case "expired_token":
				return ErrExpired
			default:
				return fmt.Errorf("the host refused the login (%s): %s", token.Error, token.ErrorDescription)
			}
		}

		if !opts.Now().Before(deadline) {
			if unreachable != nil {
				return fmt.Errorf("%w, and the host was unreachable: %w", ErrExpired, unreachable)
			}
			return ErrExpired
		}
		if err := wait(ctx, interval); err != nil {
			return err
		}
	}
}

// absolute resolves a verification URI against the origin that sent it.
//
// RFC 8628 calls for absolute URIs and Gentility sends them, but a host that
// answers with a path is still saying where to go — and printing "/device" at
// a person is not.
func absolute(base, raw string) string {
	ref, err := url.Parse(raw)
	if err != nil || ref.IsAbs() {
		return raw
	}
	origin, err := url.Parse(base)
	if err != nil {
		return raw
	}
	return origin.ResolveReference(ref).String()
}

// Logout revokes the MCP session before forgetting its bearer and preferences.
// Repeating it with no stored login is harmless. An unconfirmed revocation
// leaves local state intact so the person can retry.
func Logout(store *state.Store, out io.Writer) error {
	if err := Revoke(context.Background(), store); err != nil {
		return fmt.Errorf("%w. To forget only this machine's login without server confirmation, delete %q; the server session may remain live until it expires", err, filepath.Join(store.Dir, "auth.json"))
	}

	if err := store.ClearAuth(); err != nil {
		return err
	}
	// The remembered organization is part of the session, not of the laptop.
	if err := store.ClearPrefs(); err != nil {
		return err
	}

	fmt.Fprintln(out, "Signed out.")
	return nil
}

// deviceCode is the device endpoint's answer.
type deviceCode struct {
	DeviceCode              string `json:"device_code"`
	UserCode                string `json:"user_code"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete"`
	ExpiresIn               int64  `json:"expires_in"`
	Interval                int64  `json:"interval"`
}

func (d deviceCode) lifetime() time.Duration {
	if d.ExpiresIn <= 0 {
		return fallbackLifetime
	}
	return time.Duration(d.ExpiresIn) * time.Second
}

func (d deviceCode) interval() time.Duration {
	if d.Interval <= 0 {
		return fallbackInterval
	}
	return time.Duration(d.Interval) * time.Second
}

// tokenResponse is the token endpoint's answer in both its shapes: the grant,
// and the OAuth error word that says why not yet.
type tokenResponse struct {
	AccessToken      string `json:"access_token"`
	TokenType        string `json:"token_type"`
	ExpiresIn        int64  `json:"expires_in"`
	Scope            string `json:"scope"`
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`

	// Enrollment is a server's join material, and it rides back here exactly
	// once: the server merges it into the token response for an approval that
	// named a server, and never sends it again. (It would also send a person's
	// to a poll carrying a machine label; no poll carries one, because a
	// person's machine fetches its material when it joins.) A pointer, because
	// "no enrollment" is the ordinary shape of a plain login and must not
	// decode into a zero enrollment that then gets stored.
	Enrollment *state.Enrollment `json:"enrollment,omitempty"`
}

func requestDeviceCode(ctx context.Context, client *http.Client, base, label string) (deviceCode, error) {
	// No scope: the server's default of "agent:connect agent:execute" is the
	// one the connector wants, and naming it here would be a second place to
	// keep in step with the first.
	status, body, err := postForm(ctx, client, base+devicePath, url.Values{"client_id": {ClientID}, "machine_label": {label}})
	if err != nil {
		return deviceCode{}, err
	}
	if status != http.StatusOK {
		return deviceCode{}, fmt.Errorf("%s answered %d: %s", base+devicePath, status, snippet(body))
	}

	var code deviceCode
	if err := json.Unmarshal(body, &code); err != nil {
		return deviceCode{}, fmt.Errorf("%s answered with something that is not JSON: %s", base+devicePath, snippet(body))
	}
	if code.DeviceCode == "" || code.UserCode == "" {
		return deviceCode{}, fmt.Errorf("%s returned no device code", base+devicePath)
	}
	return code, nil
}

// pollToken asks once. An OAuth error word is a result, not a failure: it is
// how the server reports "not yet", "denied" and "too late".
//
// The client_id travels with every poll. RFC 8628 §3.4 asks the device for it,
// and Gentility holds the poll to the client the authorization was issued to:
// omitting it is refused with invalid_request, and presenting somebody else's
// with invalid_grant.
//
// No machine_label is sent. With one the server would mint a person's join
// material here, whose pre-auth key dies ten minutes later; a person's machine
// fetches its material when it joins instead (FetchEnrollment).
func pollToken(ctx context.Context, client *http.Client, base, deviceCode string) (tokenResponse, error) {
	form := url.Values{
		"grant_type":  {GrantType},
		"device_code": {deviceCode},
		"client_id":   {ClientID},
	}

	status, body, err := postForm(ctx, client, base+tokenPath, form)
	if err != nil {
		return tokenResponse{}, err
	}

	var token tokenResponse
	decoded := json.Unmarshal(body, &token) == nil

	switch {
	case status == http.StatusOK && token.AccessToken != "":
		return token, nil
	case status == http.StatusOK:
		return tokenResponse{}, fmt.Errorf("%s answered 200 without an access token", base+tokenPath)
	case decoded && token.Error != "":
		return token, nil
	default:
		return tokenResponse{}, fmt.Errorf("%s answered %d: %s", base+tokenPath, status, snippet(body))
	}
}

func save(store *state.Store, out io.Writer, base string, token tokenResponse, now time.Time, server bool) error {
	stored := state.Auth{BaseURL: base, AccessToken: token.AccessToken}
	if token.ExpiresIn > 0 {
		stored.ExpiresAt = now.Add(time.Duration(token.ExpiresIn) * time.Second).Unix()
	}

	if err := store.SaveAuth(stored); err != nil {
		return fmt.Errorf("storing the token: %w", err)
	}

	// A server's material exists only here: the token response carries it
	// once, and a login that dropped it would leave a server that can never
	// join. A person's login asked for none, so any that arrives is ignored
	// rather than left lying in a home directory.
	if server {
		token.Enrollment.Origin = base
		if err := store.SaveEnrollment(*token.Enrollment); err != nil {
			return fmt.Errorf("storing the enrollment: %w", err)
		}
	}

	if stored.ExpiresAt > 0 {
		fmt.Fprintf(out, "Signed in. This token expires on %s.\n", time.Unix(stored.ExpiresAt, 0).Format(expiryLayout))
	} else {
		fmt.Fprintln(out, "Signed in.")
	}
	if server {
		fmt.Fprintln(out, "This machine is enrolled as a server. Run `connector serve` to bring it up.")
	}
	return nil
}

// postForm posts an urlencoded body and reads back a capped answer. The
// :mcp_oauth pipeline parses :urlencoded, so a standard OAuth form body is
// what both endpoints expect.
func postForm(ctx context.Context, client *http.Client, endpoint string, form url.Values) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return 0, nil, fmt.Errorf("building the request to %s: %w", endpoint, err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	return send(client, req, endpoint, maxBody)
}

// ErrAnswerTooLarge is an answer longer than its endpoint ever sends: a
// proxy's page, or something that is not the endpoint at all.
var ErrAnswerTooLarge = errors.New("the answer is too large")

// send makes one request and reads back an answer of at most limit bytes. A
// failure to reach the host is a transportError, which is not the host's
// verdict; an answer over limit is refused whole rather than cut short, since
// a truncated answer would only fail later as one of the wrong shape.
func send(client *http.Client, req *http.Request, endpoint string, limit int64) (int, []byte, error) {
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, &transportError{fmt.Errorf("never reached %s: %w", endpoint, err)}
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return 0, nil, &transportError{fmt.Errorf("reading the answer from %s: %w", endpoint, err)}
	}
	if int64(len(body)) > limit {
		return 0, nil, fmt.Errorf("%s answered %d with more than %d bytes: %w", endpoint, resp.StatusCode, limit, ErrAnswerTooLarge)
	}
	return resp.StatusCode, body, nil
}

func wait(ctx context.Context, d time.Duration) error {
	// A clamp, not a guard: returning early on a non-positive interval is how
	// the poll loop becomes a spin.
	if d <= 0 {
		d = pollFloor
	}

	timer := time.NewTimer(d)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// snippet quotes an answer back at a person in one short line.
//
// What arrives when this endpoint goes wrong is rarely a sentence: it is a
// Phoenix exception page or a proxy's HTML, kilobytes across hundreds of
// lines. Pasted whole it buries the status under a screenful of stylesheet, so
// collapse it to a line and keep only the head of it — enough to recognise
// what answered.
func snippet(body []byte) string {
	line := strings.Join(strings.Fields(string(body)), " ")
	if len(line) > maxSnippet {
		return strings.TrimSpace(line[:maxSnippet]) + "…"
	}
	return line
}
