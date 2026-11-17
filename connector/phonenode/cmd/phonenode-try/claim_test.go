package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

var secrets = []string{"code-secret-xyz", "poll-secret-xyz", "bearer-secret-xyz", "pk-secret-xyz", "nonce-secret-xyz"}

func noSecrets(t *testing.T, where, text string) {
	t.Helper()
	for _, secret := range secrets {
		if strings.Contains(text, secret) {
			t.Fatalf("%s carried %s:\n%s", where, secret, text)
		}
	}
}

// pollStep answers one poll; the last step answers every poll after it.
type pollStep func(w http.ResponseWriter)

func says(body string) pollStep {
	return func(w http.ResponseWriter) { io.WriteString(w, body) }
}

func fails(status int, body string) pollStep {
	return func(w http.ResponseWriter) {
		w.WriteHeader(status)
		io.WriteString(w, body)
	}
}

// hangsUp drops the connection without an answer: a transport error.
func hangsUp(w http.ResponseWriter) {
	conn, _, err := w.(http.Hijacker).Hijack()
	if err != nil {
		panic(err)
	}
	conn.Close()
}

// mothership answers claim and poll with the expected wire shapes: the claim at
// once, the poll asking until the third.
func mothership(t *testing.T, final string) *httptest.Server {
	t.Helper()
	return mothershipWith(t, time.Now().Add(time.Minute), says(`{"status":"asking"}`), says(`{"status":"asking"}`), says(final))
}

// mothershipWith answers the claim with a code that expires at expires, and
// the nth poll with steps[n], the last step for every poll past them.
func mothershipWith(t *testing.T, expires time.Time, steps ...pollStep) *httptest.Server {
	t.Helper()
	var polls atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("POST /oauth/phone/claim", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		json.NewDecoder(r.Body).Decode(&body)
		if body["code"] != "code-secret-xyz" || body["phone_name"] != "harness" {
			w.WriteHeader(http.StatusNotFound)
			io.WriteString(w, `{"error":"unknown","error_description":"No Desk showed that code"}`)
			return
		}
		w.WriteHeader(http.StatusAccepted)
		io.WriteString(w, `{"claim_id":"c1","machine_label":"driftwood","expires_at":"`+
			expires.UTC().Format(time.RFC3339Nano)+`","poll_secret":"poll-secret-xyz","match":"47"}`)
	})
	mux.HandleFunc("POST /oauth/phone/poll", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		json.NewDecoder(r.Body).Decode(&body)
		if body["poll_secret"] != "poll-secret-xyz" {
			w.WriteHeader(http.StatusNotFound)
			io.WriteString(w, `{"error":"unknown","error_description":"No phone was handed that poll secret"}`)
			return
		}
		n := int(polls.Add(1)) - 1
		steps[min(n, len(steps)-1)](w)
	})
	server := httptest.NewTLSServer(mux)
	t.Cleanup(server.Close)
	return server
}

// claimWith runs a claim against server through the client claimCommand
// builds, into a new state directory.
func claimWith(t *testing.T, server *httptest.Server) (dir string, out *bytes.Buffer, err error) {
	t.Helper()
	dir = filepath.Join(t.TempDir(), "state")
	out = &bytes.Buffer{}
	err = claim(context.Background(), claimClient(server.Client().Transport),
		claimOptions{Link: server.URL + "/phone/claim#code-secret-xyz", Name: "harness", Dir: dir}, nil, out)
	noSecrets(t, "the output", out.String())
	if err != nil {
		noSecrets(t, "the error", err.Error())
	}
	return dir, out, err
}

func wroteNothing(t *testing.T, dir string) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(dir, "phone.json")); !os.IsNotExist(err) {
		t.Fatal("a claim that did not end allowed wrote phone.json")
	}
}

const allowed = `{"status":"allowed","access_token":"bearer-secret-xyz","token_type":"Bearer","expires_in":2592000,` +
	`"scope":"agent:connect agent:execute","enrollment":{"enrollment_id":"e1","preauth_key":"pk-secret-xyz",` +
	`"nonce":"nonce-secret-xyz","head_url":"https://head.example","expires_at":"2026-10-24T12:15:00Z"}}`

