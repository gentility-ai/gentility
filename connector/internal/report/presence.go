// Package report is the node saying what it is and where it can be reached.
//
// One endpoint, one ephemeral row on the other end.
// It is a **measurement and not a fact**: the
// mothership stamps its own clock on arrival, overwrites the row in place, and
// keeps no history, because a ledger of "last seen" would stop being
// replayable and would grow without bound.
//
// Two things follow from that, and both are load-bearing here.
//
// **A heartbeat is total, not a patch.** Every column the node speaks for is
// written on every beat, so a field left out is recorded as absent rather than
// unchanged. That is why a beat this node cannot fully describe is skipped
// instead of sent half-filled.
//
// **Silence is not death.** A sprite pauses when idle and overlay traffic does
// not wake it, so heartbeats from a perfectly healthy case node simply stop.
// Nothing here tries to prevent that; telling suspended from isolated is the
// mothership's job and it uses what it knows about the runtime, never an
// inference from silence.
package report

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/gentility-ai/gentility/connector/internal/tsnode"
	"github.com/gentility-ai/gentility/connector/internal/version"
)

// Path is the presence endpoint.
const Path = "/overlay/self/presence"

// DefaultInterval is how often a node beats.
const DefaultInterval = 30 * time.Second

// beatTimeout bounds one beat. A heartbeat that has not landed inside this is
// a heartbeat the next one supersedes.
const beatTimeout = 10 * time.Second

// readyPoll is how often the loop asks again while the node holds no config
// document. Short, because the answer usually arrives within a second of the
// exchange and the first beat is what makes the node visible.
const readyPoll = 250 * time.Millisecond

// Status is what a node says about itself in a presence report.
// Version and Capver are the binary's. Network observations are additive at
// capver 1; an older sender omits them and an older server ignores them.
type Status struct {
	NetworkReportVersion int                   `json:"network_report_version,omitempty"`
	PeerPaths            []tsnode.PeerPath     `json:"peer_paths,omitempty"`
	LocalAddresses       []tsnode.LocalAddress `json:"local_addresses,omitempty"`
	HomeDERP             string                `json:"home_derp,omitempty"`
	Version              string                `json:"version"`
	Capver               int                   `json:"capver"`
	Endpoint             string                `json:"endpoint"`
	Relayed              bool                  `json:"relayed"`
	Addresses            []string              `json:"addresses"`

	// Services are the listeners this node declares. Omitted when there are
	// none: the heartbeat is total, and an absent key is how the mothership
	// reads "this node advertises nothing" without a node having to send an
	// empty list to say it.
	Services []Service `json:"services,omitempty"`
}

// Service is one listener a node advertises, in exactly the three keys the
// mothership's presence handler reads (the overlay router's
// advertised_services/1).
//
// **The parser is all-or-nothing**: one entry it will not accept and the whole
// heartbeat is refused, so a node that advertised a kind it is not allowed to
// declare would stop being seen at all — every thirty seconds, silently. A node
// therefore advertises only kinds it is permitted to declare for itself, which
// today is `egress`.
//
// Declaring is not being approved. The mothership records a declared service; a
// person approves it; the approval comes back in a config document, and that
// document is the only thing that makes this node serve anything.
type Service struct {
	Kind string `json:"kind"`
	Port int    `json:"port"`
	// Destination is for the kinds that name somewhere else. An egress relay
	// names none — it is its own destination — so it is omitted.
	Destination string `json:"destination,omitempty"`
}

// Reporter beats presence for one node until its context is cancelled.
type Reporter struct {
	BaseURL    string
	Credential string
	HTTP       *http.Client

	// Status reads the node's current connectivity — tsnode.Node.Status, in
	// the connector. Version and Capver are filled in here, so a caller
	// cannot report a version this binary is not.
	Status func(context.Context) (Status, error)

	// Ready gates the whole loop: a node serves nothing until it holds a
	// config document, and a heartbeat is the node saying what
	// it is serving. config.Client.Ready, in the connector.
	Ready func() bool

	// Interval is how often to beat. Zero means DefaultInterval.
	Interval time.Duration

	// Ticks replaces the clock. Nil means a real ticker; a test drives this by
	// hand rather than waiting thirty seconds to see a second beat.
	Ticks <-chan time.Time

	// Log is where a failed beat goes. Nil is silence.
	Log func(format string, args ...any)

	// Beaten hears how each beat went: nil when the mothership took it. Nil
	// is nobody listening. A beat cut short by the node stopping is not told.
	Beaten func(err error)
}

// Run beats until ctx is cancelled, and returns ctx.Err() when it is.
//
// **It never ends the process.** Every failure — a refused credential
// included — is logged and beaten past. Presence is a report, and the config
// pull is this node's lifeline: it sees the same refusals, exits with a code
// that says which one, and two loops racing to end the process would make the
// code it exits with a matter of scheduling.
func (r *Reporter) Run(ctx context.Context) error {
	ticks := r.Ticks
	if ticks == nil {
		interval := r.Interval
		if interval <= 0 {
			interval = DefaultInterval
		}
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		ticks = ticker.C
	}

	// The first beat that lands is worth one line: it is the last of the four
	// steps a join goes through, and the one that says the mothership can see
	// this node. After that, only failures are news.
	beaten := false

	for {
		// A node that is not ready yet has nothing to say — a heartbeat is the
		// node saying what it is serving — so it comes back SOON rather than
		// on the next tick. A tick-driven wait here is why a node that took
		// two seconds to get its document then waited out the rest of the
		// interval before the mothership saw it at all.
		if r.Ready != nil && !r.Ready() {
			if err := pause(ctx, readyPoll); err != nil {
				return err
			}
			continue
		}

		err := r.Beat(ctx)
		if r.Beaten != nil && ctx.Err() == nil {
			r.Beaten(err)
		}
		switch {
		case err != nil && ctx.Err() == nil:
			r.logf("%v", err)
		case err == nil && !beaten:
			beaten = true
			r.logf("presence ok")
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticks:
		}
	}
}

// pause waits, or gives up because the node is stopping.
func pause(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// Beat sends one heartbeat.
func (r *Reporter) Beat(ctx context.Context) error {
	status, err := r.Status(ctx)
	if err != nil {
		// Nothing is sent: see the package doc — half a snapshot is worse than
		// none, because the row records what a beat leaves out as absent.
		return fmt.Errorf("report: this node cannot say what it is: %w", err)
	}
	status.Version = version.Connector
	status.Capver = version.NodeCapver

	body, err := json.Marshal(status)
	if err != nil {
		return fmt.Errorf("report: encoding the heartbeat: %w", err)
	}

	ctx, cancel := context.WithTimeout(ctx, beatTimeout)
	defer cancel()

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, r.BaseURL+Path, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("report: building the heartbeat: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+r.Credential)
	request.Header.Set("X-Node-Capver", strconv.Itoa(version.NodeCapver))

	client := r.HTTP
	if client == nil {
		client = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}

	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("report: beating at %s: %w", r.BaseURL+Path, err)
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusNoContent {
		return fmt.Errorf("report: the mothership answered %d to a heartbeat", response.StatusCode)
	}

	return nil
}

func (r *Reporter) logf(format string, args ...any) {
	if r.Log != nil {
		r.Log(format, args...)
	}
}
