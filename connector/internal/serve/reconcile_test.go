package serve

import (
	"reflect"
	"testing"

	"github.com/gentility-ai/gentility/connector/internal/config"
	"github.com/gentility-ai/gentility/connector/internal/tsnode"
	"github.com/gentility-ai/gentility/connector/internal/version"
)

// This node's addresses, one per organization it holds a grant in. A relay
// listens on the node's own address in the org that hosts the service, so the
// address is half of what identifies a listener and a re-grant is a rebind.
const (
	inAcme    = "fd06:6e7:0:1::1"
	inBeta    = "fd06:6e7:0:2::1"
	regranted = "fd06:6e7:0:1::9"
)

// The config document is the only authority on what this node serves: it never
// decides it is approved. These cases cover
// the whole decision — everything else in serve is the plumbing that
// carries one of them out.
//
// The decision is over the whole set of approved services and not over one of
// them: a node holds services in as many organizations as it has grants, the
// relay generation binds every approved one, and a decision that watched only
// the first would leave a second organization's listener unstarted.
func TestReconcileFollowsTheDocumentAndNeverTheNode(t *testing.T) {
	for _, c := range []struct {
		name     string
		document *config.Document
		now      serving
		want     decision
	}{
		{
			name:     "approved and nothing is bound: bind it",
			document: document(service{kind: "egress", state: "approved", port: 8443}),
			now:      serving{},
			want:     decision{do: startRelay, listeners: []listener{{"acme", inAcme, 8443}}},
		},
		{
			name:     "approved on the port already bound: nothing to do",
			document: document(service{kind: "egress", state: "approved", port: 8443}),
			now:      serving{up: true, listeners: []listener{{"acme", inAcme, 8443}}},
			want:     decision{do: noop},
		},
		{
			name:     "approved on another port: rebind on the one the document names",
			document: document(service{kind: "egress", state: "approved", port: 9443}),
			now:      serving{up: true, listeners: []listener{{"acme", inAcme, 8443}}},
			want:     decision{do: restartRelay, listeners: []listener{{"acme", inAcme, 9443}}},
		},
		{
			name:     "declared but not approved: stop serving it",
			document: document(service{kind: "egress", state: "declared", port: 8443}),
			now:      serving{up: true, listeners: []listener{{"acme", inAcme, 8443}}},
			want:     decision{do: stopRelay},
		},
		{
			name:     "the service is gone from the document: stop serving it",
			document: document(),
			now:      serving{up: true, listeners: []listener{{"acme", inAcme, 8443}}},
			want:     decision{do: stopRelay},
		},
		{
			name: "the floor is above this binary: advertise nothing, keep serving",
			document: func() *config.Document {
				d := document(service{kind: "egress", state: "approved", port: 8443})
				d.CapverFloor = version.NodeCapver + 1
				return d
			}(),
			now:  serving{up: true, listeners: []listener{{"acme", inAcme, 8443}}},
			want: decision{do: stopAdvertising},
		},
		{
			name:     "nothing approved and nothing bound: nothing to do",
			document: document(),
			now:      serving{},
			want:     decision{do: noop},
		},
		{
			name: "a second organization's service is approved: rebind on both",
			document: document(
				service{kind: "egress", state: "approved", port: 8443},
				service{kind: "egress", state: "approved", org: "beta", address: inBeta, port: 8443},
			),
			now: serving{up: true, listeners: []listener{{"acme", inAcme, 8443}}},
			want: decision{do: restartRelay, listeners: []listener{
				{"acme", inAcme, 8443}, {"beta", inBeta, 8443},
			}},
		},
		{
			name: "one of two is withdrawn: rebind on the one that is left",
			document: document(
				service{kind: "egress", state: "approved", org: "beta", address: inBeta, port: 8443},
			),
			now: serving{up: true, listeners: []listener{
				{"acme", inAcme, 8443}, {"beta", inBeta, 8443},
			}},
			want: decision{do: restartRelay, listeners: []listener{{"beta", inBeta, 8443}}},
		},
		{
			name:     "the same port on a re-granted address: rebind on the address the document names",
			document: document(service{kind: "egress", state: "approved", address: regranted, port: 8443}),
			now:      serving{up: true, listeners: []listener{{"acme", inAcme, 8443}}},
			want:     decision{do: restartRelay, listeners: []listener{{"acme", regranted, 8443}}},
		},
		{
			name: "the same set in another order: nothing to do",
			document: document(
				service{kind: "egress", state: "approved", org: "beta", address: inBeta, port: 8443},
				service{kind: "egress", state: "approved", port: 8443},
			),
			now: serving{up: true, listeners: []listener{
				{"acme", inAcme, 8443}, {"beta", inBeta, 8443},
			}},
			want: decision{do: noop},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := reconcile(c.document, c.now); !reflect.DeepEqual(got, c.want) {
				t.Fatalf("reconcile = %s, want %s", got, c.want)
			}
		})
	}
}

// A node holding an approved egress service in one organization and a declared
// one in another serves the approved one, and only it: `services` is a list
// holding every kind in every state, and what this node binds must not depend
// on the order the renderer happened to write.
func TestReconcileFindsTheApprovedServiceWhereverItIsInTheList(t *testing.T) {
	doc := document(
		service{kind: "egress", state: "declared", port: 8443},
		service{kind: "gateway", state: "approved", port: 443},
		service{kind: "egress", state: "approved", org: "beta", address: inBeta, port: 9443},
	)

	want := decision{do: startRelay, listeners: []listener{{"beta", inBeta, 9443}}}
	if got := reconcile(doc, serving{}); !reflect.DeepEqual(got, want) {
		t.Fatalf("reconcile = %s, want %s", got, want)
	}
}

type service struct {
	kind    string
	state   string
	org     string
	address string
	port    int
}

func document(services ...service) *config.Document {
	doc := &config.Document{RendererVersion: config.RendererVersion, Revision: 1}
	for _, s := range services {
		org, address := s.org, s.address
		if org == "" {
			org = "acme"
		}
		if address == "" {
			address = inAcme
		}
		doc.Services = append(doc.Services, config.Service{
			ID: org + "-" + s.kind + "-" + s.state, Kind: s.kind, Org: org, Port: s.port, State: s.state,
		})
		doc.Grants = append(doc.Grants, tsnode.ConfigGrant{Org: org, Address: address})
	}
	return doc
}
