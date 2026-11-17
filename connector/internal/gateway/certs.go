package gateway

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"sync"
	"time"
)

// CertPath is the internal endpoint that issues the gateway's leaf
// (the internal certificate endpoint). It is reached over the deploy's
// docker network and never over the overlay: the leaf's private key must not
// travel on the network the leaf protects.
const CertPath = "/internal/gateway/cert"

const (
	// fetchTimeout bounds one issue. The app generates a P-256 key and signs
	// one certificate; there is no long poll here.
	fetchTimeout = 30 * time.Second

	// checkEvery is how often Run asks whether the certificate in hand is
	// still the right one.
	//
	// Short, because one of the two questions it asks is short-lived: an
	// organization added after boot is an address this gateway starts
	// answering on within a second (listen.go's reconcile), and until the leaf
	// names that address every node in that organization fails the handshake.
	// The other question — is it time to renew — would be happy with a day.
	//
	// Asking is free: it reads the local netmap and compares two small lists.
	// Acting is not, which is what the two waits below are for.
	checkEvery = 5 * time.Second

	// renewRetry is the wait after a fetch that did not work.
	//
	// A flat interval rather than a backoff: renewal starts at two thirds of a
	// ninety-day life, so there are thirty days of headroom and nothing to be
	// gained by widening the gap. What matters is that the attempt keeps
	// happening and says why it failed each time.
	renewRetry = 5 * time.Minute

	// renewFloor is the shortest gap between two fetches that worked, and it
	// is what stops a flapping netmap — or a leaf that arrives already past
	// its own renewal point — from spinning against the issuing endpoint at
	// checkEvery.
	renewFloor = time.Minute
)

// RefusedError is the app declining to issue.
//
// Reason is the mothership's own `error`, and **only** if it is one this
// package already knows the spelling of (see reasons). The response body is
// otherwise not carried at all, unlike the node's other clients, which quote a
// couple of kilobytes of whatever arrived so a proxy's complaint is
// recognisable.
//
// This client is the one that holds a bearer, and what comes back is decided
// by the far end. An error value that echoed it verbatim is an error value
// something upstream could fill with the request's own Authorization header
// and let this process print — which is how a secret ends up in a log without
// anybody writing a line that logs one. So nothing from the far end is ever
// printed: what is printed is a constant from this file that the far end got
// to choose between.
type RefusedError struct {
	StatusCode int
	Reason     string
}

func (e *RefusedError) Error() string {
	if e.Reason == "" {
		return fmt.Sprintf("gateway: the mothership refused to issue a certificate: %d", e.StatusCode)
	}
	return fmt.Sprintf("gateway: the mothership refused to issue a certificate: %d %s", e.StatusCode, e.Reason)
}

// Leaf is one issued certificate and the window it is good for.
type Leaf struct {
	Certificate tls.Certificate
	NotBefore   time.Time
	NotAfter    time.Time
}

// renewAt is two thirds of the way through this leaf's life — sixty days into
// a ninety-day one.
//
// Renewal is the only thing that proves the issuing path still works, which is
// why the leaf is short and why this point is well before the cliff: thirty
// days is enough for every retry, every deploy and somebody's holiday.
func (l Leaf) renewAt() time.Time {
	life := l.NotAfter.Sub(l.NotBefore)
	if life <= 0 {
		return l.NotBefore
	}

	return l.NotBefore.Add(life * 2 / 3)
}

