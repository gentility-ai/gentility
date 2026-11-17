package traffic

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/gentility-ai/gentility/connector/internal/tsnode"
	"github.com/gentility-ai/gentility/connector/internal/version"
)

const (
	// Path is the traffic report endpoint.
	Path = "/overlay/self/traffic"
	// DefaultInterval is how often a node sends until an answer says otherwise.
	DefaultInterval = 10 * time.Second
	// MinInterval and MaxInterval bound what an answer may ask for.
	MinInterval = time.Second
	MaxInterval = 30 * time.Second
	// NotHereInterval is the wait after a 404: a mothership without the route
	// is asked again ten minutes later, not every ten seconds.
	NotHereInterval = 10 * time.Minute
	// DefaultMaxBody keeps a batch under the mothership's 512 KB cap with
	// room to spare; what does not fit goes next time.
	DefaultMaxBody = 448 << 10

	sendTimeout = 10 * time.Second
	readyPoll   = 250 * time.Millisecond
	// maxLead is how many seconds a label may run ahead of the wall clock.
	// A late tick puts it one or two ahead; more means the clock stepped
	// back, and the labels would otherwise stay that far ahead for good.
	maxLead = 5
)

// Reporter samples this node's peer counters once a second and sends them,
// with the node's own overlay credential, at the interval the mothership's
// last answer named. An idle node still sends a batch — no peers, but the
// seconds it covered — so it hears when somebody starts watching and its
// quiet links read as idle rather than as gaps. A batch the mothership did not take (a 429
// included) stays in the ring for the next; one it never will (413, 422) is dropped and logged.
// It never ends the process: like presence, it is a report.
type Reporter struct {
	BaseURL    string
	Credential string
	HTTP       *http.Client

	// Read is tsnode.Node.PeerCounters in the connector.
	Read func(context.Context) ([]tsnode.PeerCounter, error)
	// Ready gates sending, as it gates presence: config.Client.Ready.
	Ready func() bool

	// SampleTicks replaces the one-second ticker; After replaces time.After;
	// Now replaces the clock. Tests drive all three by hand.
	SampleTicks <-chan time.Time
	After       func(time.Duration) <-chan time.Time
	Now         func() time.Time

	// MaxBody is the largest body sent, before compression. Zero means
	// DefaultMaxBody.
	MaxBody int

	Log func(format string, args ...any)

	ring     *Ring
	deltas   *Deltas
	reported bool
	failing  bool
	primed   bool
	// second is the newest second sampled; the next is always later.
	second int64
}

type outcome struct {
	batch  Batch
	status int
	next   time.Duration
	err    error
}

// Run samples and sends until ctx ends, and returns ctx.Err() when it does.
// Sending happens on its own goroutine so a slow mothership never stops the
// sampling; the ring and the deltas are only ever touched on this loop.
func (r *Reporter) Run(ctx context.Context) error {
	ticks := r.SampleTicks
	if ticks == nil {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		ticks = ticker.C
	}
	r.ring, r.deltas = NewRing(), NewDeltas()
	interval := DefaultInterval
	due := r.after(interval)
	results := make(chan outcome, 1)

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()

		case <-ticks:
			r.sample(ctx)

		case <-due:
			due = nil
			if r.Ready != nil && !r.Ready() {
				due = r.after(readyPoll)
				continue
			}
			batch, body, err := r.fit()
			if err != nil {
				r.logf("traffic: %v", err)
				due = r.after(interval)
				continue
			}
			go func() { results <- r.send(ctx, batch, body) }()

		case result := <-results:
			var wait time.Duration
			wait, interval = r.settle(result, interval)
			due = r.after(wait)
		}
	}
}

// sample reads the counters once and files this second's deltas, under the
// second the read began in — or, when the clock stood still or stepped back,
// the second after the last one, so no two samples share a second and the
// ring never holds more than MaxSeconds of them.
func (r *Reporter) sample(ctx context.Context) {
	now := r.now()
	counters, err := r.Read(ctx)
	if err != nil {
		if !r.failing {
			r.logf("traffic: cannot read peer counters: %v", err)
		}
		r.failing = true
		return
	}
	r.failing = false
	r.second = max(now.Unix(), r.second+1)
	if lead := r.second - now.Unix(); lead > maxLead {
		// The clock stepped back. Start again from it: a fresh ring, so no
		// label it holds runs ahead, and this reading the new baseline. What
		// the old ring held was never delivered, and reads as a gap.
		r.logf("traffic: the wall clock stepped back about %ds; restarted the traffic ring", lead)
		r.ring, r.deltas, r.primed = NewRing(), NewDeltas(), false
		r.second = now.Unix()
	}
	moved := r.deltas.Next(now, counters)
	if !r.primed {
		// The first reading is every counter's baseline: it says nothing
		// about its own second, so that second is not covered.
		r.primed = true
		return
	}
	handshakes := make(map[string]int64, len(counters))
	for _, c := range counters {
		if !c.LastHandshake.IsZero() {
			handshakes[c.Address] = c.LastHandshake.Unix()
		}
	}
	r.ring.Add(r.second, moved, handshakes)
}

