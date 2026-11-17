package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gentility-ai/gentility/connector/internal/state"
)

// fixedNow is the clock every test runs on, so a printed expiry date is a fact
// about the code rather than about the day the suite ran.
var fixedNow = time.Date(2026, time.August, 29, 12, 0, 0, 0, time.UTC)

// instantly stands in for the server's five-second interval. Login's waits are
// real; making them microseconds is what keeps the suite from sleeping.
const instantly = time.Microsecond

func testOptions() Options {
	return Options{Interval: instantly, Now: func() time.Time { return fixedNow }}
}

func writeJSON(w http.ResponseWriter, status int, body map[string]any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(body)
}

// recorder collects what the server saw under a lock, so the assertions and
// the handler goroutines do not race.
type recorder struct {
	mu          sync.Mutex
	deviceForm  url.Values
	deviceCT    string
	tokenForms  []url.Values
	tokenCTs    []string
	tokenAccept []string
}

func (r *recorder) device(req *http.Request) {
	req.ParseForm()
	r.mu.Lock()
	defer r.mu.Unlock()
	r.deviceForm = req.PostForm
	r.deviceCT = req.Header.Get("Content-Type")
}

func (r *recorder) token(req *http.Request) int {
	req.ParseForm()
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tokenForms = append(r.tokenForms, req.PostForm)
	r.tokenCTs = append(r.tokenCTs, req.Header.Get("Content-Type"))
	r.tokenAccept = append(r.tokenAccept, req.Header.Get("Accept"))
	return len(r.tokenForms)
}

func (r *recorder) polls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.tokenForms)
}

func (r *recorder) seen() (url.Values, string, []url.Values) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.deviceForm, r.deviceCT, append([]url.Values(nil), r.tokenForms...)
}

// deviceCodeBody is what MCPAuthController.oauth_device_code returns.
func deviceCodeBody() map[string]any {
	return map[string]any{
		"device_code":               "dev-1",
		"user_code":                 "ABCD-EFGH",
		"verification_uri":          "https://mcp.example/device",
		"verification_uri_complete": "https://mcp.example/device?user_code=ABCD-EFGH",
		"expires_in":                900,
		"interval":                  5,
	}
}

