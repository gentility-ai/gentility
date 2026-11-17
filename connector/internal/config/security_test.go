package config

import (
	"github.com/gentility-ai/gentility/connector/internal/state"
	"testing"
)

func TestGatewayRefusalPrecedesDocumentEffects(t *testing.T) {
	store := &state.Store{Dir: t.TempDir()}
	before := state.Node{NodeID: "node", NodeCredential: "test", LastRevision: 12}
	if err := store.SaveNode(before); err != nil {
		t.Fatal(err)
	}
	for _, addr := range []string{"127.0.0.1", "2001:4860:4860::8888", "invalid"} {
		document := &Document{Origin: "https://mcp.example", Gateway: Gateway{Address: addr, Port: 443}}
		// A nil node also detects any attempt to apply grants before refusal.
		if err := document.Adopt(nil, store); err == nil {
			t.Fatalf("accepted %s", addr)
		}
		after, err := store.LoadNode()
		if err != nil || after != before {
			t.Fatalf("changed state on refusal: %v", err)
		}
	}
}