func quickPolls(t *testing.T) {
	every := pollEvery
	pollEvery = 10 * time.Millisecond
	t.Cleanup(func() { pollEvery = every })
}

func TestClaimPrintsTheMatchAndKeepsSecretsInTheFile(t *testing.T) {
	quickPolls(t)
	server := mothership(t, allowed)
	dir := filepath.Join(t.TempDir(), "state")
	var out bytes.Buffer

	link := server.URL + "/phone/claim#code-secret-xyz"
	err := claim(context.Background(), server.Client(), claimOptions{Link: "-", Name: "harness", Dir: dir},
		strings.NewReader(link+"\n"), &out)
	if err != nil {
		t.Fatalf("claim: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "type 47") {
		t.Fatalf("the match was not printed:\n%s", out.String())
	}
	noSecrets(t, "the output", out.String())

	info, err := os.Stat(filepath.Join(dir, "phone.json"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("phone.json is %v, want 0600", info.Mode().Perm())
	}
	if dirInfo, _ := os.Stat(dir); dirInfo.Mode().Perm() != 0o700 {
		t.Fatalf("the state dir is %v, want 0700", dirInfo.Mode().Perm())
	}
	p, err := readPaired(dir)
	if err != nil {
		t.Fatal(err)
	}
	if p.AccessToken != "bearer-secret-xyz" || !strings.Contains(string(p.Enrollment), "pk-secret-xyz") || p.Origin != server.URL {
		t.Fatalf("phone.json = %+v", p)
	}
}

func TestClaimDeniedEndsWithTheWordAndWritesNothing(t *testing.T) {
	quickPolls(t)
	server := mothership(t, `{"status":"denied"}`)
	dir := filepath.Join(t.TempDir(), "state")
	var out bytes.Buffer

	err := claim(context.Background(), server.Client(),
		claimOptions{Link: server.URL + "/phone/claim#code-secret-xyz", Name: "harness", Dir: dir}, nil, &out)
	if err == nil || !strings.Contains(err.Error(), "denied") {
		t.Fatalf("claim = %v, want the pairing ended denied", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "phone.json")); !os.IsNotExist(err) {
		t.Fatal("a denied claim wrote phone.json")
	}
}

func TestClaimSaysTheServersRefusal(t *testing.T) {
	server := mothership(t, allowed)
	var out bytes.Buffer
	err := claim(context.Background(), server.Client(),
		claimOptions{Link: server.URL + "/phone/claim#not-the-code", Name: "harness", Dir: filepath.Join(t.TempDir(), "state")}, nil, &out)
	if err == nil || !strings.Contains(err.Error(), "unknown: No Desk showed that code") {
		t.Fatalf("claim = %v", err)
	}
}

func TestParseClaimLinkTakesWhatTheAppWould(t *testing.T) {
	for link, want := range map[string]string{
		"https://mcp.gentility.ai/phone/claim#code-secret-xyz":   "https://mcp.gentility.ai",
		"http://localhost:4001/phone/claim#code-secret-xyz":      "http://localhost:4001",
		"http://wt-7.localhost:4001/phone/claim#code-secret-xyz": "http://wt-7.localhost:4001",
	} {
		origin, code, err := parseClaimLink(link)
		if err != nil || origin != want || code != "code-secret-xyz" {
			t.Errorf("parseClaimLink(%q) = %q, %q, %v", link, origin, code, err)
		}
	}
	for _, link := range []string{
		"http://mcp.gentility.ai/phone/claim#code-secret-xyz",
		"https://mcp.gentility.ai/elsewhere#code-secret-xyz",
		"https://mcp.gentility.ai/phone/claim",
		"https://mcp.gentility.ai/phone/claim#",
		"%zz://code-secret-xyz",
		"https:///phone/claim#code-secret-xyz",
		"https://code-secret-xyz@mcp.gentility.ai/phone/claim#code-secret-xyz",
		"https://morgan:code-secret-xyz@mcp.gentility.ai/phone/claim#code-secret-xyz",
	} {
		_, _, err := parseClaimLink(link)
		if err == nil {
			t.Errorf("parseClaimLink(%q) took it", link)
			continue
		}
		noSecrets(t, "the refusal of "+link, err.Error())
	}
}

func TestClaimRefusesARedirect(t *testing.T) {
	var followed atomic.Bool
	mux := http.NewServeMux()
	mux.HandleFunc("POST /oauth/phone/claim", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/elsewhere", http.StatusTemporaryRedirect)
	})
	mux.HandleFunc("/elsewhere", func(http.ResponseWriter, *http.Request) { followed.Store(true) })
	server := httptest.NewTLSServer(mux)
	t.Cleanup(server.Close)

	_, _, err := claimWith(t, server)
	if err == nil || !strings.Contains(err.Error(), "HTTP 307") {
		t.Fatalf("claim = %v, want the redirect refused", err)
	}
	if followed.Load() {
		t.Fatal("the claim followed the redirect, code and all")
	}
}

func TestPollRetriesTransportErrorsAndServerErrors(t *testing.T) {
	quickPolls(t)
	server := mothershipWith(t, time.Now().Add(time.Minute),
		hangsUp,
		fails(http.StatusServiceUnavailable, ``),
		fails(http.StatusBadGateway, `{"error":"upstream","error_description":"try again"}`),
		says(`{"status":"asking"}`),
		says(allowed))

	dir, out, err := claimWith(t, server)
	if err != nil {
		t.Fatalf("claim: %v\n%s", err, out.String())
	}
	for _, want := range []string{"HTTP 503; trying again", "upstream: try again; trying again", "poll: asking", "poll: allowed"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("the output lacks %q:\n%s", want, out.String())
		}
	}
	if p, err := readPaired(dir); err != nil || p.AccessToken != "bearer-secret-xyz" {
		t.Fatalf("phone.json = %+v, %v", p, err)
	}
}