// flowServer serves the two device-flow endpoints. token decides what the
// token endpoint answers on each poll, counting from 1.
func flowServer(t *testing.T, token func(poll int, w http.ResponseWriter)) (*httptest.Server, *recorder) {
	t.Helper()
	rec := &recorder{}

	mux := http.NewServeMux()
	mux.HandleFunc("/oauth/device/code", func(w http.ResponseWriter, r *http.Request) {
		rec.device(r)
		writeJSON(w, 200, deviceCodeBody())
	})
	mux.HandleFunc("/oauth/token", func(w http.ResponseWriter, r *http.Request) {
		token(rec.token(r), w)
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, rec
}

func store(t *testing.T) *state.Store {
	t.Helper()
	return &state.Store{Dir: t.TempDir()}
}

func TestLoginPollsUntilApprovalAndStoresTheBearer(t *testing.T) {
	srv, rec := flowServer(t, func(poll int, w http.ResponseWriter) {
		if poll < 3 {
			writeJSON(w, 400, map[string]any{
				"error":             "authorization_pending",
				"error_description": "User has not yet authorized the device",
			})
			return
		}
		writeJSON(w, 200, map[string]any{
			"access_token": "tok-xyz",
			"token_type":   "Bearer",
			"expires_in":   2592000,
			"scope":        "agent:connect agent:execute",
		})
	})

	st := store(t)
	var out strings.Builder

	if err := Login(context.Background(), Origin{URL: srv.URL, Source: "--url"}, st, &out, testOptions()); err != nil {
		t.Fatalf("Login: %v", err)
	}

	if got := rec.polls(); got != 3 {
		t.Fatalf("polled %d times, want 3 — authorization_pending must keep waiting", got)
	}

	deviceForm, deviceCT, tokenForms := rec.seen()
	if got := deviceForm.Get("client_id"); got != ClientID {
		t.Fatalf("client_id = %q, want %q — the server requires one", got, ClientID)
	}
	if _, sent := deviceForm["scope"]; sent {
		t.Fatalf("scope = %v, want it omitted so the server's default applies", deviceForm["scope"])
	}
	if !strings.HasPrefix(deviceCT, "application/x-www-form-urlencoded") {
		t.Fatalf("device Content-Type = %q, want a form body", deviceCT)
	}
	for i, form := range tokenForms {
		if got := form.Get("grant_type"); got != GrantType {
			t.Fatalf("poll %d grant_type = %q, want %q", i+1, got, GrantType)
		}
		if got := form.Get("device_code"); got != "dev-1" {
			t.Fatalf("poll %d device_code = %q, want the one the server minted", i+1, got)
		}
		// RFC 8628 §3.4: the device repeats its own client_id at the token
		// endpoint. Gentility holds the poll to it — the authorization was
		// issued to this client and to no other — so a poll that omits it is
		// refused with invalid_request before the device_code is even read.
		if got := form.Get("client_id"); got != ClientID {
			t.Fatalf("poll %d client_id = %q, want %q — the device repeats its own (RFC 8628 §3.4)", i+1, got, ClientID)
		}
	}

	auth, err := st.LoadAuth()
	if err != nil {
		t.Fatalf("LoadAuth: %v", err)
	}
	if auth.AccessToken != "tok-xyz" {
		t.Fatalf("AccessToken = %q", auth.AccessToken)
	}
	if want := fixedNow.Add(2592000 * time.Second).Unix(); auth.ExpiresAt != want {
		t.Fatalf("ExpiresAt = %d, want %d — an absolute time computed from expires_in", auth.ExpiresAt, want)
	}
}

// The stored base URL is what every later tools/call will use, so it must be
// the origin the MCP client would derive from the same string — never a path,
// never userinfo. Normalising once is what stops the device flow and the MCP
// client disagreeing about which host they are on.
func TestLoginStoresTheOriginWithoutItsPathOrUserinfo(t *testing.T) {
	srv, _ := flowServer(t, func(poll int, w http.ResponseWriter) {
		writeJSON(w, 200, map[string]any{"access_token": "tok", "token_type": "Bearer", "expires_in": 60})
	})

	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	origin := u.String()
	u.User = url.UserPassword("someone", "secret")
	u.Path = "/mcp"
	u.RawQuery = "x=1"

	st := store(t)
	var out strings.Builder

	if err := Login(context.Background(), Origin{URL: u.String(), Source: "--url"}, st, &out, testOptions()); err != nil {
		t.Fatalf("Login: %v", err)
	}

	auth, err := st.LoadAuth()
	if err != nil {
		t.Fatalf("LoadAuth: %v", err)
	}
	if auth.BaseURL != origin {
		t.Fatalf("BaseURL = %q, want the normalised origin %q", auth.BaseURL, origin)
	}
	if strings.Contains(out.String(), "secret") {
		t.Fatalf("the printed host leaked userinfo:\n%s", out.String())
	}
}

func TestLoginPrintsTheCodeAndTheURLBeforePolling(t *testing.T) {
	var printedBeforeFirstPoll string

	srv, _ := flowServer(t, func(poll int, w http.ResponseWriter) {
		writeJSON(w, 200, map[string]any{"access_token": "tok-xyz", "token_type": "Bearer", "expires_in": 2592000})
	})

	st := store(t)
	var out strings.Builder

	// The instruction must be on screen before the wait starts, or the user is
	// watching a silent terminal with nothing to act on.
	watcher := &watchWriter{under: &out, on: func(s string) {
		if printedBeforeFirstPoll == "" && strings.Contains(s, "Waiting") {
			printedBeforeFirstPoll = out.String()
		}
	}}

	if err := Login(context.Background(), Origin{URL: srv.URL, Source: "--url"}, st, watcher, testOptions()); err != nil {
		t.Fatalf("Login: %v", err)
	}

	expiry := time.Unix(fixedNow.Add(2592000*time.Second).Unix(), 0).Format(expiryLayout)
	want := fmt.Sprintf(`Host  %s  (from --url)

Open  https://mcp.example/device
Code  ABCD-EFGH
Or    https://mcp.example/device?user_code=ABCD-EFGH

Waiting for approval…
Signed in. This token expires on %s.
`, srv.URL, expiry)

	if out.String() != want {
		t.Fatalf("login printed:\n%s\nwant:\n%s", out.String(), want)
	}
	if !strings.Contains(printedBeforeFirstPoll, "ABCD-EFGH") {
		t.Fatalf("the code was not on screen before the wait began:\n%s", printedBeforeFirstPoll)
	}
}

// watchWriter calls on with every write, so a test can see what had been
// printed at a given moment.
type watchWriter struct {
	under *strings.Builder
	on    func(string)
}

func (w *watchWriter) Write(p []byte) (int, error) {
	n, err := w.under.Write(p)
	w.on(string(p))
	return n, err
}

func TestLoginNeverPrintsTheBearer(t *testing.T) {
	srv, _ := flowServer(t, func(poll int, w http.ResponseWriter) {
		writeJSON(w, 200, map[string]any{"access_token": "sup3r-s3cr3t", "token_type": "Bearer", "expires_in": 60})
	})

	var out strings.Builder
	if err := Login(context.Background(), Origin{URL: srv.URL, Source: "--url"}, store(t), &out, testOptions()); err != nil {
		t.Fatalf("Login: %v", err)
	}
	if strings.Contains(out.String(), "sup3r-s3cr3t") {
		t.Fatalf("the bearer reached the terminal:\n%s", out.String())
	}
}

func TestLoginStopsOnAccessDenied(t *testing.T) {
	srv, rec := flowServer(t, func(poll int, w http.ResponseWriter) {
		writeJSON(w, 400, map[string]any{"error": "access_denied", "error_description": "User denied authorization"})
	})

	st := store(t)
	err := Login(context.Background(), Origin{URL: srv.URL, Source: "--url"}, st, io.Discard, testOptions())

	if !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("err = %v, want ErrAccessDenied", err)
	}
	if got := rec.polls(); got != 1 {
		t.Fatalf("polled %d times, want 1 — a refusal is final", got)
	}
	if _, err := st.LoadAuth(); !state.IsNotLoggedIn(err) {
		t.Fatalf("a denied login stored something: %v", err)
	}
}

func TestLoginStopsOnExpiredToken(t *testing.T) {
	srv, rec := flowServer(t, func(poll int, w http.ResponseWriter) {
		writeJSON(w, 400, map[string]any{"error": "expired_token", "error_description": "Device code has expired"})
	})

	err := Login(context.Background(), Origin{URL: srv.URL, Source: "--url"}, store(t), io.Discard, testOptions())

	if !errors.Is(err, ErrExpired) {
		t.Fatalf("err = %v, want ErrExpired", err)
	}
	if got := rec.polls(); got != 1 {
		t.Fatalf("polled %d times, want 1", got)
	}
}

func TestLoginStopsOnInvalidGrant(t *testing.T) {
	srv, _ := flowServer(t, func(poll int, w http.ResponseWriter) {
		writeJSON(w, 400, map[string]any{"error": "invalid_grant", "error_description": "Invalid device code"})
	})

	err := Login(context.Background(), Origin{URL: srv.URL, Source: "--url"}, store(t), io.Discard, testOptions())
	if err == nil {
		t.Fatal("invalid_grant was not reported")
	}
	if errors.Is(err, ErrAccessDenied) || errors.Is(err, ErrExpired) {
		t.Fatalf("err = %v — invalid_grant is neither a refusal nor an expiry", err)
	}
	if !strings.Contains(err.Error(), "invalid_grant") {
		t.Fatalf("err = %v, want the server's word in it", err)
	}
}

func TestLoginGivesUpWhenTheDeviceCodeLifetimeElapses(t *testing.T) {
	// The server can stay pending forever; the code cannot. expires_in is 900,
	// and this clock jumps 600 seconds a reading.
	srv, rec := flowServer(t, func(poll int, w http.ResponseWriter) {
		writeJSON(w, 400, map[string]any{"error": "authorization_pending"})
	})

	var mu sync.Mutex
	tick := 0
	clock := func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		at := fixedNow.Add(time.Duration(tick) * 600 * time.Second)
		tick++
		return at
	}

	err := Login(context.Background(), Origin{URL: srv.URL, Source: "--url"}, store(t), io.Discard,
		Options{Interval: instantly, Now: clock})

	if !errors.Is(err, ErrExpired) {
		t.Fatalf("err = %v, want ErrExpired once the code's lifetime is past", err)
	}
	if got := rec.polls(); got == 0 {
		t.Fatal("gave up without polling at all")
	}
}