// covers says whether every address in addresses is an iPAddress SAN on this
// leaf.
//
// The iPAddress SANs and not the dNSName: a node dials its gateway by address
// literal (`enroll.Gateway` builds `https://[fd06:6e7:…]:443`), so the name being
// verified at every handshake is the address, and a leaf that does not carry
// one is a leaf that refuses that organization.
//
// One direction only. A leaf naming an address the gateway has since lost is
// not worth a fetch: it is a SAN for a listener that no longer exists, and it
// will go away at the next renewal anyway.
func (l Leaf) covers(addresses []netip.Addr) bool {
	if l.Certificate.Leaf == nil {
		// readLeaf parses every certificate it installs, so this is
		// unreachable. Answering "covered" is the safe unreachable branch: the
		// alternative is a fetch every renewFloor, forever, over a question
		// that cannot be asked.
		return true
	}

	named := make([]netip.Addr, 0, len(l.Certificate.Leaf.IPAddresses))
	for _, ip := range l.Certificate.Leaf.IPAddresses {
		// Unmap because x509 keeps a net.IP, which spells a v4 address as a
		// v4-in-v6 sixteen bytes about half the time. Overlay addresses are v6
		// either way; this is so the comparison cannot depend on that.
		if address, ok := netip.AddrFromSlice(ip); ok {
			named = append(named, address.Unmap())
		}
	}

	for _, address := range addresses {
		if !slices.Contains(named, address.Unmap()) {
			return false
		}
	}

	return true
}

// Certs holds the gateway's current TLS leaf and knows how to get another.
//
// Nothing about a leaf is persisted, here or on the app: a fresh key is
// generated per issue and a restart simply asks again. So there is no file to
// protect, no key on disk to rotate, and a compromised gateway container loses
// a certificate that expires rather than one that has to be revoked.
type Certs struct {
	// Now is the clock. Exported for the tests.
	Now func() time.Time
	// Log is where a renewal says what it could not do. Nil is silence. It is
	// never given the bearer, the key or the certificate to print.
	Log func(string, ...any)

	// CheckEvery, RenewFloor and RenewRetry are the three waits in Run.
	// Exported for the tests, which would otherwise spend minutes per case
	// proving the loop turns — the same reason config.Client.MinPoll is
	// exported.
	CheckEvery time.Duration
	RenewFloor time.Duration
	RenewRetry time.Duration

	upstream *url.URL
	secret   []byte
	http     *http.Client

	mu      sync.RWMutex
	current *Leaf
}

// NewCerts builds the client that fetches the gateway's leaf from upstream.
//
// httpClient is the plain docker-network client; a nil one gets a default with
// no timeout of its own, because each fetch bounds itself on the context.
func NewCerts(upstream *url.URL, secret []byte, httpClient *http.Client, log func(string, ...any)) *Certs {
	if httpClient == nil {
		httpClient = &http.Client{}
	}

	return &Certs{
		Now:        time.Now,
		Log:        log,
		CheckEvery: checkEvery,
		RenewFloor: renewFloor,
		RenewRetry: renewRetry,
		upstream:   upstream,
		secret:     secret,
		http:       httpClient,
	}
}

// GetCertificate is `tls.Config.GetCertificate`: it answers whatever leaf is
// current at the moment of the handshake.
//
// This indirection is the whole of the renewal story. A listener never
// restarts and a connection in flight keeps the certificate it started with;
// the next handshake gets the new one.
//
// No certificate is a refused handshake, not a handshake with none. A gateway
// that answered plaintext, or with a certificate it had made up, would be
// worse than one that is briefly unreachable.
func (c *Certs) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	if c.current == nil {
		return nil, errors.New("gateway: no certificate has been issued yet")
	}

	return &c.current.Certificate, nil
}

// Current is the leaf in hand, or nil.
func (c *Certs) Current() *Leaf {
	c.mu.RLock()
	defer c.mu.RUnlock()

	return c.current
}

// Fetch asks for a leaf covering addresses and installs it.
//
// addresses are the gateway's own overlay addresses, one per organization it
// holds a grant in, and they become the `iPAddress` SANs — which are the SANs
// that decide anything, because a node dials its gateway by address literal
// and so verifies the address, never the dNSName.
//
// The hostname is NOT the caller's to name: the app issues for its own
// `platform_hostname`, so a bearer-holder cannot have the internal CA sign a
// certificate for a name of its choosing. It is a label on the subject, not
// something a handshake turns on.
func (c *Certs) Fetch(ctx context.Context, addresses []netip.Addr) (*Leaf, error) {
	leaf, err := c.issue(ctx, addresses)
	if err != nil {
		return nil, err
	}

	c.mu.Lock()
	c.current = leaf
	c.mu.Unlock()

	return leaf, nil
}

