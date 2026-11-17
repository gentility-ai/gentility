package report

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/gentility-ai/gentility/connector/internal/tsnode"
	"github.com/gentility-ai/gentility/connector/internal/version"
)

type recorder struct {
	mu       sync.Mutex
	requests []*http.Request
	bodies   []map[string]any
	status   int
}

func (r *recorder) ServeHTTP(w http.ResponseWriter, request *http.Request) {
	var body map[string]any
	_ = json.NewDecoder(request.Body).Decode(&body)

	r.mu.Lock()
	r.requests = append(r.requests, request.Clone(context.Background()))
	r.bodies = append(r.bodies, body)
	status := r.status
	r.mu.Unlock()

	if status == 0 {
		status = 204
	}
	w.WriteHeader(status)
}

func (r *recorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.requests)
}

func (r *recorder) first() (*http.Request, map[string]any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.requests[0], r.bodies[0]
}

func reporter(t *testing.T, sink *recorder, ticks <-chan time.Time) *Reporter {
	t.Helper()
	server := httptest.NewServer(sink)
	t.Cleanup(server.Close)

	return &Reporter{
		BaseURL:    server.URL,
		Credential: "cred-shhh",
		HTTP:       server.Client(),
		Ready:      func() bool { return true },
		Status: func(context.Context) (Status, error) {
			return Status{
				Endpoint:  "203.0.113.7:41641",
				Relayed:   false,
				Addresses: []string{"fd06:6e7:0:1::a"},
			}, nil
		},
		Ticks: ticks,
	}
}

func TestHeartbeatBeatsOnStartAndOnEveryTick(t *testing.T) {
	sink := &recorder{}
	ticks := make(chan time.Time)
	r := reporter(t, sink, ticks)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()

	// One on start, then one per tick: a node that has just been handed its
	// config should be visible now, not thirty seconds from now.
	waitForCount(t, sink, 1)
	ticks <- time.Now()
	waitForCount(t, sink, 2)
	ticks <- time.Now()
	waitForCount(t, sink, 3)

	cancel()
	<-done
}

func TestHeartbeatBodyCarriesVersionCapverEndpointRelayedAndAddresses(t *testing.T) {
	sink := &recorder{}
	r := reporter(t, sink, make(chan time.Time))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.Run(ctx)
	waitForCount(t, sink, 1)

	request, body := sink.first()

	if request.Method != http.MethodPost || request.URL.Path != Path {
		t.Fatalf("%s %s, want POST %s", request.Method, request.URL.Path, Path)
	}
	if want := "Bearer cred-shhh"; request.Header.Get("Authorization") != want {
		t.Fatalf("Authorization = %q, want %q", request.Header.Get("Authorization"), want)
	}
	if request.Header.Get("X-Node-Capver") != "1" {
		t.Fatalf("X-Node-Capver = %q, want 1", request.Header.Get("X-Node-Capver"))
	}

	if body["version"] != version.Connector {
		t.Fatalf("version = %v, want %q", body["version"], version.Connector)
	}
	if body["capver"] != float64(version.NodeCapver) {
		t.Fatalf("capver = %v, want %d", body["capver"], version.NodeCapver)
	}
	if body["endpoint"] != "203.0.113.7:41641" {
		t.Fatalf("endpoint = %v", body["endpoint"])
	}
	if body["relayed"] != false {
		t.Fatalf("relayed = %v, want false", body["relayed"])
	}
	addresses, ok := body["addresses"].([]any)
	if !ok || len(addresses) != 1 || addresses[0] != "fd06:6e7:0:1::a" {
		t.Fatalf("addresses = %v", body["addresses"])
	}
}