func TestLoginStopsWhenTheDeviceEndpointFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 500, map[string]any{"error": "server_error", "error_description": "Failed to generate device code"})
	}))
	defer srv.Close()

	st := store(t)
	err := Login(context.Background(), Origin{URL: srv.URL, Source: "--url"}, st, io.Discard, testOptions())
	if err == nil {
		t.Fatal("a 500 from the device endpoint was not reported")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Fatalf("err = %v, want the status in it", err)
	}
	if _, err := st.LoadAuth(); !state.IsNotLoggedIn(err) {
		t.Fatalf("a failed login stored something: %v", err)
	}
}

func TestLoginStopsWhenTheTokenEndpointFails(t *testing.T) {
	srv, _ := flowServer(t, func(poll int, w http.ResponseWriter) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(500)
		io.WriteString(w, "<html>Internal Server Error</html>")
	})

	err := Login(context.Background(), Origin{URL: srv.URL, Source: "--url"}, store(t), io.Discard, testOptions())
	if err == nil {
		t.Fatal("a 500 from the token endpoint was not reported")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Fatalf("err = %v, want the status in it", err)
	}
}

func TestLoginRefusesAnUnusableHost(t *testing.T) {
	if err := Login(context.Background(), Origin{URL: "mcp.example.com", Source: "--url"},
		store(t), io.Discard, testOptions()); err == nil {
		t.Fatal("a URL with no scheme was accepted")
	}
}

