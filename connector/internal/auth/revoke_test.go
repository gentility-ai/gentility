package auth

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gentility-ai/gentility/connector/internal/state"
)

func TestRevokeHonoursCancellationAndRetainsAuth(t *testing.T) {
	st := store(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("cancelled request reached server") }))
	defer srv.Close()
	if err := st.SaveAuth(state.Auth{BaseURL: srv.URL, AccessToken: "test-bearer"}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := Revoke(ctx, st); err == nil {
		t.Fatal("cancelled revocation succeeded")
	}
	if _, err := st.LoadAuth(); err != nil {
		t.Fatal("lost auth", err)
	}
}

func TestLogoutRetainsUnreadableAuth(t *testing.T) {
	st := store(t)
	if err := st.SaveAuth(state.Auth{BaseURL: "https://example.com", AccessToken: "test-bearer"}); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(st.Dir, "auth.json")
	if err := os.WriteFile(file, []byte("{broken"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := Logout(st, io.Discard); err == nil {
		t.Fatal("unreadable auth silently deleted")
	}
	if _, err := os.Stat(file); err != nil {
		t.Fatal("auth file deleted", err)
	}
}

func TestLogoutConfirmsRevocationBeforeClearingAuth(t *testing.T) {
	st := store(t)
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		if r.Method != "POST" || r.URL.Path != "/oauth/revoke" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		if err := r.ParseForm(); err != nil {
			t.Error(err)
		}
		if r.Form.Get("token") != "test-bearer" {
			t.Error("missing token")
		}
		if r.Form.Get("client_id") != ClientID {
			t.Error("missing client binding")
		}
		if _, err := st.LoadAuth(); err != nil {
			t.Error("auth deleted before confirmation")
		}
		w.WriteHeader(200)
	}))
	defer srv.Close()
	if err := st.SaveAuth(state.Auth{BaseURL: srv.URL, AccessToken: "test-bearer"}); err != nil {
		t.Fatal(err)
	}
	if err := Logout(st, io.Discard); err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("server never received revocation")
	}
	if _, err := st.LoadAuth(); !state.IsNotLoggedIn(err) {
		t.Fatal("auth not cleared")
	}
}

func TestLogoutRetainsAuthAndPreferencesOnRefusalOrRedirect(t *testing.T) {
	for _, status := range []int{302, 307, 401, 500} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			redirected := false
			target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { redirected = true; w.WriteHeader(200) }))
			defer target.Close()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Location", target.URL)
				w.WriteHeader(status)
				io.WriteString(w, "test-bearer")
			}))
			defer srv.Close()
			st := store(t)
			if err := st.SaveAuth(state.Auth{BaseURL: srv.URL, AccessToken: "test-bearer"}); err != nil {
				t.Fatal(err)
			}
			if err := st.SetRecentOrg("acme"); err != nil {
				t.Fatal(err)
			}
			err := Logout(st, io.Discard)
			if err == nil {
				t.Fatal("unconfirmed logout succeeded")
			}
			if strings.Contains(err.Error(), "test-bearer") {
				t.Fatal("secret leaked")
			}
			if !strings.Contains(err.Error(), filepath.Join(st.Dir, "auth.json")) {
				t.Fatal("missing manual local-only escape path")
			}
			if redirected {
				t.Fatal("followed redirect")
			}
			if _, err := st.LoadAuth(); err != nil {
				t.Fatal("lost auth", err)
			}
			if st.RecentOrg() != "acme" {
				t.Fatal("lost preferences")
			}
		})
	}
}
