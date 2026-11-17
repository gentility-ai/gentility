package enroll

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"time"

	"github.com/gentility-ai/gentility/connector/internal/config"
	"github.com/gentility-ai/gentility/connector/internal/exitcodes"
	"github.com/gentility-ai/gentility/connector/internal/state"
	"github.com/gentility-ai/gentility/connector/internal/version"
)

// Path is the exchange endpoint.
const Path = "/overlay/enroll/exchange"

const (
	// exchangeTimeout bounds one attempt. The mothership mints two secrets and
	// writes a ledger event; there is no long poll here and nothing that
	// should take longer.
	exchangeTimeout = 30 * time.Second

	maxErrorBody = 2048
)

// HTTPError is a status the exchange has no other name for.
type HTTPError struct {
	StatusCode int
	Reason     string
	Body       string
}

func (e *HTTPError) Error() string {
	switch {
	case e.Reason != "":
		return fmt.Sprintf("enroll: the mothership answered %d %s", e.StatusCode, e.Reason)
	case e.Body != "":
		return fmt.Sprintf("enroll: the mothership answered %d: %s", e.StatusCode, e.Body)
	default:
		return fmt.Sprintf("enroll: the mothership answered %d", e.StatusCode)
	}
}

// material is the exchange's answer: the node's two secrets,
// handed over once.
type material struct {
	NodeID         string `json:"node_id"`
	NodeCredential string `json:"node_credential"`
	RuntimeToken   string `json:"runtime_token"`

	// Config is the document the renderer has already written for this node,
	// or null. It is decoded and deliberately **not applied**: one path applies
	// a document and it is config.Client's, whose first pull is held open by
	// the mothership and answers the moment there is one. Two paths would be
	// two places a revision could be recorded for a document the node did not
	// take on.
	Config *config.Document `json:"config"`
}

// Gateway resolves the base URL every call to Gentility goes to, and the TLS
// material to make it with.
//
// Three cases and one refusal:
//
//   - a loopback gateway is the dev recipe, where the "gateway" is the overlay
//     listener itself on this machine and there is no network to protect;
//   - a pinned ca_pem — the gateway's CA, delivered in the config document,
//     or over OVERLAY_GATEWAY_CA for the first exchange no document has
//     preceded — is verified against that CA and nothing else;
//   - OVERLAY_GATEWAY_INSECURE is the dev recipe's explicit opt-in to a
//     plaintext hop over the overlay, which WireGuard has already encrypted.
//
// Anything else is refused. **InsecureSkipVerify is never set**: an unverified
// https is a plaintext hop wearing a padlock, and the one thing worse than a
// deployment that knows it is insecure is one that thinks it is not.
func Gateway(addr netip.AddrPort, caPEM string, insecure bool) (string, *tls.Config, error) {
	if addr.Addr().IsLoopback() {
		if version.Connector != "dev" {
			return "", nil, usage(fmt.Errorf("enroll: a loopback gateway requires a dev connector"))
		}
		return "http://" + addr.String(), nil, nil
	}

	if caPEM != "" {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM([]byte(caPEM)) {
			return "", nil, usage(fmt.Errorf(
				"enroll: the gateway's ca_pem holds no certificate this connector could read",
			))
		}
		return "https://" + addr.String(), &tls.Config{RootCAs: pool}, nil
	}

	if insecure {
		return "http://" + addr.String(), nil, nil
	}

	return "", nil, usage(fmt.Errorf(
		"enroll: the gateway at %s has no pinned CA and %s is not set, so there is no way to "+
			"reach it this connector is willing to use — a first join hands the CA over %s, "+
			"the way it was handed the enrollment",
		addr, VarGatewayInsecure, VarGatewayCA,
	))
}