func TestLoginHonoursACancelledContext(t *testing.T) {
	srv, _ := flowServer(t, func(poll int, w http.ResponseWriter) {
		writeJSON(w, 400, map[string]any{"error": "authorization_pending"})
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := Login(ctx, Origin{URL: srv.URL, Source: "--url"}, store(t), io.Discard, testOptions())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func TestLogoutClearsTheStoredBearer(t *testing.T) {
	st := store(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	defer srv.Close()
	if err := st.SaveAuth(state.Auth{BaseURL: srv.URL, AccessToken: "tok"}); err != nil {
		t.Fatal(err)
	}

	var out strings.Builder
	if err := Logout(st, &out); err != nil {
		t.Fatalf("Logout: %v", err)
	}
	if _, err := st.LoadAuth(); !state.IsNotLoggedIn(err) {
		t.Fatalf("the bearer survived logout: %v", err)
	}
	if got := out.String(); !strings.Contains(got, "Signed out.") {
		t.Fatalf("Logout printed %q", got)
	}

	// Forgetting what is already forgotten is not a failure.
	if err := Logout(st, io.Discard); err != nil {
		t.Fatalf("second Logout: %v", err)
	}
}

func TestResolveOriginPrefersTheFlagThenTheEnvironmentThenTheLastLogin(t *testing.T) {
	st := store(t)
	if err := st.SaveAuth(state.Auth{BaseURL: "https://stored.example", AccessToken: "tok"}); err != nil {
		t.Fatal(err)
	}

	t.Setenv(EnvBaseURL, "https://from-env.example/ignored/path")

	got, err := ResolveOrigin("https://from-flag.example/mcp", st)
	if err != nil {
		t.Fatal(err)
	}
	if got.URL != "https://from-flag.example" || got.Source != "--url" {
		t.Fatalf("with a flag: %+v", got)
	}

	got, err = ResolveOrigin("", st)
	if err != nil {
		t.Fatal(err)
	}
	if got.URL != "https://from-env.example" || !strings.Contains(got.Source, EnvBaseURL) {
		t.Fatalf("with the environment: %+v", got)
	}

	t.Setenv(EnvBaseURL, "")
	got, err = ResolveOrigin("", st)
	if err != nil {
		t.Fatal(err)
	}
	if got.URL != "https://stored.example" {
		t.Fatalf("with a stored login: %+v", got)
	}
}

func TestResolveOriginFallsBackToProduction(t *testing.T) {
	t.Setenv(EnvBaseURL, "")

	got, err := ResolveOrigin("", store(t))
	if err != nil {
		t.Fatal(err)
	}
	if got.URL != DefaultBaseURL {
		t.Fatalf("URL = %q, want %q", got.URL, DefaultBaseURL)
	}
	if got.Source == "" {
		t.Fatal("Source is empty; the whole point is saying where the host came from")
	}
}

func TestResolveOriginRefusesAnUnusableURL(t *testing.T) {
	t.Setenv(EnvBaseURL, "")

	if got, err := ResolveOrigin("not a url", store(t)); err == nil {
		t.Fatalf("ResolveOrigin = %+v, want an error", got)
	}
}

func TestAFailureQuotesAnErrorPageBrieflyAndOnOneLine(t *testing.T) {
	// A Phoenix error page is kilobytes of HTML across hundreds of lines.
	// Quoting it whole buries the one thing that matters — the status — under
	// a screenful of stylesheet.
	page := "<!DOCTYPE html>\n<html>\n<head>\n<style>" + strings.Repeat("a{color:red}\n", 500) + "</style>\n</head>\n</html>"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(500)
		io.WriteString(w, page)
	}))
	defer srv.Close()

	err := Login(context.Background(), Origin{URL: srv.URL, Source: "--url"}, store(t), io.Discard, testOptions())
	if err == nil {
		t.Fatal("the error page was not reported")
	}

	message := err.Error()
	if strings.Contains(message, "\n") {
		t.Fatalf("the message spans lines:\n%s", message)
	}
	if len(message) > 400 {
		t.Fatalf("the message is %d bytes; a terminal deserves better:\n%s", len(message), message)
	}
	if !strings.Contains(message, "500") {
		t.Fatalf("err = %v, want the status kept", err)
	}
}

// flakyTransport fails a round trip to one path outright — no response, no
// status — the way a dropped wifi connection, a proxy blinking or a dev server
// restarting mid-approval does. It is not the server answering; it is the
// request never arriving.
type flakyTransport struct {
	mu    sync.Mutex
	path  string
	fails int
	under http.RoundTripper
}

func (f *flakyTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	f.mu.Lock()
	if req.URL.Path == f.path && f.fails > 0 {
		f.fails--
		f.mu.Unlock()
		return nil, errors.New("dial tcp: connect: network is unreachable")
	}
	f.mu.Unlock()
	return f.under.RoundTrip(req)
}

// A person is standing at a browser with a code in their hand. A poll that
// never reached the host is not an answer from the host, and abandoning the
// login over one throws away an approval that is still good — the device code
// has its own deadline, and that deadline is the only thing entitled to end
// the wait.
func TestLoginRidesOutATransientPollFailure(t *testing.T) {
	srv, rec := flowServer(t, func(poll int, w http.ResponseWriter) {
		writeJSON(w, 200, map[string]any{"access_token": "tok-xyz", "token_type": "Bearer", "expires_in": 60})
	})

	opts := testOptions()
	opts.Client = &http.Client{Transport: &flakyTransport{
		path:  tokenPath,
		fails: 2,
		under: http.DefaultTransport,
	}}

	st := store(t)
	if err := Login(context.Background(), Origin{URL: srv.URL, Source: "--url"}, st, io.Discard, opts); err != nil {
		t.Fatalf("Login: %v — two unreachable polls must not end the flow", err)
	}

	// The server saw exactly the one poll that got through: the other two never
	// arrived, and neither was treated as a verdict.
	if got := rec.polls(); got != 1 {
		t.Fatalf("the server saw %d polls, want 1", got)
	}
	auth, err := st.LoadAuth()
	if err != nil {
		t.Fatalf("LoadAuth: %v", err)
	}
	if auth.AccessToken != "tok-xyz" {
		t.Fatalf("AccessToken = %q", auth.AccessToken)
	}
}

// Riding out a failure is not ignoring it: a host that is unreachable for the
// whole life of the device code still ends the wait, and says what went wrong
// rather than only that time ran out.
func TestLoginGivesUpWhenTheHostStaysUnreachable(t *testing.T) {
	srv, _ := flowServer(t, func(poll int, w http.ResponseWriter) {
		writeJSON(w, 400, map[string]any{"error": "authorization_pending"})
	})

	var mu sync.Mutex
	tick := 0
	clock := func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		at := fixedNow.Add(time.Duration(tick) * 600 * time.Second)
		tick++
		return at
	}

	err := Login(context.Background(), Origin{URL: srv.URL, Source: "--url"}, store(t), io.Discard,
		Options{
			Interval: instantly,
			Now:      clock,
			Client: &http.Client{Transport: &flakyTransport{
				path:  tokenPath,
				fails: 1000,
				under: http.DefaultTransport,
			}},
		})

	if !errors.Is(err, ErrExpired) {
		t.Fatalf("err = %v, want ErrExpired once the code's lifetime is past", err)
	}
	if !strings.Contains(err.Error(), "unreachable") {
		t.Fatalf("err = %v, want the transport failure named too — a person who cannot reach the host needs to know that", err)
	}
}