func TestPollGivesUpAtTheClaimsExpiry(t *testing.T) {
	quickPolls(t)
	server := mothershipWith(t, time.Now().Add(300*time.Millisecond), fails(http.StatusServiceUnavailable, ``))

	started := time.Now()
	dir, _, err := claimWith(t, server)
	if err == nil || !strings.Contains(err.Error(), "the code expired") {
		t.Fatalf("claim = %v, want the code expired", err)
	}
	if took := time.Since(started); took > 5*time.Second {
		t.Fatalf("claim took %s to give up", took)
	}
	wroteNothing(t, dir)
}

func TestPollEndsOnARefusal(t *testing.T) {
	quickPolls(t)
	server := mothershipWith(t, time.Now().Add(time.Minute),
		fails(http.StatusGone, `{"error":"spent","error_description":"That poll secret was used"}`))

	dir, _, err := claimWith(t, server)
	if err == nil || !strings.Contains(err.Error(), "spent: That poll secret was used") {
		t.Fatalf("claim = %v", err)
	}
	wroteNothing(t, dir)
}

func TestPollRepliesThatEndTheClaim(t *testing.T) {
	quickPolls(t)
	for reply, want := range map[string]string{
		`{"status":"failed","failure":"enrollment_unavailable"}`: "could not finish the pairing: enrollment_unavailable",
		`{"status":"expired"}`:      "the pairing ended expired",
		`{"status":"spent"}`:        "the pairing ended spent",
		`{"status":"reconsidered"}`: "the pairing ended reconsidered",
		`{"status":"allowed","token_type":"Bearer","enrollment":{"preauth_key":"pk-secret-xyz"}}`: "no bearer",
		`{"status":"allowed","access_token":"bearer-secret-xyz","token_type":"Bearer"}`:           "no join material",
	} {
		server := mothershipWith(t, time.Now().Add(time.Minute), says(reply))
		dir, out, err := claimWith(t, server)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("after %s: claim = %v, want %q\n%s", reply, err, want, out.String())
		}
		wroteNothing(t, dir)
	}
}

