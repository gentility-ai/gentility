package config

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/gentility-ai/gentility/connector/internal/exitcodes"
	"github.com/gentility-ai/gentility/connector/internal/state"
	"github.com/gentility-ai/gentility/connector/internal/version"
)

// Path is the node's config endpoint.
const Path = "/overlay/self/config"

const (
	// pullTimeout bounds one request. The mothership holds a poll open for up
	// to sixty seconds, so anything near that is a normal answer and only a
	// margin above it is a hung connection.
	pullTimeout = 90 * time.Second

	// defaultMinPoll is the floor between two pulls. The long poll is what
	// paces this loop, and this is only what keeps a mothership answering
	// instantly — a misconfigured window, a proxy that will not hold a
	// connection — from turning the loop into a spin.
	defaultMinPoll = time.Second

	// maxErrorBody caps how much of a refusal is quoted back. Enough to
	// recognise a proxy's complaint, not enough to fill a log.
	maxErrorBody = 2048
)

// ErrUnchanged is the mothership's 304: `since` already names the current
// revision. Not a failure — the poll's window closed with nothing to say.
var ErrUnchanged = errors.New("config: unchanged")

// ErrNotRendered is the mothership's 409: nothing has been rendered for this
// node yet. Also not a failure — the render is queued, and a node serves
// nothing until it holds a document.
var ErrNotRendered = errors.New("config: nothing rendered for this node yet")

// CapverTooOldError is a 426: this binary speaks a node_capver below the
// mothership's floor. The floor is in the body because it is the one refusal
// the node can act on.
type CapverTooOldError struct {
	Declared int
	Floor    int
}

func (e *CapverTooOldError) Error() string {
	return fmt.Sprintf(
		"config: this connector speaks node_capver %d and the mothership's floor is %d; upgrade it",
		e.Declared, e.Floor,
	)
}

// UnknownRendererError is a document written by a renderer generation this
// binary does not know. It is refused rather than partly read, and
// only a new binary reads it — which is why it exits the way a capver floor
// does.
type UnknownRendererError struct {
	Version int
}

func (e *UnknownRendererError) Error() string {
	return fmt.Sprintf(
		"config: the document is renderer_version %d and this connector reads %d; upgrade it",
		e.Version, RendererVersion,
	)
}

// HTTPError is a status this package has no other name for. Reason is the
// mothership's own one-word `error`, which every refusal on the overlay
// listener carries, and Body is what came back when there was no JSON to read.
type HTTPError struct {
	StatusCode int
	Reason     string
	Body       string
}

func (e *HTTPError) Error() string {
	switch {
	case e.Reason != "":
		return fmt.Sprintf("config: the mothership answered %d %s", e.StatusCode, e.Reason)
	case e.Body != "":
		return fmt.Sprintf("config: the mothership answered %d: %s", e.StatusCode, e.Body)
	default:
		return fmt.Sprintf("config: the mothership answered %d", e.StatusCode)
	}
}

// Client pulls this node's config document and remembers how far it has got.
//
// One Client per node. It is safe for concurrent readers — Ready and Revision
// are what the rest of the connector asks before it serves anything — while
// Run is the single writer.
type Client struct {
	// OnUnchanged retries optional local work after a successful unchanged poll.
	OnUnchanged func()

	base       string
	credential string
	store      *state.Store
	http       *http.Client

	// MinPoll is the floor between two pulls. Exported for the tests, which
	// would otherwise spend a second per iteration proving the loop iterates.
	MinPoll time.Duration

	// Log is where this client says what it could not do about a refusal it
	// intends to retry. Nil is silence, which is what a test wants.
	Log func(format string, args ...any)

	mu       sync.Mutex
	revision int64
	ready    bool
	retry    backoff
}

// New builds a client for one node.
//
// httpClient is the connector's overlay-dialling client — the transport whose
// dials are pinned to this node's source address in the gateway's organization
// (tsnode.DialInOrg). A nil one gets a plain client, which is what the tests
// and the loopback dev recipe use.
//
// **The cursor starts at NoRevision however far the state file says this node
// once got.** A config document is applied into memory — grants, listeners,
// the relay — so a restarted process holds none of it, and a `since` seeded
// from disk would ask for something newer than a document it does not have,
// be told 304, and never become Ready: no grants, no listeners, and a
// heartbeat loop waiting on a readiness that could not arrive. So a restart
// asks for the whole document and applies it, which is free — every apply is
// a full replace.
//
// node.LastRevision stays what it is: the record of what this node did apply,
// for `connector status` and for whoever is reading an incident. It is not a
// resume point and nothing here reads it as one.
func New(baseURL string, node state.Node, store *state.Store, httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: pullTimeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}

	return &Client{
		base:       baseURL,
		credential: node.NodeCredential,
		store:      store,
		http:       httpClient,
		MinPoll:    defaultMinPoll,
		revision:   state.NoRevision,
	}
}