// RFC 8628 §3.5: slow_down means "keep waiting, but less often, and add five
// seconds". Reading it as a refusal abandons a login the server was still
// willing to complete.
func TestLoginHonoursSlowDown(t *testing.T) {
	var mu sync.Mutex
	var at []time.Time

	srv, rec := flowServer(t, func(poll int, w http.ResponseWriter) {
		mu.Lock()
		at = append(at, time.Now())
		mu.Unlock()

		if poll == 1 {
			writeJSON(w, 400, map[string]any{"error": "slow_down"})
			return
		}
		writeJSON(w, 200, map[string]any{"access_token": "tok", "token_type": "Bearer", "expires_in": 60})
	})

	opts := testOptions()
	opts.SlowDownStep = 40 * time.Millisecond

	if err := Login(context.Background(), Origin{URL: srv.URL, Source: "--url"}, store(t), io.Discard, opts); err != nil {
		t.Fatalf("Login: %v — slow_down is not a refusal", err)
	}
	if got := rec.polls(); got != 2 {
		t.Fatalf("polled %d times, want 2", got)
	}

	mu.Lock()
	defer mu.Unlock()
	if gap := at[1].Sub(at[0]); gap < 30*time.Millisecond {
		t.Fatalf("the second poll came %s after the first; slow_down must lengthen the interval", gap)
	}
}

