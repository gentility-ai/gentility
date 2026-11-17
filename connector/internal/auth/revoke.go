package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gentility-ai/gentility/connector/internal/mcp"
	"github.com/gentility-ai/gentility/connector/internal/state"
)

// ErrReadLogin means revocation could not even inspect the stored login.
var ErrReadLogin = errors.New("could not read the stored login; local state was kept")

// Revoke confirms revocation of the stored MCP session without removing local
// state. It is bounded even when uninstall's caller has no deadline. A redirect
// never receives the credential, and response bodies never become diagnostics.
func Revoke(ctx context.Context, store *state.Store) error {
	stored, err := store.LoadAuth()
	if state.IsNotLoggedIn(err) {
		return nil
	}
	if err != nil {
		return ErrReadLogin
	}
	base, err := mcp.NormalizeBaseURL(stored.BaseURL)
	if err != nil {
		return fmt.Errorf("the stored login has an invalid server address; local state was kept")
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	form := url.Values{"token": {stored.AccessToken}, "token_type_hint": {"access_token"}, "client_id": {ClientID}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/oauth/revoke", strings.NewReader(form.Encode()))
	if err != nil {
		return fmt.Errorf("could not prepare session revocation; local state was kept")
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	client := &http.Client{Timeout: 5 * time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("could not confirm server sign-out; local state was kept. Check your connection and try again")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("the server did not confirm sign-out (HTTP %d); local state was kept", resp.StatusCode)
	}
	return nil
}