func TestHeartbeatCarriesNetworkObservations(t *testing.T) {
	sink := &recorder{}
	r := reporter(t, sink, make(chan time.Time))
	r.Status = func(context.Context) (Status, error) {
		return Status{NetworkReportVersion: 1, HomeDERP: "syd",
			PeerPaths:      []tsnode.PeerPath{{Address: "fd06:6e7:0:2::4", Path: "direct"}},
			LocalAddresses: []tsnode.LocalAddress{{Interface: "en0", Address: "192.168.1.42", Subnet: "192.168.1.0/24"}},
		}, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.Run(ctx)
	waitForCount(t, sink, 1)
	_, body := sink.first()
	if body["network_report_version"] != float64(1) || body["home_derp"] != "syd" {
		t.Fatalf("network report fields missing: %v", body)
	}
	paths := body["peer_paths"].([]any)
	locals := body["local_addresses"].([]any)
	if paths[0].(map[string]any)["path"] != "direct" || locals[0].(map[string]any)["subnet"] != "192.168.1.0/24" {
		t.Fatalf("observations lost in heartbeat: %v", body)
	}
}

// Presence is a report, not the node's lifeline. A mothership that is having a
// bad minute must not take a healthy node with it — the config loop is the one
// that decides whether this process lives.
func TestHeartbeatSurvivesAServerError(t *testing.T) {
	sink := &recorder{status: 500}
	ticks := make(chan time.Time)
	r := reporter(t, sink, ticks)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.Run(ctx)

	waitForCount(t, sink, 1)
	ticks <- time.Now()
	waitForCount(t, sink, 2)
}

// Even a refused credential: the config pull sees the same refusal and exits
// with a code that says so, and two loops racing to end the process would make
// which code it exits with a matter of scheduling.
func TestHeartbeatSurvivesARefusedCredential(t *testing.T) {
	sink := &recorder{status: 401}
	ticks := make(chan time.Time)
	r := reporter(t, sink, ticks)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.Run(ctx)

	waitForCount(t, sink, 1)
	ticks <- time.Now()
	waitForCount(t, sink, 2)
}

func TestHeartbeatStopsOnContextCancel(t *testing.T) {
	sink := &recorder{}
	r := reporter(t, sink, make(chan time.Time))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()

	waitForCount(t, sink, 1)
	cancel()

	select {
	case err := <-done:
		if err != context.Canceled {
			t.Fatalf("Run = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return when its context was cancelled")
	}
}

// A node serves nothing until it holds a config document, and a
// heartbeat is the node saying what it is serving.
func TestHeartbeatDoesNotStartBeforeTheFirstConfigDocument(t *testing.T) {
	sink := &recorder{}
	ticks := make(chan time.Time)
	r := reporter(t, sink, ticks)

	var ready bool
	var mu sync.Mutex
	r.Ready = func() bool {
		mu.Lock()
		defer mu.Unlock()
		return ready
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.Run(ctx)

	// Long enough for several turns of the not-ready loop. Proving a negative
	// is the one thing a wait-for-a-condition cannot do: what is asserted here
	// is that nothing was sent, and the only way to be sure is to give it the
	// chance.
	time.Sleep(5 * readyPoll)

	if got := sink.count(); got != 0 {
		t.Fatalf("%d beats before the first config document, want 0", got)
	}

	mu.Lock()
	ready = true
	mu.Unlock()

	// No tick. A node that has just been handed its document is visible NOW
	// rather than an interval from now — waiting for the next tick is what
	// left a freshly joined node invisible for thirty seconds.
	waitForCount(t, sink, 1)
}

// A status this node cannot read is a beat with nothing true to say. Skipping
// it is better than sending a snapshot that is half nil, because a heartbeat
// is total: what it leaves out, the row records as absent.
func TestHeartbeatSkipsABeatItCannotDescribe(t *testing.T) {
	sink := &recorder{}
	ticks := make(chan time.Time)
	r := reporter(t, sink, ticks)

	var mu sync.Mutex
	broken := true
	r.Status = func(context.Context) (Status, error) {
		mu.Lock()
		defer mu.Unlock()
		if broken {
			return Status{}, context.DeadlineExceeded
		}
		return Status{Endpoint: "203.0.113.7:41641"}, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.Run(ctx)

	ticks <- time.Now()
	if got := sink.count(); got != 0 {
		t.Fatalf("%d beats while the status could not be read, want 0", got)
	}

	mu.Lock()
	broken = false
	mu.Unlock()

	ticks <- time.Now()
	waitForCount(t, sink, 1)
}

func waitForCount(t *testing.T, sink *recorder, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if sink.count() >= want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("%d beats, want %d", sink.count(), want)
}

// The services a node advertises are how a machine declares what it offers:
// the mothership's presence handler turns each one into a declared service a
// person can then approve. The parser is all-or-nothing — one kind it will not
// accept and the whole heartbeat is refused, every thirty seconds — so the
// shape here has to be exactly the three keys it reads.
func TestHeartbeatBodyCarriesTheServicesThisNodeAdvertises(t *testing.T) {
	body, err := json.Marshal(Status{
		Endpoint: "203.0.113.7:41641",
		Services: []Service{{Kind: "egress", Port: 8443}},
	})
	if err != nil {
		t.Fatalf("marshalling: %v", err)
	}

	var decoded map[string]any
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("unmarshalling: %v", err)
	}

	services, ok := decoded["services"].([]any)
	if !ok || len(services) != 1 {
		t.Fatalf("services = %v, want one entry", decoded["services"])
	}
	service, ok := services[0].(map[string]any)
	if !ok {
		t.Fatalf("service = %v, want an object", services[0])
	}
	if service["kind"] != "egress" {
		t.Fatalf("kind = %v, want egress", service["kind"])
	}
	if service["port"] != float64(8443) {
		t.Fatalf("port = %v, want 8443", service["port"])
	}
	if _, present := service["destination"]; present {
		t.Fatalf("destination should be absent on a relay's own service: %v", service)
	}
}

// A node advertising nothing says nothing: an empty `services` list would be
// this node declaring it offers none, which is what `stopAdvertising` means to
// say — but the key itself is omitted rather than sent empty, because the
// heartbeat is total and an absent key is how every other optional half of it
// is spelled.
func TestHeartbeatOmitsServicesWhenThisNodeAdvertisesNone(t *testing.T) {
	body, err := json.Marshal(Status{Endpoint: "203.0.113.7:41641"})
	if err != nil {
		t.Fatalf("marshalling: %v", err)
	}

	var decoded map[string]any
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("unmarshalling: %v", err)
	}
	if _, present := decoded["services"]; present {
		t.Fatalf("services should be absent: %s", body)
	}
}
