package tsnode

import (
	"time"

	"tailscale.com/types/logger"
)

// RateLimit wraps logf so each format string passes at most burst lines at once,
// refilling one per every, and tracks up to formats distinct formats. A format
// that overruns its burst is held, with one notice saying so, until it refills.
//
// It lives here because the limiter is the library's own, and tsnode is the one
// place that names the library.
func RateLimit(logf func(string, ...any), every time.Duration, burst, formats int) func(string, ...any) {
	return logger.RateLimitedFn(logf, every, burst, formats)
}
