package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestLoginDisplayRefusesControls(t *testing.T) {
	poison := "\x1b]52;c;clipboard\a\r\n\u009b"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/oauth/device/code" {
			body := deviceCodeBody()
			body["verification_uri"] = "https://example.test/" + poison
			body["verification_uri_complete"] = "https://example.test/complete/" + poison
			body["user_code"] = "CODE" + poison
			writeJSON(w, 200, body)
		} else {
			writeJSON(w, 200, map[string]any{"access_token": "test-token", "token_type": "Bearer", "expires_in": 60})
		}
	}))
	defer server.Close()
	var out strings.Builder
	if err := Login(context.Background(), Origin{URL: server.URL, Source: "--url"}, store(t), &out, testOptions()); err != nil {
		t.Fatal(err)
	}
	for _, r := range out.String() {
		if r < 32 && r != '\n' || r >= 127 && r <= 159 {
			t.Fatalf("terminal control U+%04X in %q", r, out.String())
		}
	}
}
