package serve

import (
	"context"
	"github.com/gentility-ai/gentility/connector/internal/state"
	"github.com/gentility-ai/gentility/connector/internal/tsnode/tsnodetest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// Exercise the callers, not just record: ordinary Status deliberately omits
// private diagnostics, so either writer using it would save an empty peer list.
func TestSnapshotWritersSavePrivatePeers(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	node, address, endpoint := tsnodetest.PeerNode(t, ctx)
	for _, writer := range []string{"heartbeat", "applied-document"} {
		t.Run(writer, func(t *testing.T) {
			store := &state.Store{Dir: t.TempDir()}
			a := Attendance{Store: store, Session: Session{Node: node}}
			documents := newLastDocument()
			documents.set(fromDocument(applied()))
			advertised := &advertisement{}
			if writer == "heartbeat" {
				if _, err := status(a, advertised, documents, t.Logf)(ctx); err != nil {
					t.Fatal(err)
				}
			} else {
				note(ctx, a, advertised, documents, t.Logf)
			}
			private, err := store.LoadNetCheck()
			if err != nil {
				t.Fatal(err)
			}
			if len(private.Peers) != 1 || private.Peers[0].Address != address || !slices.Contains(private.Peers[0].Endpoints, endpoint) {
				t.Fatalf("writer lost private peer candidates: %+v", private)
			}
			public, err := os.ReadFile(filepath.Join(store.Dir, "status.json"))
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(public), endpoint) {
				t.Fatal("writer leaked private endpoint into public status")
			}
		})
	}
}
