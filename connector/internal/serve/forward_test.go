package serve

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gentility-ai/gentility/connector/internal/config"
	"github.com/gentility-ai/gentility/connector/internal/forward"
	"github.com/gentility-ai/gentility/connector/internal/state"
	"github.com/gentility-ai/gentility/connector/internal/tsnode"
)

func forwardDocument(services ...config.Service) *config.Document {
	return &config.Document{
		RendererVersion: config.RendererVersion,
		Revision:        1,
		Grants:          []tsnode.ConfigGrant{{Org: "acme", Address: inAcme}},
		Services:        services,
		Gateway:         config.Gateway{Address: "fd06:6e7:0:1::9", Port: 443},
	}
}

var markedRunner = config.Service{Kind: "vmrunner", Org: "acme", Port: 7470, State: "approved", Forwardable: true}

// channel is a config client against a server that answers one document and
// then that nothing is new, polled as fast as the test can.
func channel(t *testing.T, document *config.Document) *config.Client {
	t.Helper()

	wire, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Has("since") {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		_, _ = w.Write(wire)
	}))
	t.Cleanup(server.Close)

	store := &state.Store{Dir: t.TempDir()}
	node := state.Node{NodeID: "node-1", NodeCredential: "test-node-secret", LastRevision: state.NoRevision}
	if err := store.SaveNode(node); err != nil {
		t.Fatal(err)
	}
	configs := config.New(server.URL, node, store, server.Client())
	configs.MinPoll = time.Millisecond
	return configs
}

func followForward(t *testing.T, configs *config.Client, forwarder *forward.Forwarder) <-chan error {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	t.Cleanup(forwarder.Stop)

	ended := make(chan error, 1)
	go func() {
		ended <- follow(ctx, configs, forwarder, func(document *config.Document) error {
			return carryOut(ctx, forwarder, document, &relayGeneration{}, &advertisement{}, Attendance{}, nil, t.Logf)
		}, func(*config.Document) {})
	}()
	return ended
}

func eventually(t *testing.T, what string, ended <-chan error, done func() bool) {
	t.Helper()

	deadline := time.After(5 * time.Second)
	for !done() {
		select {
		case err := <-ended:
			t.Fatalf("the config channel ended before %s: %v", what, err)
		case <-deadline:
			t.Fatalf("never: %s", what)
		case <-time.After(time.Millisecond):
		}
	}
}

// The heartbeat waits on configs.Ready, so a first document without the entry
// has to leave the node ready: a server that declares the service when it
// hears the node would otherwise never hear it.
func TestAForwardingNodeIsReadyToBeatOnADocumentWithoutTheEntry(t *testing.T) {
	configs := channel(t, forwardDocument())
	ended := followForward(t, configs, &forward.Forwarder{
		Spec: forward.Spec{Kind: "vmrunner", Socket: "/run/vmrunner/door.sock"},
		Listen: func(netip.Addr, uint16) (net.Listener, error) {
			t.Error("a document without the entry was bound")
			return nil, errors.New("unwanted")
		},
	})

	eventually(t, "the node was ready to beat", ended, configs.Ready)
}

func TestAFailedBindIsTriedAgainOnAnUnchangedPoll(t *testing.T) {
	var binds atomic.Int32
	configs := channel(t, forwardDocument(markedRunner))
	ended := followForward(t, configs, &forward.Forwarder{
		Spec: forward.Spec{Kind: "vmrunner", Socket: "/run/vmrunner/door.sock"},
		Listen: func(netip.Addr, uint16) (net.Listener, error) {
			if binds.Add(1) == 1 {
				return nil, errors.New("the address is not up yet")
			}
			return net.Listen("tcp", "127.0.0.1:0")
		},
	})

	eventually(t, "the bind was tried again", ended, func() bool { return binds.Load() == 2 })
}

// Even with an approved egress service in its document, a forwarding node
// starts no relay. Without the forwarder the same document starts one, which
// is what makes the first half able to fail.
func TestAForwardingNodeRunsNoRelay(t *testing.T) {
	document := forwardDocument(markedRunner,
		config.Service{Kind: "egress", Org: "acme", Port: 8443, State: "approved"})
	if d := reconcile(document, serving{}); d.do != startRelay {
		t.Fatalf("the document asks for %s, so this test proves nothing", d)
	}

	binds := 0
	forwarder := &forward.Forwarder{
		Spec: forward.Spec{Kind: "vmrunner", Socket: "/run/vmrunner/door.sock"},
		Listen: func(netip.Addr, uint16) (net.Listener, error) {
			binds++
			return net.Listen("tcp", "127.0.0.1:0")
		},
	}
	t.Cleanup(forwarder.Stop)

	relay := &relayGeneration{}
	if err := carryOut(context.Background(), forwarder, document, relay, &advertisement{}, Attendance{}, nil, t.Logf); err != nil {
		t.Fatal(err)
	}
	if relay.serving().up {
		t.Fatal("a forwarding node started an egress relay")
	}
	if binds != 1 {
		t.Fatalf("the forward bound %d time(s), want once", binds)
	}
}
