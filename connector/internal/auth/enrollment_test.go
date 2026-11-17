package auth

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/gentility-ai/gentility/connector/internal/state"
)

// enrollmentRequest is what the enrollment endpoint saw.
type enrollmentRequest struct {
	method        string
	path          string
	authorization string
	contentType   string
	accept        string
	body          map[string]any
}

// enrollmentServer answers POST /oauth/enrollment with status and body, and
// records each request it saw.
func enrollmentServer(t *testing.T, status int, body map[string]any) (*httptest.Server, func() []enrollmentRequest) {
	t.Helper()
	var (
		mu   sync.Mutex
		seen []enrollmentRequest
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var decoded map[string]any
		json.Unmarshal(raw, &decoded)

		mu.Lock()
		seen = append(seen, enrollmentRequest{
			method:        r.Method,
			path:          r.URL.Path,
			authorization: r.Header.Get("Authorization"),
			contentType:   r.Header.Get("Content-Type"),
			accept:        r.Header.Get("Accept"),
			body:          decoded,
		})
		mu.Unlock()

		writeJSON(w, status, body)
	}))
	t.Cleanup(srv.Close)

	return srv, func() []enrollmentRequest {
		mu.Lock()
		defer mu.Unlock()
		return append([]enrollmentRequest(nil), seen...)
	}
}

func TestFetchEnrollmentAnswersTheMaterialTheServerMinted(t *testing.T) {
	srv, seen := enrollmentServer(t, 200, map[string]any{"enrollment": serverEnrollment()})

	got, err := FetchEnrollment(context.Background(), nil, srv.URL, "bearer-secret", "cedar")
	if err != nil {
		t.Fatalf("FetchEnrollment: %v", err)
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
	if got != want {
		t.Fatalf("enrollment = %#v, want %#v", got, want)
	}

	requests := seen()
	if len(requests) != 1 {
		t.Fatalf("saw %d requests, want 1", len(requests))
	}
	req := requests[0]
	if req.method != http.MethodPost || req.path != "/oauth/enrollment" {
		t.Fatalf("request = %s %s, want POST /oauth/enrollment", req.method, req.path)
	}
	if req.authorization != "Bearer bearer-secret" {
		t.Fatalf("Authorization = %q, want the person's bearer", req.authorization)
	}
	if !strings.HasPrefix(req.contentType, "application/json") || req.accept != "application/json" {
		t.Fatalf("Content-Type = %q, Accept = %q, want JSON both ways", req.contentType, req.accept)
	}
	if req.body["machine_label"] != "cedar" || len(req.body) != 1 {
		t.Fatalf("body = %v, want {\"machine_label\": \"cedar\"}", req.body)
	}
}

// Each refusal the server can give has its own remedy, so each is its own
// error rather than a status code the caller has to know the meaning of.
func TestFetchEnrollmentNamesEachRefusal(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   map[string]any
		want   error
	}{
		{"no gateway", 409, map[string]any{"error": "no_gateway", "error_description": "None of your organizations has a gateway"}, ErrNoGateway},
		{"head unavailable", 503, map[string]any{"error": "head_unavailable"}, ErrHeadUnavailable},
		{"session expired", 401, map[string]any{"error": "invalid_token"}, ErrSessionExpired},
		{"not a device login", 403, map[string]any{"error": "invalid_client"}, ErrNotDeviceLogin},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := enrollmentServer(t, tc.status, tc.body)
			_, err := FetchEnrollment(context.Background(), nil, srv.URL, "bearer", "cedar")
			if !errors.Is(err, tc.want) {
				t.Fatalf("FetchEnrollment = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestFetchEnrollmentQuotesAnyOtherRefusalsDescription(t *testing.T) {
	srv, _ := enrollmentServer(t, 422, map[string]any{
		"error":             "invalid_request",
		"error_description": "A machine_label is required",
	})

	_, err := FetchEnrollment(context.Background(), nil, srv.URL, "bearer", "cedar")
	if err == nil || !strings.Contains(err.Error(), "A machine_label is required") || !strings.Contains(err.Error(), "422") {
		t.Fatalf("FetchEnrollment = %v, want an error naming 422 and the description", err)
	}
}

// A 200 is where the secrets live, so an answer of the wrong shape is
// refused without quoting a word of it.
func TestFetchEnrollmentRefusesAnAnswerWithoutMaterialAndQuotesNoneOfIt(t *testing.T) {
	for name, body := range map[string]map[string]any{
		"no enrollment": {"other": "tskey-auth-leaked"},
		"no preauth key": {"enrollment": map[string]any{
			"enrollment_id": "enr-1",
			"nonce":         "tskey-auth-leaked",
			"head_url":      "https://head.example",
		}},
	} {
		t.Run(name, func(t *testing.T) {
			srv, _ := enrollmentServer(t, 200, body)
			_, err := FetchEnrollment(context.Background(), nil, srv.URL, "bearer", "cedar")
			if err == nil {
				t.Fatal("FetchEnrollment succeeded, want a refusal")
			}
			if strings.Contains(err.Error(), "tskey-auth-leaked") {
				t.Fatalf("FetchEnrollment = %v, quoted the answer", err)
			}
		})
	}
}

func TestFetchEnrollmentSaysItNeverReachedAClosedHost(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	base := srv.URL
	srv.Close()

	_, err := FetchEnrollment(context.Background(), nil, base, "bearer-secret", "cedar")
	if !isTransport(err) {
		t.Fatalf("FetchEnrollment = %v, want a transport error", err)
	}
	if strings.Contains(err.Error(), "bearer-secret") {
		t.Fatalf("FetchEnrollment = %v, quoted the bearer", err)
	}
}

// The answer carries the gateway's CA, so it may run well past an OAuth
// answer's cap; it is read whole up to the enrollment endpoint's own.
func TestFetchEnrollmentReadsAnAnswerLargerThanAnOAuthOne(t *testing.T) {
	enrollment := serverEnrollment()
	enrollment["gateway_ca"] = strings.Repeat("A", 32<<10)
	srv, _ := enrollmentServer(t, 200, map[string]any{"enrollment": enrollment})

	got, err := FetchEnrollment(context.Background(), nil, srv.URL, "bearer", "cedar")
	if err != nil {
		t.Fatalf("FetchEnrollment: %v", err)
	}
	if len(got.GatewayCA) != 32<<10 {
		t.Fatalf("GatewayCA is %d bytes, want all %d", len(got.GatewayCA), 32<<10)
	}
}

// An answer over the cap is refused as too large, not cut short and then
// refused as the wrong shape, and still quotes none of what it held.
func TestFetchEnrollmentRefusesAnOversizeAnswerAndQuotesNoneOfIt(t *testing.T) {
	enrollment := serverEnrollment()
	enrollment["preauth_key"] = "tskey-auth-leaked"
	enrollment["gateway_ca"] = strings.Repeat("A", maxEnrollmentBody)
	srv, _ := enrollmentServer(t, 200, map[string]any{"enrollment": enrollment})

	_, err := FetchEnrollment(context.Background(), nil, srv.URL, "bearer", "cedar")
	if !errors.Is(err, ErrAnswerTooLarge) {
		t.Fatalf("FetchEnrollment = %v, want ErrAnswerTooLarge", err)
	}
	if isTransport(err) {
		t.Fatalf("FetchEnrollment = %v, want the host's answer, not a transport error", err)
	}
	if strings.Contains(err.Error(), "tskey-auth-leaked") {
		t.Fatalf("FetchEnrollment = %v, quoted the answer", err)
	}
}
