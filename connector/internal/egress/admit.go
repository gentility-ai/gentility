package egress

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/netip"
	"strconv"
	"time"

	"github.com/gentility-ai/gentility/connector/internal/version"
)

// Decision is Gentility's answer to one CONNECT. The zero value is a refusal,
// which is why every error path in this package can return it unchanged.
type Decision struct {
	Admitted bool
	Reason   string
	FactID   string
}

// AdmitClient calls POST /overlay/egress/admit over the overlay, through the
// gateway. The relay caches nothing and there is no fallback — a
// timeout, a transport error or a malformed answer all mean refuse.
//
// It authenticates exactly as every other node → mothership call does
// (`internal/config`.Client): the node credential as a bearer and the capver
// header, over the connector's overlay-dialling http.Client. It holds no
// overlay address of its own — the client's transport is what pins the source,
// the same way the config pull and the lease dial do.
type AdmitClient struct {
	http    *http.Client
	baseURL string
	cred    string
	// Timeout is the whole-call budget. Kept as a field so the
	// tests can shorten it; production leaves it at 2s.
	Timeout time.Duration
}

// NewAdmitClient builds the client the relay calls once per CONNECT.
//
// hc is the connector's overlay-dialling http.Client — the same transport the
// config pull uses, whose dials are pinned to this node's source address.
// baseURL is where Gentility answers on the overlay (the config document's
// gateway address); credential is this node's own node credential from
// `state.Node`.
func NewAdmitClient(hc *http.Client, baseURL, credential string) *AdmitClient {
	return &AdmitClient{http: hc, baseURL: baseURL, cred: credential, Timeout: 2 * time.Second}
}

type admitRequest struct {
	SourceAddress string `json:"source_address"`
	Host          string `json:"host"`
	Port          int    `json:"port"`
}

type admitResponse struct {
	Decision string `json:"decision"`
	Reason   string `json:"reason"`
	FactID   string `json:"fact_id"`
}

// Admit asks Gentility whether src may reach host:port, and fails closed on
// every path that is not an explicit admit. A timeout (the 2 s budget), a
// transport error, a non-200, or an answer that does not say "admit" all return
// the zero Decision — never admitted — and, except for a plain refusal, an
// error. The relay turns anything that is not an admitted Decision into a
// refused CONNECT.
//
// The refusal is not an error: a 200 carrying `decision: "refuse"` is
// Gentility's answer, so it returns `(Decision{Admitted:false, Reason:…}, nil)`
// — `POST /overlay/egress/admit` answers 200 for both admission and refusal.
func (c *AdmitClient) Admit(ctx context.Context, src netip.Addr, host string, port int) (Decision, error) {
	ctx, cancel := context.WithTimeout(ctx, c.Timeout)
	defer cancel()

	body, err := json.Marshal(admitRequest{SourceAddress: src.String(), Host: host, Port: port})
	if err != nil {
		return Decision{}, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/overlay/egress/admit", bytes.NewReader(body))
	if err != nil {
		return Decision{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.cred)
	req.Header.Set("X-Node-Capver", strconv.Itoa(version.NodeCapver))

	resp, err := c.http.Do(req)
	if err != nil {
		return Decision{}, fmt.Errorf("egress: admit call failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return Decision{}, fmt.Errorf("egress: admit returned %d", resp.StatusCode)
	}

	var out admitResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return Decision{}, fmt.Errorf("egress: admit answer unreadable: %w", err)
	}

	return Decision{Admitted: out.Decision == "admit", Reason: out.Reason, FactID: out.FactID}, nil
}

// Close reports the byte counts for an admitted connection. Best-effort by
// design: the connection is over, the decision is already recorded, and a lost
// count must never be able to hold a socket open.
func (c *AdmitClient) Close(ctx context.Context, factID string, toHost, fromHost int64, reason string) error {
	ctx, cancel := context.WithTimeout(ctx, c.Timeout)
	defer cancel()

	body, err := json.Marshal(map[string]any{
		"fact_id":         factID,
		"bytes_to_host":   toHost,
		"bytes_from_host": fromHost,
		"close_reason":    reason,
	})
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/overlay/egress/close", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.cred)
	req.Header.Set("X-Node-Capver", strconv.Itoa(version.NodeCapver))

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return nil
}
