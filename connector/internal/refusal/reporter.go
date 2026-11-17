package refusal

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/gentility-ai/gentility/connector/internal/version"
)

// Path is the refusal report endpoint.
const Path = "/overlay/self/refusals"

// DefaultInterval is how often a node reports, at most.
const DefaultInterval = time.Minute

const sendTimeout = 10 * time.Second

// Reporter folds attempts into edges and reports them once a minute, with
// the node's own overlay credential, until its context ends.
//
// It owns the pending edges alone: attempts arrive on a channel, so nothing
// here needs a lock and nothing on the packet path waits on it. A report the
// mothership did not take is merged into the next one; a report it never
// will take (404: older than this endpoint; 422: a shape it refuses) is
// dropped and logged. It never ends the process — like presence, it is a
// report and not this node's lifeline.
type Reporter struct {
	BaseURL    string
	Credential string
	HTTP       *http.Client

	Attempts <-chan Attempt
	Interval time.Duration
	Ticks    <-chan time.Time
	Now      func() time.Time
	Log      func(format string, args ...any)
}

func (r *Reporter) Run(ctx context.Context) error {
	in := r.Attempts
	if in == nil {
		in = Attempts()
	}
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

	pend := newPending()
	logged := map[Edge]time.Time{}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case a := <-in:
			if a.Side == Rejected {
				r.rejected(a, logged)
				continue
			}
			pend.add(a, r.now())
		case <-ticks:
			r.forget(logged)
			r.flush(ctx, pend)
		}
	}
}

// rejected logs a peer's notice that it refused this node's SYN, once a
// minute per edge: a connection retransmits its SYN several times.
func (r *Reporter) rejected(a Attempt, logged map[Edge]time.Time) {
	e := Edge{Side: Rejected, Source: a.Src, Destination: a.Dst, Protocol: "tcp", Port: a.Port}
	now := r.now()
	if last, ok := logged[e]; ok && now.Sub(last) < time.Minute {
		return
	}
	logged[e] = now
	r.logf("refused by %s's rules: %s:%d", a.Dst, a.Dst, a.Port)
}

// forget drops rejections logged more than a minute ago, so the map holds
// only the last minute's edges.
func (r *Reporter) forget(logged map[Edge]time.Time) {
	now := r.now()
	for e, last := range logged {
		if now.Sub(last) >= time.Minute {
			delete(logged, e)
		}
	}
}

func (r *Reporter) flush(ctx context.Context, pend *pending) {
	pend.overflow = plus(pend.overflow, Lost())
	if pend.empty() {
		return
	}
	batch := pend.take()

	switch status, err := r.send(ctx, batch); {
	case err == nil && status == http.StatusNoContent:
		r.logf("reported %d refused connection(s), %d more lost to the buffer", len(batch.Edges), batch.Overflow)
		for _, s := range batch.Edges {
			r.logf("refused (%s): %s -> %s:%d, %d time(s)", s.Side, s.Source, s.Destination, s.Port, s.Count)
		}
	case err == nil && (status == http.StatusNotFound || status == http.StatusUnprocessableEntity):
		r.logf("the mothership answered %d to a refusal report: dropped %d edge(s)", status, len(batch.Edges))
	case err == nil:
		pend.merge(batch)
		r.logf("the mothership answered %d to a refusal report: kept %d edge(s) for the next", status, len(batch.Edges))
	default:
		pend.merge(batch)
		r.logf("could not report refusals: %v; kept %d edge(s) for the next", err, len(batch.Edges))
	}
}

func (r *Reporter) send(ctx context.Context, batch Batch) (int, error) {
	body, err := json.Marshal(batch)
	if err != nil {
		return 0, fmt.Errorf("encoding the report: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, sendTimeout)
	defer cancel()

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, r.BaseURL+Path, bytes.NewReader(body))
	if err != nil {
		return 0, fmt.Errorf("building the report: %w", err)
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
		return 0, fmt.Errorf("posting to %s: %w", r.BaseURL+Path, err)
	}
	defer response.Body.Close()
	return response.StatusCode, nil
}

func (r *Reporter) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *Reporter) logf(format string, args ...any) {
	if r.Log != nil {
		r.Log(format, args...)
	}
}
