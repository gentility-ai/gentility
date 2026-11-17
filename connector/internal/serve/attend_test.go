package serve

import (
	"fmt"
	"strings"
	"testing"
)

// The library's lines reach the connector's log with a prefix, and a format
// that repeats past its burst is limited, while another format still passes:
// a storm of one chatty line cannot drown the rest, and a different line is
// never held back by that storm.
func TestOverlayLoggerPrefixesAndLimitsPerFormat(t *testing.T) {
	var lines []string
	logf := overlay(func(format string, args ...any) { lines = append(lines, fmt.Sprintf(format, args...)) })

	for range 8 {
		logf("netmap: %d peers", 3)
	}
	logf("wgengine: Reconfig: %s", "done")

	if lines[0] != "overlay: netmap: 3 peers" {
		t.Fatalf("first line: %q", lines[0])
	}
	var peers int
	for _, line := range lines {
		if strings.Contains(line, "3 peers") && !strings.Contains(line, "RATELIMIT") {
			peers++
		}
	}
	if peers != 5 {
		t.Fatalf("want the burst of 5 peers lines, got %d in %q", peers, lines)
	}
	if last := lines[len(lines)-1]; last != "overlay: wgengine: Reconfig: done" {
		t.Fatalf("the other format was held back: %q", last)
	}
}

// Some library formats end in a newline ("… sent %v len %v\n"), and the
// connector's log adds its own: the overlay drops the format's, so each such
// line is one line, not a line and a blank.
func TestOverlayLoggerDropsTheFormatsTrailingNewline(t *testing.T) {
	var lines []string
	logf := overlay(func(format string, args ...any) { lines = append(lines, fmt.Sprintf(format, args...)) })

	logf("magicsock: disco: %v->%v sent %v len %v\n", "[abcde]", "[fghij]", "ping", 72)

	if len(lines) != 1 || lines[0] != "overlay: magicsock: disco: [abcde]->[fghij] sent ping len 72" {
		t.Fatalf("want one line with no trailing newline, got %q", lines)
	}
}

// The library exempts its disco lines from rate limiting, and magicsock's path
// change ("now using <addr>") is one: every change must land, prefixed, with
// no limiter notice, however many come at once.
func TestOverlayLoggerPassesEveryPathChange(t *testing.T) {
	var lines []string
	logf := overlay(func(format string, args ...any) { lines = append(lines, fmt.Sprintf(format, args...)) })

	for range 8 {
		logf("magicsock: disco: node %v %v now using %v mtu=%v", "[abcde]", "[fghij]", "192.0.2.20:41641", 1280)
	}

	if len(lines) != 8 {
		t.Fatalf("want all 8 path changes, got %d in %q", len(lines), lines)
	}
	for _, line := range lines {
		if !strings.HasPrefix(line, "overlay: magicsock: disco: node ") || strings.Contains(line, "RATELIMIT") {
			t.Fatalf("path change not passed through as is: %q", line)
		}
	}
}

func TestOverlayLoggerOnNilIsQuiet(t *testing.T) {
	deps := Default(nil)
	if deps.Log != nil {
		t.Fatal("Default(nil).Log should stay nil")
	}
}