// Run keeps a certificate that covers this gateway's addresses in hand until
// ctx is done.
//
// Two reasons to fetch, and the second is the one with a deadline on it:
//
//   - the leaf is two thirds through its life, which is renewal;
//   - the node holds an address the leaf does not name, which is an
//     organization added after boot. That address is bound and answering
//     within a second, and every node in that organization fails the handshake
//     until the SAN exists — so waiting sixty days for a renewal to carry it
//     would be an organization enrolled and unreachable.
//
// addresses is therefore read every checkEvery rather than at each fetch, and
// what it answers is compared against the SANs actually in hand. The two waits
// keep that from becoming a hammer: nothing is fetched within RenewFloor of a
// fetch that worked, or within RenewRetry of one that did not, so a netmap
// flapping an address in and out costs one request a minute rather than one
// per flap.
//
// It expects a leaf to have been fetched already: `connector gateway` fetches
// once at boot and treats a refusal there as fatal, because a gateway with no
// certificate serves nobody and saying so is more use than retrying quietly.
func (c *Certs) Run(ctx context.Context, addresses func(context.Context) ([]netip.Addr, error)) error {
	// The earliest the next fetch may happen. Zero is "now".
	var next time.Time

	for {
		if err := sleep(ctx, c.CheckEvery); err != nil {
			return err
		}

		if c.Now().Before(next) {
			continue
		}

		held, err := addresses(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}

			c.logf("gateway: reading this node's addresses: %v; trying again in %s", err, c.RenewRetry)
			next = c.Now().Add(c.RenewRetry)
			continue
		}

		why := c.due(held)
		if why == "" {
			continue
		}

		leaf, err := c.Fetch(ctx, held)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}

			c.logf("gateway: fetching a certificate (%s): %v; trying again in %s", why, err, c.RenewRetry)
			next = c.Now().Add(c.RenewRetry)
			continue
		}

		c.logf("gateway: certificate issued (%s), good until %s", why, leaf.NotAfter.Format(time.RFC3339))
		next = c.Now().Add(c.RenewFloor)
	}
}

// due says why the gateway wants another certificate, or "" if it does not.
//
// The reason is a constant from this file and it goes in the log line, so a
// fetch at an unexpected moment says which of the two questions asked for it.
func (c *Certs) due(held []netip.Addr) string {
	leaf := c.Current()

	switch {
	case leaf == nil:
		return "there is none"
	case !c.Now().Before(leaf.renewAt()):
		return "renewal"
	case !leaf.covers(held):
		return "an address it does not name"
	default:
		return ""
	}
}