// SlowDownStep defaults to RFC 8628's five seconds, which is the value a
// person gets. The tests shorten it; nothing else may.
func TestTheDefaultSlowDownStepIsFiveSeconds(t *testing.T) {
	if got := (Options{}).withDefaults().SlowDownStep; got != 5*time.Second {
		t.Fatalf("SlowDownStep = %s, want 5s (RFC 8628 §3.5)", got)
	}
}

func TestWaitNeverSpinsOnANonPositiveInterval(t *testing.T) {
	// An interval of zero — a server that sent one, or arithmetic that produced
	// one — must not turn the poll loop into a spin against someone's MCP host.
	// Proving the wait took real time is enough; waiting out the whole floor is
	// not worth a second of suite time.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()

	start := time.Now()
	err := wait(ctx, 0)

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("wait(ctx, 0) = %v, want it still waiting when the context ran out", err)
	}
	if elapsed := time.Since(start); elapsed < 20*time.Millisecond {
		t.Fatalf("wait(ctx, 0) returned after %s — a non-positive interval must be clamped, not hot-looped", elapsed)
	}
}

// A device endpoint that answers with a path rather than an absolute URL is
// still telling the person where to go. Printing "/device" at them is not.
func TestLoginResolvesARelativeVerificationURIAgainstTheHost(t *testing.T) {
	var srv *httptest.Server

	mux := http.NewServeMux()
	mux.HandleFunc("/oauth/device/code", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{
			"device_code":               "dev-1",
			"user_code":                 "ABCD-EFGH",
			"verification_uri":          "/device",
			"verification_uri_complete": "/device?user_code=ABCD-EFGH",
			"expires_in":                900,
			"interval":                  5,
		})
	})
	mux.HandleFunc("/oauth/token", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"access_token": "tok", "token_type": "Bearer", "expires_in": 60})
	})
	srv = httptest.NewServer(mux)
	defer srv.Close()

	var out strings.Builder
	if err := Login(context.Background(), Origin{URL: srv.URL, Source: "--url"}, store(t), &out, testOptions()); err != nil {
		t.Fatalf("Login: %v", err)
	}

	for _, want := range []string{
		"Open  " + srv.URL + "/device\n",
		"Or    " + srv.URL + "/device?user_code=ABCD-EFGH\n",
	} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("login printed:\n%s\nwant a line %q", out.String(), want)
		}
	}
}

// A stored expiry does not replace server confirmation.
func TestLogoutRevokesEvenWhenAnExpiryIsStored(t *testing.T) {
	st := store(t)
	expiry := fixedNow.Add(30 * 24 * time.Hour)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	defer srv.Close()

	if err := st.SaveAuth(state.Auth{
		BaseURL:     srv.URL,
		AccessToken: "tok",
		ExpiresAt:   expiry.Unix(),
	}); err != nil {
		t.Fatal(err)
	}

	var out strings.Builder
	if err := Logout(st, &out); err != nil {
		t.Fatalf("Logout: %v", err)
	}

	want := "Signed out.\n"

	if got := out.String(); got != want {
		t.Fatalf("Logout printed:\n%s\nwant:\n%s", got, want)
	}
}