func TestPollKeepsPollingWhileAllowedIsPending(t *testing.T) {
	quickPolls(t)
	server := mothershipWith(t, time.Now().Add(time.Minute),
		says(`{"status":"asking"}`), says(`{"status":"allowed_pending"}`), says(`{"status":"allowed_pending"}`), says(allowed))

	dir, out, err := claimWith(t, server)
	if err != nil {
		t.Fatalf("claim: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "poll: allowed_pending") || strings.Count(out.String(), "poll: allowed_pending") != 1 {
		t.Fatalf("allowed_pending printed other than once:\n%s", out.String())
	}
	if _, err := readPaired(dir); err != nil {
		t.Fatal(err)
	}
}

func TestPollDeadlineIsTheClaimsExpiry(t *testing.T) {
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	if got := pollDeadline("2026-10-24T12:05:00Z", now); !got.Equal(now.Add(5 * time.Minute)) {
		t.Errorf("deadline = %s", got)
	}
	if got := pollDeadline("2026-10-24T12:05:00.123456Z", now); !got.Equal(now.Add(5*time.Minute + 123456*time.Microsecond)) {
		t.Errorf("deadline with microseconds = %s", got)
	}
	if got := pollDeadline("soon", now); !got.Equal(now.Add(claimLifetime)) {
		t.Errorf("deadline of an unreadable expiry = %s, want the server's lifetime", got)
	}
}

func TestClaimRefusesAStateDirectoryOthersCanEnter(t *testing.T) {
	server := mothership(t, allowed)
	dir := filepath.Join(t.TempDir(), "state")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	err := claim(context.Background(), claimClient(server.Client().Transport),
		claimOptions{Link: server.URL + "/phone/claim#code-secret-xyz", Name: "harness", Dir: dir}, nil, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "open to group or others") {
		t.Fatalf("claim = %v, want the directory refused", err)
	}
	if info, _ := os.Stat(dir); info.Mode().Perm() != 0o750 {
		t.Fatalf("the directory is now %v: claim changed a directory it did not make", info.Mode().Perm())
	}
	wroteNothing(t, dir)
}

func TestClaimUsesAnExistingPrivateStateDirectory(t *testing.T) {
	quickPolls(t)
	server := mothership(t, allowed)
	dir := filepath.Join(t.TempDir(), "state")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	err := claim(context.Background(), claimClient(server.Client().Transport),
		claimOptions{Link: server.URL + "/phone/claim#code-secret-xyz", Name: "harness", Dir: dir}, nil, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := readPaired(dir); err != nil {
		t.Fatal(err)
	}
}

// landsLate hands each poll's answer back only once the code has expired, as
// an answer the server sent in time can reach the phone after it.
type landsLate struct {
	base  http.RoundTripper
	after time.Time
}

func (l landsLate) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Path != "/oauth/phone/poll" {
		return l.base.RoundTrip(r)
	}
	response, err := l.base.RoundTrip(r.WithContext(context.WithoutCancel(r.Context())))
	if err != nil {
		return nil, err
	}
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		return nil, err
	}
	response.Body = io.NopCloser(bytes.NewReader(body))
	time.Sleep(time.Until(l.after))
	return response, nil
}

// The enrollment an allowed hands over is single-use: dropping the answer
// because it landed at the deadline would spend the pairing for nothing.
func TestPollKeepsAnAllowedThatLandsAtTheDeadline(t *testing.T) {
	quickPolls(t)
	expires := time.Now().Add(300 * time.Millisecond)
	server := mothershipWith(t, expires, says(allowed))
	dir := filepath.Join(t.TempDir(), "state")
	var out bytes.Buffer

	err := claim(context.Background(), claimClient(landsLate{base: server.Client().Transport, after: expires.Add(100 * time.Millisecond)}),
		claimOptions{Link: server.URL + "/phone/claim#code-secret-xyz", Name: "harness", Dir: dir}, nil, &out)
	if err != nil {
		t.Fatalf("claim = %v, want the allowed kept\n%s", err, out.String())
	}
	if p, err := readPaired(dir); err != nil || p.AccessToken != "bearer-secret-xyz" || len(p.Enrollment) == 0 {
		t.Fatalf("phone.json = %+v, %v", p, err)
	}
}