func sleep(ctx context.Context, wait time.Duration) error {
	timer := time.NewTimer(wait)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// certRequest is the body the internal certificate endpoint reads: a list
// of addresses and nothing else.
type certRequest struct {
	Addresses []string `json:"addresses"`
}

// certResponse is what it answers on 200. The private key exists here, in the
// gateway's memory, and nowhere else — not on disk, not in a config document,
// not in a log line.
type certResponse struct {
	CertificatePEM string    `json:"certificate_pem"`
	PrivateKeyPEM  string    `json:"private_key_pem"`
	CAPEM          string    `json:"ca_pem"`
	NotBefore      time.Time `json:"not_before"`
	NotAfter       time.Time `json:"not_after"`
}

func (c *Certs) issue(ctx context.Context, addresses []netip.Addr) (*Leaf, error) {
	ctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()

	named := make([]string, 0, len(addresses))
	for _, address := range addresses {
		named = append(named, address.String())
	}

	body, err := json.Marshal(certRequest{Addresses: named})
	if err != nil {
		return nil, fmt.Errorf("gateway: encoding the certificate request: %w", err)
	}

	bearer, err := Bearer(c.secret, c.Now().Unix())
	if err != nil {
		return nil, err
	}

	endpoint := c.upstream.JoinPath(CertPath).String()

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("gateway: building the certificate request: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+bearer)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")

	response, err := c.http.Do(request)
	if err != nil {
		// The endpoint named rather than the request: the URL is a container
		// alias and a port, and the bearer is in a header that is not printed.
		return nil, fmt.Errorf("gateway: asking %s for a certificate: %w", endpoint, err)
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		return nil, refused(response)
	}

	return readLeaf(response.Body)
}

// refused reads the mothership's one-word `error` and nothing else. See
// RefusedError.
func refused(response *http.Response) error {
	var body struct {
		Error string `json:"error"`
	}
	_ = json.NewDecoder(io.LimitReader(response.Body, 2048)).Decode(&body)

	return &RefusedError{StatusCode: response.StatusCode, Reason: known(body.Error)}
}

// reasons is every `error` the internal certificate endpoint answers with,
// written out.
//
// A fixed set and not a shape. A filter that admitted anything spelled like a
// reason would still be printing bytes the far end chose; this prints one of
// six strings that live in this file, and the far end only gets to say which.
var reasons = []string{
	// The four this gateway can act on.
	"invalid_bearer",
	"invalid_address",
	"no_addresses",
	"overlay_peer_on_internal_surface",

	// The two the router answers for anything else.
	"invalid_request",
	"internal_error",
}

// known keeps a refusal reason only if it is one of ours, and drops it
// otherwise. Dropping loses nothing that matters: the status code is still
// there, and a reason nobody wrote down is not a reason.
func known(reason string) string {
	if slices.Contains(reasons, reason) {
		return reason
	}

	return ""
}

func readLeaf(body io.Reader) (*Leaf, error) {
	var answer certResponse
	if err := json.NewDecoder(body).Decode(&answer); err != nil {
		return nil, fmt.Errorf("gateway: reading the issued certificate: %w", err)
	}

	certificate, err := tls.X509KeyPair([]byte(answer.CertificatePEM), []byte(answer.PrivateKeyPEM))
	if err != nil {
		// The parse error, never the material: a PEM that would not parse is
		// still a private key.
		return nil, errors.New("gateway: the issued certificate and key are not a usable pair")
	}

	// Parsed here and kept, so the SANs are readable without re-parsing at
	// every turn of the loop that checks them. tls.X509KeyPair already parsed
	// this DER to match it against the key; this is the copy that is kept.
	certificate.Leaf, err = x509.ParseCertificate(certificate.Certificate[0])
	if err != nil {
		return nil, fmt.Errorf("gateway: the issued certificate would not parse: %w", err)
	}

	if err := chainsTo(certificate.Leaf, answer.CAPEM); err != nil {
		return nil, err
	}

	if answer.NotAfter.IsZero() {
		return nil, errors.New("gateway: the issued certificate carries no expiry, so there is nothing to renew against")
	}

	return &Leaf{Certificate: certificate, NotBefore: answer.NotBefore, NotAfter: answer.NotAfter}, nil
}

// chainsTo refuses a leaf that does not verify against the `ca_pem` it arrived
// with.
//
// Every node is told to trust exactly that root — it is what
// `enroll.Gateway` pins the config document's ca_pem to — so a leaf that does
// not chain to it is a certificate no node will accept, and installing it is
// installing an outage that only shows up at the far end. Better to refuse the
// fetch here, where the gateway keeps serving the leaf it already has and says
// why the new one was no good.
func chainsTo(leaf *x509.Certificate, caPEM string) error {
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM([]byte(caPEM)) {
		return errors.New("gateway: the issued certificate came with no ca_pem to check it against")
	}

	if _, err := leaf.Verify(x509.VerifyOptions{
		Roots:     roots,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}); err != nil {
		return fmt.Errorf("gateway: the issued certificate does not chain to the ca_pem it came with: %w", err)
	}

	return nil
}

func (c *Certs) logf(format string, args ...any) {
	if c.Log != nil {
		c.Log(format, args...)
	}
}