// Ready reports whether this node holds a config document. **It serves nothing
// until this is true**.
func (c *Client) Ready() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ready
}

// Revision is the config revision this node has applied, or state.NoRevision.
func (c *Client) Revision() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.revision
}

// Pull asks once for a document newer than the one this node holds.
//
// It decodes and validates; it does not apply. The cursor moves in Run, after
// the document has actually been taken on — a cursor moved by the decode would
// mean a document the node failed to apply is never offered again.
func (c *Client) Pull(ctx context.Context) (*Document, error) {
	// The bound on one attempt, and it is the context's rather than the
	// client's: the connector's HTTP client dials through the overlay and has
	// no Timeout of its own, so without this a pull that never answers is a
	// loop that never turns again.
	ctx, cancel := context.WithTimeout(ctx, pullTimeout)
	defer cancel()

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+Path, nil)
	if err != nil {
		return nil, fmt.Errorf("config: building the request: %w", err)
	}

	// No cursor before the first document: an absent `since` asks for the
	// whole document, and `since=0` would be a claim to hold revision 0.
	if revision := c.Revision(); revision >= 0 {
		query := request.URL.Query()
		query.Set("since", strconv.FormatInt(revision, 10))
		request.URL.RawQuery = query.Encode()
	}

	request.Header.Set("Authorization", "Bearer "+c.credential)
	request.Header.Set("X-Node-Capver", strconv.Itoa(version.NodeCapver))
	request.Header.Set("Accept", "application/json")

	response, err := c.http.Do(request)
	if err != nil {
		// A transport failure is the overlay being unreachable, which is the
		// thing the backoff exists for.
		return nil, fmt.Errorf("config: pulling %s: %w", c.base+Path, err)
	}
	defer response.Body.Close()

	return decode(response)
}

func decode(response *http.Response) (*Document, error) {
	switch response.StatusCode {
	case http.StatusOK:
		return readDocument(response.Body)

	case http.StatusNotModified:
		return nil, ErrUnchanged

	// The node has no document at all. The server uses 409 for the same
	// shape for the head's World: "not the state you asked about", without
	// also meaning "and never will be".
	case http.StatusConflict:
		return nil, ErrNotRendered

	case http.StatusUpgradeRequired:
		return nil, exitcodes.Wrap(exitcodes.CapverTooOld, capverTooOld(response))

	// The address this request arrived from names nothing here: an address
	// outside the scheme, or one whose grant or node is no longer live. Only
	// enrolling again answers that.
	case http.StatusForbidden:
		return nil, exitcodes.Wrap(exitcodes.EnrollRefused, refusal(response))

	// A 401 is two different things wearing one status, and only one of them
	// is this node's fault — see fatal401.
	case http.StatusUnauthorized:
		return nil, unauthorized(response)

	// Every request this client sends carries the capver header, so a 400 is a
	// proxy rewriting it or a binary talking to something that is not the
	// overlay listener. Retrying re-sends the same header.
	case http.StatusBadRequest:
		return nil, exitcodes.Wrap(exitcodes.Failure, refusal(response))

	default:
		return nil, refusal(response)
	}
}

func readDocument(body io.Reader) (*Document, error) {
	var doc Document
	if err := json.NewDecoder(body).Decode(&doc); err != nil {
		return nil, fmt.Errorf("config: reading the document: %w", err)
	}

	// Before anything else is read off it: a version this binary does not know
	// may have moved the meaning of every field below.
	if doc.RendererVersion != RendererVersion {
		return nil, exitcodes.Wrap(
			exitcodes.CapverTooOld,
			&UnknownRendererError{Version: doc.RendererVersion},
		)
	}

	return &doc, nil
}

