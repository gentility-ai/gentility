package config

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/gentility-ai/gentility/connector/internal/state"
	"github.com/gentility-ai/gentility/connector/internal/tsnode"
)

// A document is taken on whole or not at all, and the grants come first. The
// grants are what every dial is pinned from, so a node that wrote the gateway
// down and then refused the grants would be holding half a document — and it is
// the half it comes back from a restart with.
//
// The nil node is the second assertion: SetGrants on one would panic, so an
// Adopt that reached it could not pass quietly.
func TestAdoptRefusesTheWholeDocumentWhenTheGrantsDoNot(t *testing.T) {
	store := &state.Store{Dir: t.TempDir()}
	if err := store.SaveNode(state.Node{NodeID: "n1", NodeCredential: "secret"}); err != nil {
		t.Fatal(err)
	}

	document := &Document{
		Grants:  []tsnode.ConfigGrant{{Org: "acme", Address: "not an address"}},
		Gateway: Gateway{Address: "fd06:6e7:0:1::1", Port: 443, CAPEM: "-----BEGIN CERTIFICATE-----"},
	}

	if err := document.Adopt(nil, store); err == nil {
		t.Fatal("a document whose grants do not parse was adopted")
	}

	stored, err := store.LoadNode()
	if err != nil {
		t.Fatal(err)
	}
	if stored.GatewayAddr != "" || stored.CAPEM != "" {
		t.Fatalf("the gateway was recorded from a document that was refused: %+v", stored)
	}
}

func TestOriginRefusalPrecedesEffects(t *testing.T) {
	store := &state.Store{Dir: t.TempDir()}
	if err := store.SaveNode(state.Node{NodeID: "node", NodeCredential: "secret", Origin: "https://old.example", LastRevision: 12}); err != nil {
		t.Fatal(err)
	}
	for _, origin := range []string{"https://other.example", "https://user:secret@old.example/path", "http://remote.example"} {
		document := &Document{Origin: origin, Revision: 13, Gateway: Gateway{Address: "fd06:6e7:0:1::1", Port: 443}}
		// A nil tsnode panics if the refusal permits any grant application.
		if err := document.Adopt(nil, store); err == nil {
			t.Fatal("expected origin refusal")
		}
		saved, err := store.LoadNode()
		if err != nil || saved.Origin != "https://old.example" || saved.LastRevision != 12 {
			t.Fatal("refused config changed state")
		}
	}
}

func TestUnreadableOriginStatePrecedesEffects(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permission fixture requires unprivileged process")
	}
	directory := t.TempDir()
	store := &state.Store{Dir: directory}
	if err := store.SaveNode(state.Node{NodeID: "node", NodeCredential: "secret"}); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(filepath.Join(directory, "node.json"))
	if err := os.Chmod(filepath.Join(directory, "node.json"), 0000); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(filepath.Join(directory, "node.json"), 0600)
	document := &Document{Origin: "https://mcp.example", Gateway: Gateway{Address: "fd06:6e7:0:1::1", Port: 443}}
	if err := document.Adopt(nil, store); err == nil {
		t.Fatal("adopted despite unreadable identity")
	}
	if err := os.Chmod(filepath.Join(directory, "node.json"), 0600); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(filepath.Join(directory, "node.json"))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("refusal damaged identity")
	}
}

func TestOptionalOriginWireCompatibility(t *testing.T) {
	for _, wire := range []string{`{"renderer_version":1}`, `{"renderer_version":1,"origin":"https://mcp.example"}`} {
		var current Document
		if err := json.Unmarshal([]byte(wire), &current); err != nil {
			t.Fatal(err)
		}
		var old struct {
			RendererVersion int `json:"renderer_version"`
		}
		if err := json.Unmarshal([]byte(wire), &old); err != nil || old.RendererVersion != RendererVersion {
			t.Fatal("additive field broke old decoder")
		}
	}
}

// `forwardable` is written only when true, so its absence is false: a service
// the server did not mark is one `serve --forward` never binds. readDocument is
// the one decoder every reader of the document goes through.
func TestForwardableIsReadOnlyWhereTheServerWroteIt(t *testing.T) {
	wire := `{"renderer_version":1,"services":[` +
		`{"id":"a","kind":"vmrunner","org":"platform","port":7443,"state":"approved","address":"fd06:6e7:0:a::5","forwardable":true},` +
		`{"id":"b","kind":"egress","org":"platform","port":8443,"state":"approved","address":"fd06:6e7:0:a::5"}]}`

	doc, err := readDocument(bytes.NewBufferString(wire))
	if err != nil {
		t.Fatal(err)
	}
	if !doc.Services[0].Forwardable || doc.Services[1].Forwardable {
		t.Fatalf("forwardable = %t, %t; want true where written and false where absent",
			doc.Services[0].Forwardable, doc.Services[1].Forwardable)
	}

	// Re-encoded, an unmarked service carries no mark, as the server wrote it.
	encoded, err := json.Marshal(doc.Services[1])
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encoded, []byte("forwardable")) {
		t.Fatalf("an unmarked service encodes as %s, want no forwardable key", encoded)
	}
}
