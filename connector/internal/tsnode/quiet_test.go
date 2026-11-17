package tsnode

import (
	"context"
	"testing"

	"tailscale.com/envknob"
)

// tsnet starts a logtail uploader to log.tailscale.com unless this knob is
// set (tsnet.Server.startLogger). Up sets it before it does anything else,
// including refusing a bad join, so no path through Up leaves it unset.
func TestUpKeepsTheLibrarysLogsOnThisMachine(t *testing.T) {
	t.Setenv("TS_NO_LOGS_NO_SUPPORT", "")
	if envknob.NoLogsNoSupport() {
		t.Fatal("precondition: the knob is already set")
	}
	if _, err := Up(context.Background(), Options{}); err == nil {
		t.Fatal("an Up with no control URL should refuse")
	}
	if !envknob.NoLogsNoSupport() {
		t.Fatal("TS_NO_LOGS_NO_SUPPORT is not set after Up")
	}
}