// credentialRefusals are the 401s that are about the bearer this node
// presented. Everything else the overlay listener answers 401 to is about the
// PEER — no X-Overlay-Peer, two of them, or a verifier that would not believe
// the one there was — and every one of those is the gateway being
// misconfigured or a signature that has gone stale, which a node cannot fix by
// enrolling again and which the next request may well not see.
//
// So the fatal list is named rather than derived. Reading it the other way
// round — retry unless the reason is one this file knows — would make a node
// whose credential was refused sit in a backoff loop forever instead of
// telling its supervisor to enrol it again.
var credentialRefusals = map[string]bool{
	"no_credential":  true,
	"bad_credential": true,
	"node_revoked":   true,
}

// unauthorized decides whether a 401 ends this process.
func unauthorized(response *http.Response) error {
	err := refusal(response)

	var refused *HTTPError
	if errors.As(err, &refused) && credentialRefusals[refused.Reason] {
		return exitcodes.Wrap(exitcodes.EnrollRefused, err)
	}

	return err
}

func capverTooOld(response *http.Response) error {
	var body struct {
		Floor int `json:"floor"`
	}
	_ = json.NewDecoder(io.LimitReader(response.Body, maxErrorBody)).Decode(&body)

	return &CapverTooOldError{Declared: version.NodeCapver, Floor: body.Floor}
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

// Run pulls, applies, and keeps pulling until ctx is cancelled or a fatal
// refusal arrives.
//
// apply is what the rest of the connector does with a document — bind
// listeners, set grants, start the relay. Its failure is retryable and leaves
// the cursor alone, because a document the node could not take on is one it
// still needs.
func (c *Client) Run(ctx context.Context, apply func(*Document) error) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}

		wait, err := c.once(ctx, apply)
		if _, fatal := exitcodes.Of(err); fatal {
			return err
		}

		// A retryable refusal is the one thing this loop can neither fix nor
		// report anywhere else: it comes back, waits, and comes back again. A
		// node whose gateway has stopped signing peer headers would otherwise
		// sit silent for as long as it takes somebody to notice.
		if err != nil && !errors.Is(err, ErrUnchanged) && !errors.Is(err, ErrNotRendered) &&
			ctx.Err() == nil {
			c.logf("error: %v; trying again in %s", err, wait)
		}

		if wait > 0 {
			timer := time.NewTimer(wait)
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-timer.C:
			}
		}
	}
}

// once is one turn of the loop: what it answers is how long to wait before the
// next one, and the error is for the caller to decide fatality on.
func (c *Client) once(ctx context.Context, apply func(*Document) error) (time.Duration, error) {
	doc, err := c.Pull(ctx)

	switch {
	case errors.Is(err, ErrUnchanged), errors.Is(err, ErrNotRendered):
		// The mothership answered; there is simply nothing new. Its own window
		// is the pacing and MinPoll is only the guard against one that is not.
		c.reset()
		if errors.Is(err, ErrUnchanged) && c.Ready() && c.OnUnchanged != nil {
			c.OnUnchanged()
		}
		return c.MinPoll, err

	case err != nil:
		return c.backoff(), err
	}

	// The document this node already holds, offered again. A mothership that
	// answers 200 to a `since` naming its own current revision is not one this
	// loop should spin against, and re-applying costs a listener reconcile for
	// nothing.
	if doc.Revision == c.Revision() {
		if c.Ready() && c.OnUnchanged != nil {
			c.OnUnchanged()
		}
		c.reset()
		return c.MinPoll, nil
	}

	if err := apply(doc); err != nil {
		return c.backoff(), fmt.Errorf("config: applying revision %d: %w", doc.Revision, err)
	}

	c.applied(doc.Revision)
	// Straight back for the next one: the request that follows carries the
	// revision just applied and is held open by the mothership.
	return 0, nil
}

// applied records a document the node has taken on: the revision is written
// down so a restart resumes rather than re-applying, and only then does the
// node say it is ready.
//
// The disk write comes first because Ready is what the rest of the connector
// waits on, and a node that announced itself ready before its cursor was
// durable would be one whose next restart quietly re-applied the document it
// had just told everybody it held.
//
// A cursor that could not be persisted is one that will be re-fetched after a
// restart, which costs a single document — not worth refusing a document the
// node has already applied over.
func (c *Client) applied(revision int64) {
	if err := c.store.AdvanceRevision(revision); err != nil {
		c.logf("config: could not record revision %d: %v", revision, err)
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	c.revision = revision
	c.ready = true
	c.retry.reset()
}

func (c *Client) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.retry.reset()
}

func (c *Client) backoff() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.retry.next()
}

func (c *Client) logf(format string, args ...any) {
	if c.Log != nil {
		c.Log(format, args...)
	}
}