// Client builds the HTTP client every call to Gentility is made with.
//
// dial is tsnode's: it pins the source address to this node's own in the
// gateway's organization, which is what makes the gateway's WhoIs attribute
// the connection to this node rather than dropping it. A nil dial
// is the loopback dev recipe, where there is no overlay to dial through.
//
// peer is the address this node names as its own, and it is set for exactly
// the hop that has no gateway on it to name one — see Direct. An invalid peer
// sends no header, which is every hop that goes through a gateway.
func Client(dial func(context.Context, string, string) (net.Conn, error), tlsConfig *tls.Config, peer netip.Addr) *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = tlsConfig
	// The connector's OWN control-plane traffic must never honour an ambient
	// proxy. DefaultTransport's Proxy is ProxyFromEnvironment, and the build env
	// this connector launches inside sets HTTPS_PROXY to the connector's own
	// preflight port — which it binds only AFTER this exchange and the first
	// config pull. Routing bootstrap through that not-yet-listening port would
	// deadlock the join on a real (non-loopback) gateway. Nil is the structural
	// guarantee, independent of the launcher's environment cleanup.
	transport.Proxy = nil
	if dial != nil {
		transport.DialContext = dial
	}

	if !peer.IsValid() {
		return &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}

	return &http.Client{Transport: peerHeader{peer: peer.String(), inner: transport}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

// Exchange trades the nonce for this node's two secrets and writes them down.
//
// It is one attempt. Retrying is the caller's, and it is safe: the mothership
// is idempotent on (enrollment_id, node_id, nonce) for one hour, which is
// exactly how long the material it hands back lives. Past that hour a retry is
// a 410 and the node must enrol again.
//
// httpClient is the overlay-dialling client from Client; a nil one is a plain
// client, which is what the tests and the loopback dev recipe use.
func Exchange(ctx context.Context, httpClient *http.Client, base string, env Env, store *state.Store) (state.Node, error) {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: exchangeTimeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}

	// The bound belongs on the context, not on the client: the connector's own
	// client dials through the overlay and carries no Timeout, so an exchange
	// against a listener that accepts and then says nothing would hang this
	// process indefinitely rather than exiting Temporary for a restart.
	ctx, cancel := context.WithTimeout(ctx, exchangeTimeout)
	defer cancel()

	body, err := json.Marshal(map[string]string{
		"enrollment_id": env.EnrollmentID,
		"nonce":         env.Nonce,
	})
	if err != nil {
		return state.Node{}, fmt.Errorf("enroll: encoding the exchange: %w", err)
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, base+Path, bytes.NewReader(body))
	if err != nil {
		return state.Node{}, fmt.Errorf("enroll: building the exchange: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	request.Header.Set("X-Node-Capver", strconv.Itoa(version.NodeCapver))
	// No Authorization header, deliberately: the nonce is the authentication
	// and the attributed address is what it is checked against.

	response, err := httpClient.Do(request)
	if err != nil {
		return state.Node{}, fmt.Errorf("enroll: exchanging at %s: %w", base+Path, err)
	}
	defer response.Body.Close()

	answer, err := read(response)
	if err != nil {
		return state.Node{}, err
	}

	node := state.Node{
		Origin:         env.Origin,
		NodeID:         answer.NodeID,
		NodeCredential: answer.NodeCredential,
		RuntimeToken:   answer.RuntimeToken,
		// The cursor stays unset even when a document came back with the
		// material — see material.Config.
		LastRevision: state.NoRevision,
		// The two the node cannot be brought back without. They come from the
		// Env rather than from the answer because they are facts about the join
		// that just happened, and this is the only moment that holds both the
		// join environment and a place to write it down.
		HeadURL:  env.HeadURL,
		Hostname: env.Hostname,
	}

	// The gateway and the CA that verifies it, written down at the moment they
	// first exist rather than when the first config document arrives.
	//
	// Store.RecordGateway still refreshes all three from every document, and
	// that is where they change; but it must not be the FIRST to write them.
	// `serve` clears the enrollment the moment this exchange succeeds, so a
	// daemon that stops between here and the first document comes back through
	// EnvFromNode — and a node.json holding an address with no CA is one Gateway
	// refuses to dial, leaving the node unable to fetch the very document that
	// would have handed it the CA.
	//
	// Only when there is a gateway: a gatewayless join must write nothing, not
	// the zero AddrPort's "invalid IP" placeholder, which is neither an address
	// nor an absence.
	if env.GatewayAddr.IsValid() {
		node.GatewayAddr = env.GatewayAddr.Addr().String()
		node.GatewayPort = int(env.GatewayAddr.Port())
		node.CAPEM = env.GatewayCA
	}

	if err := store.SaveNode(node); err != nil {
		return state.Node{}, fmt.Errorf("enroll: writing down the exchange: %w", err)
	}

	return node, nil
}

func read(response *http.Response) (material, error) {
	switch response.StatusCode {
	case http.StatusOK:
		return answer(response.Body)

	// Refused or compromised from the exchange itself, and invalid_peer or
	// unknown_peer from the ingress. All four say the same thing to a node:
	// this material will never be handed over, so enrol again.
	case http.StatusForbidden:
		return material{}, exitcodes.Wrap(exitcodes.EnrollRefused, refusal(response))

	// The peer identity was never established — no header, two headers, or a
	// verifier that would not believe the one there was. The exchange presents
	// no bearer, so a 401 here can be nothing else: it is the gateway being
	// misconfigured or a signature that has gone stale, neither of which this
	// node can do anything about and neither of which enrolling again would
	// change. Retryable, so the supervisor brings it back when whoever holds
	// the gateway has fixed it — the nonce is good for an hour.
	case http.StatusUnauthorized:
		return material{}, refusal(response)

	// The hour is up. The material existed and is gone, and there is nothing
	// left to be idempotent with.
	case http.StatusGone:
		return material{}, exitcodes.Wrap(exitcodes.MaterialExpired, refusal(response))

	case http.StatusUpgradeRequired:
		return material{}, exitcodes.Wrap(exitcodes.CapverTooOld, capverTooOld(response))

	// This connector's own request: a malformed enrollment id, a missing
	// nonce, an unreadable body. Retrying sends the same thing again.
	case http.StatusUnprocessableEntity, http.StatusBadRequest:
		return material{}, exitcodes.Wrap(exitcodes.Usage, refusal(response))

	default:
		return material{}, refusal(response)
	}
}

func answer(body io.Reader) (material, error) {
	var answer material
	if err := json.NewDecoder(body).Decode(&answer); err != nil {
		return material{}, fmt.Errorf("enroll: reading the exchange's answer: %w", err)
	}

	// A 2xx naming no credential is worse than a refusal: the node would write
	// down an identity it cannot present and never enrol again.
	if answer.NodeID == "" || answer.NodeCredential == "" {
		return material{}, exitcodes.Wrap(exitcodes.Failure, fmt.Errorf(
			"enroll: the exchange succeeded but carried no node credential",
		))
	}

	return answer, nil
}

func capverTooOld(response *http.Response) error {
	var body struct {
		Floor int `json:"floor"`
	}
	_ = json.NewDecoder(io.LimitReader(response.Body, maxErrorBody)).Decode(&body)

	return fmt.Errorf(
		"enroll: this connector speaks node_capver %d and the mothership's floor is %d; upgrade it",
		version.NodeCapver, body.Floor,
	)
}

// refusal reads the mothership's one-word `error`, which every refusal on the
// overlay listener carries, and falls back to whatever did arrive.
func refusal(response *http.Response) error {
	raw, _ := io.ReadAll(io.LimitReader(response.Body, maxErrorBody))

	var body struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(raw, &body)

	return &HTTPError{StatusCode: response.StatusCode, Reason: body.Error, Body: string(raw)}
}
