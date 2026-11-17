package tsnode

import (
	"strings"
	"testing"

	"tailscale.com/tsnet"
)

func TestAssertNetstackRefusesAServerThatHasNotStarted(t *testing.T) {
	// A tsnet.Server that has never been brought up has no netstack, so the
	// type assertion cannot succeed. The point is the SHAPE of the failure:
	// a named error, not a panic on a nil interface.
	err := AssertNetstack(&tsnet.Server{})
	if err == nil {
		t.Fatal("AssertNetstack accepted a server that was never started")
	}
	if !strings.Contains(err.Error(), "DialContextTCPWithBind") {
		t.Fatalf("error %q does not name the method the assertion is about", err)
	}
}
