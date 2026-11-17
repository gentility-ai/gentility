package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/gentility-ai/gentility/connector/internal/mcp"
	"github.com/gentility-ai/gentility/connector/internal/state"
)

// RevokeNode confirms withdrawal at the origin that issued this identity.
// It never changes local state and never follows a redirect with the credential.
func RevokeNode(ctx context.Context, store *state.Store) error {
	node, err := store.LoadNode()
	if state.IsNotEnrolled(err) {
		for _, name := range []string{"node.json", state.TSNetDir} {
			_, statErr := os.Lstat(filepath.Join(store.Dir, name))
			if statErr == nil {
				return errors.New("This machine's saved connection details are missing. Nothing was removed. Restore its original connection details, then run leave again. If they cannot be restored, ask an admin to remove this machine under Servers.")
			}
			if !errors.Is(statErr, os.ErrNotExist) {
				return errors.New("Couldn't inspect this machine's saved connection details. Nothing was removed. Check file permissions and run leave again.")
			}
		}
		return nil
	}
	if err != nil {
		return errors.New("Couldn't read this machine's saved connection details. Nothing was removed. Check file permissions and run leave again.")
	}
	if node.NodeID == "" || node.Origin == "" {
		return errors.New("This machine's saved connection details are incomplete. Nothing was removed. Restore its original connection details, then run leave again. If they cannot be restored, ask an admin to remove this machine under Servers.")
	}
	base, err := mcp.NormalizeBaseURL(node.Origin)
	if err != nil {
		return errors.New("This machine's saved Gentility address is invalid. Nothing was removed. Restore its original connection details, then run leave again.")
	}
	payload, err := json.Marshal(map[string]string{"node_id": node.NodeID, "credential": node.NodeCredential})
	if err != nil {
		return errors.New("Couldn't prepare this machine to leave the network. Nothing was removed. Check its saved connection details and run leave again.")
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/overlay/self/revoke", bytes.NewReader(payload))
	if err != nil {
		return errors.New("Couldn't prepare this machine to leave the network. Nothing was removed. Check its saved connection details and run leave again.")
	}
	request.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 5 * time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(request)
	if err != nil {
		return errors.New("Couldn't confirm with Gentility that this machine left the network. Nothing was removed from this machine. Check the connection and run leave again.")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return errors.New("Gentility did not confirm that this machine left the network. Nothing was removed from this machine. Check the connection and run leave again.")
	}
	var answer struct {
		Status string `json:"status"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 4096)).Decode(&answer); err != nil || answer.Status != "revoked" {
		return errors.New("Gentility did not confirm that this machine left the network. Nothing was removed from this machine. Check the connection and run leave again.")
	}
	return nil
}