// fit is the oldest part of the ring that encodes within MaxBody: a gateway
// cut off for two minutes can hold more than the mothership reads at once.
func (r *Reporter) fit() (Batch, []byte, error) {
	limit := r.ring.Seconds()
	for {
		batch := r.ring.Batch(limit)
		body, err := json.Marshal(batch)
		if err != nil {
			return Batch{}, nil, fmt.Errorf("encoding the batch: %w", err)
		}
		if len(body) <= r.maxBody() || limit <= 1 {
			return batch, body, nil
		}
		limit /= 2
	}
}

func (r *Reporter) send(ctx context.Context, batch Batch, body []byte) outcome {
	var zipped bytes.Buffer
	zw := gzip.NewWriter(&zipped)
	if _, err := zw.Write(body); err != nil {
		return outcome{batch: batch, err: fmt.Errorf("compressing the batch: %w", err)}
	}
	if err := zw.Close(); err != nil {
		return outcome{batch: batch, err: fmt.Errorf("compressing the batch: %w", err)}
	}

	ctx, cancel := context.WithTimeout(ctx, sendTimeout)
	defer cancel()

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, r.BaseURL+Path, &zipped)
	if err != nil {
		return outcome{batch: batch, err: fmt.Errorf("building the request: %w", err)}
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Content-Encoding", "gzip")
	request.Header.Set("Authorization", "Bearer "+r.Credential)
	request.Header.Set("X-Node-Capver", strconv.Itoa(version.NodeCapver))

	client := r.HTTP
	if client == nil {
		client = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	response, err := client.Do(request)
	if err != nil {
		return outcome{batch: batch, err: fmt.Errorf("posting to %s: %w", r.BaseURL+Path, err)}
	}
	defer func() {
		// Read to the end, so the connection carries the next send.
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
		response.Body.Close()
	}()

	result := outcome{batch: batch, status: response.StatusCode}
	if response.StatusCode == http.StatusOK {
		var answer struct {
			NextIntervalMs int64 `json:"next_interval_ms"`
		}
		if err := json.NewDecoder(io.LimitReader(response.Body, 4096)).Decode(&answer); err != nil {
			result.err = fmt.Errorf("reading the answer: %w", err)
			return result
		}
		result.next = clamp(time.Duration(answer.NextIntervalMs) * time.Millisecond)
	}
	return result
}

// settle applies one send's outcome to the ring and answers the wait before
// the next send and the interval after that. A 404's ten minutes are one
// wait, never the interval: what follows it waits as before.
func (r *Reporter) settle(result outcome, interval time.Duration) (wait, next time.Duration) {
	switch {
	case result.status == http.StatusOK && result.err != nil:
		// Taken; only the answer was unreadable.
		r.ring.Ack(result.batch)
		r.logf("traffic: %v; keeping the %s interval", result.err, interval)
		return interval, interval
	case result.err != nil:
		r.logf("traffic: %v; kept %d second(s) for the next", result.err, r.ring.Seconds())
		return interval, interval
	case result.status == http.StatusOK:
		r.ring.Ack(result.batch)
		if !r.reported {
			r.reported = true
			r.logf("traffic ok")
		}
		return result.next, result.next
	case result.status == http.StatusNotFound:
		r.logf("traffic: the mothership has no %s; asking again in %s", Path, NotHereInterval)
		return NotHereInterval, interval
	case result.status == http.StatusRequestEntityTooLarge, result.status == http.StatusUnprocessableEntity:
		r.ring.Ack(result.batch)
		r.logf("traffic: the mothership answered %d; dropped %d peer row(s) it will never take", result.status, len(result.batch.Peers))
		return interval, interval
	default:
		r.logf("traffic: the mothership answered %d; kept %d second(s) for the next", result.status, r.ring.Seconds())
		return interval, interval
	}
}

func clamp(d time.Duration) time.Duration {
	switch {
	case d == 0:
		return DefaultInterval
	case d < MinInterval:
		return MinInterval
	case d > MaxInterval:
		return MaxInterval
	default:
		return d
	}
}

func (r *Reporter) after(d time.Duration) <-chan time.Time {
	if r.After != nil {
		return r.After(d)
	}
	return time.After(d)
}

func (r *Reporter) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *Reporter) maxBody() int {
	if r.MaxBody > 0 {
		return r.MaxBody
	}
	return DefaultMaxBody
}

func (r *Reporter) logf(format string, args ...any) {
	if r.Log != nil {
		r.Log(format, args...)
	}
}