func TestLogoutConfirmsWithoutAnExpiry(t *testing.T) {
	st := store(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	defer srv.Close()
	if err := st.SaveAuth(state.Auth{BaseURL: srv.URL, AccessToken: "tok"}); err != nil {
		t.Fatal(err)
	}

	var out strings.Builder
	if err := Logout(st, &out); err != nil {
		t.Fatalf("Logout: %v", err)
	}

	want := "Signed out.\n"
	if got := out.String(); got != want {
		t.Fatalf("Logout printed:\n%s\nwant:\n%s", got, want)
	}
}

// serverEnrollment is the `enrollment` object Tier B merges into the token
// response for an approval that named a server: the two secrets that buy a node
// identity, the head to present them to, and — when the organization already
// has a placed gateway — the trio needed to reach it.
func serverEnrollment() map[string]any {
	return map[string]any{
		"enrollment_id": "enr-1",
		"preauth_key":   "tskey-auth-abc",
		"nonce":         "nonce-xyz",
		"head_url":      "https://head.example",
		"expires_at":    "2026-09-06T13:00:00Z",
		"gateway_addr":  "100.64.0.7",
		"gateway_port":  8443,
		"gateway_ca":    "-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----\n",
	}
}

// The enrollment rides back on the token response, so the login that earned it
// is the only moment it can be captured. Every field is stored as the server
// spelled it: expires_at stays the ISO 8601 string it sent, and the gateway
// trio is what `serve` will dial.
func TestLoginStoresTheServerEnrollmentTheTokenCarried(t *testing.T) {
	srv, _ := flowServer(t, func(poll int, w http.ResponseWriter) {
		writeJSON(w, 200, map[string]any{
			"access_token": "tok-xyz",
			"token_type":   "Bearer",
			"expires_in":   2592000,
			"enrollment":   serverEnrollment(),
		})
	})

	st := store(t)
	opts := testOptions()
	opts.Server = true

	var out strings.Builder
	if err := Login(context.Background(), Origin{URL: srv.URL, Source: "--url"}, st, &out, opts); err != nil {
		t.Fatalf("Login: %v", err)
	}

	stored, err := st.LoadAuth()
	if err != nil {
		t.Fatalf("LoadAuth: %v", err)
	}
	if stored.AccessToken != "tok-xyz" {
		t.Fatalf("AccessToken = %q — the bearer is still stored as it always was", stored.AccessToken)
	}

	enrollment, err := st.LoadEnrollment()
	if err != nil {
		t.Fatalf("LoadEnrollment: %v", err)
	}
	want := state.Enrollment{
		EnrollmentID: "enr-1",
		PreauthKey:   "tskey-auth-abc",
		Nonce:        "nonce-xyz",
		HeadURL:      "https://head.example",
		ExpiresAt:    "2026-09-06T13:00:00Z",
		GatewayAddr:  "100.64.0.7",
		GatewayPort:  8443,
		GatewayCA:    "-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----\n",
	}
	want.Origin = srv.URL
	if enrollment != want {
		t.Fatalf("enrollment = %#v, want %#v", enrollment, want)
	}

	// Two secrets in a file: the mode is part of the behaviour, not a detail.
	info, err := os.Stat(filepath.Join(st.Dir, "enrollment.json"))
	if err != nil {
		t.Fatalf("stat enrollment.json: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("enrollment.json mode = %v, want 0600", got)
	}

	if !strings.Contains(out.String(), "serve") {
		t.Fatalf("login printed:\n%s\nwant a line naming `serve` as what comes next", out.String())
	}
}

// The flag is a client-side expectation and nothing else: the person asked to
// enrol a server, and an approval that named no server did not do that. Saying
// "Signed in." would leave them waiting for a `serve` that can never start.
func TestLoginAsAServerRefusesAnApprovalThatNamedNoServer(t *testing.T) {
	srv, _ := flowServer(t, func(poll int, w http.ResponseWriter) {
		writeJSON(w, 200, map[string]any{
			"access_token": "tok-xyz",
			"token_type":   "Bearer",
			"expires_in":   2592000,
		})
	})

	st := store(t)
	opts := testOptions()
	opts.Server = true

	var out strings.Builder
	err := Login(context.Background(), Origin{URL: srv.URL, Source: "--url"}, st, &out, opts)
	if !errors.Is(err, ErrNoServerEnrollment) {
		t.Fatalf("Login = %v, want ErrNoServerEnrollment", err)
	}

	if _, err := st.LoadEnrollment(); !state.IsNoEnrollment(err) {
		t.Fatalf("LoadEnrollment = %v, want no enrollment stored", err)
	}
	if strings.Contains(out.String(), "Signed in") {
		t.Fatalf("login printed:\n%s\nwant no success notice", out.String())
	}
}

// The person is at the browser with a code in their hand, and the one thing
// that makes this login a server enrolment is which button they press there.
func TestLoginAsAServerSaysWhichApprovalToChoose(t *testing.T) {
	srv, rec := flowServer(t, func(poll int, w http.ResponseWriter) {
		writeJSON(w, 200, map[string]any{
			"access_token": "tok",
			"token_type":   "Bearer",
			"expires_in":   60,
			"enrollment":   serverEnrollment(),
		})
	})

	opts := testOptions()
	opts.Server = true
	opts.MachineLabel = "build-box"

	var out strings.Builder
	if err := Login(context.Background(), Origin{URL: srv.URL, Source: "--url"}, store(t), &out, opts); err != nil {
		t.Fatalf("Login: %v", err)
	}
	if !strings.Contains(out.String(), "This is a server for") {
		t.Fatalf("login printed:\n%s\nwant the words the browser puts on the button", out.String())
	}
	if strings.Contains(out.String(), "?user_code=") {
		t.Fatal("server login printed a prefilled approval link")
	}
	form, _, _ := rec.seen()
	if form.Get("machine_label") != "build-box" {
		t.Fatalf("machine_label = %q, want build-box", form.Get("machine_label"))
	}
}

// A plain login is untouched by any of this: no enrollment arrives, none is
// expected, and nothing new is written beside the bearer.
func TestPlainLoginStoresNoEnrollment(t *testing.T) {
	srv, _ := flowServer(t, func(poll int, w http.ResponseWriter) {
		writeJSON(w, 200, map[string]any{"access_token": "tok", "token_type": "Bearer", "expires_in": 60})
	})

	st := store(t)
	var out strings.Builder
	if err := Login(context.Background(), Origin{URL: srv.URL, Source: "--url"}, st, &out, testOptions()); err != nil {
		t.Fatalf("Login: %v", err)
	}

	if _, err := st.LoadEnrollment(); !state.IsNoEnrollment(err) {
		t.Fatalf("LoadEnrollment = %v, want no enrollment stored", err)
	}
	if !strings.Contains(out.String(), "Signed in.") {
		t.Fatalf("login printed:\n%s\nwant the plain sign-in notice", out.String())
	}
	if strings.Contains(out.String(), "This is a server for") {
		t.Fatalf("login printed:\n%s\nwant no server guidance", out.String())
	}
}

// The key is absent from every plain login, so the pointer has to survive its
// absence as nil rather than as a zero enrollment that would be stored.
func TestTokenResponseWithoutAnEnrollmentDecodesAsNil(t *testing.T) {
	var token tokenResponse
	if err := json.Unmarshal([]byte(`{"access_token":"tok","token_type":"Bearer","expires_in":60}`), &token); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if token.Enrollment != nil {
		t.Fatalf("Enrollment = %#v, want nil", token.Enrollment)
	}
}

// A login is a login: joining is `gentility net join`, which fetches its own
// material when it joins. So no poll asks for an enrollment, and a person's
// login never burns a pre-auth key it will not use within ten minutes.
func TestLoginNeverSendsAMachineLabel(t *testing.T) {
	srv, rec := flowServer(t, func(poll int, w http.ResponseWriter) {
		if poll < 2 {
			writeJSON(w, 400, map[string]any{"error": "authorization_pending"})
			return
		}
		writeJSON(w, 200, map[string]any{"access_token": "tok", "token_type": "Bearer", "expires_in": 60})
	})

	if err := Login(context.Background(), Origin{URL: srv.URL, Source: "--url"}, store(t), io.Discard, testOptions()); err != nil {
		t.Fatalf("Login: %v", err)
	}

	_, _, polls := rec.seen()
	if len(polls) != 2 {
		t.Fatalf("polled %d times, want 2", len(polls))
	}
	for i, form := range polls {
		if _, sent := form["machine_label"]; sent {
			t.Fatalf("poll %d sent machine_label %v, want it never sent", i+1, form["machine_label"])
		}
	}
}

// The server sends a person's enrollment only to a poll that asked for one,
// and no poll does. Should one arrive anyway, a person's login neither stores
// it nor mentions it: material that dies in ten minutes, written where nothing
// will use it, is a pre-auth key lying in a home directory.
func TestAPersonsLoginIgnoresAnEnrollmentItDidNotAskFor(t *testing.T) {
	srv, _ := flowServer(t, func(poll int, w http.ResponseWriter) {
		writeJSON(w, 200, map[string]any{
			"access_token": "tok-person-secret",
			"token_type":   "Bearer",
			"expires_in":   60,
			"enrollment":   serverEnrollment(),
		})
	})

	st := store(t)
	var out strings.Builder
	if err := Login(context.Background(), Origin{URL: srv.URL, Source: "--url"}, st, &out, testOptions()); err != nil {
		t.Fatalf("Login: %v", err)
	}

	if _, err := st.LoadAuth(); err != nil {
		t.Fatalf("LoadAuth: %v — the bearer is stored as always", err)
	}
	if _, err := os.Stat(filepath.Join(st.Dir, "enrollment.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stat enrollment.json = %v, want no file written", err)
	}

	expiry := time.Unix(fixedNow.Add(60*time.Second).Unix(), 0).Format(expiryLayout)
	want := fmt.Sprintf("Signed in. This token expires on %s.\n", expiry)
	if !strings.HasSuffix(out.String(), "Waiting for approval…\n"+want) {
		t.Fatalf("login printed:\n%s\nwant it to end with the sign-in line alone:\n%s", out.String(), want)
	}
	for _, word := range []string{"enrol", "6net", "tok-person-secret", "tskey-auth-abc"} {
		if strings.Contains(out.String(), word) {
			t.Fatalf("login printed:\n%s\nwant nothing containing %q", out.String(), word)
		}
	}
}

func TestMachineLabelForIsTheHostnamesFirstLabel(t *testing.T) {
	for hostname, want := range map[string]string{
		"willow.local": "willow",
		"willow":       "willow",
		"box.home.lan": "box",
		".local":       "",
		"":             "",
	} {
		if got := MachineLabelFor(hostname); got != want {
			t.Errorf("MachineLabelFor(%q) = %q, want %q", hostname, got, want)
		}
	}
}
