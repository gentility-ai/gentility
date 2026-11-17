package phonenode

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func quickPings(t *testing.T) {
	t.Helper()
	every, within := pingEvery, pingWithin
	pingEvery, pingWithin = 50*time.Millisecond, 150*time.Millisecond
	t.Cleanup(func() { pingEvery, pingWithin = every, within })
}

// Review focus 2: iOS suspended the app with the socket open, and the door
// went away while it slept. Nothing will ever arrive, and nothing says so.
func TestASilentDoorIsClosedAsSilent(t *testing.T) {
	quickPings(t)
	for name, serve := range map[string]func(context.Context, *websocket.Conn){
		"says nothing and reads nothing": func(ctx context.Context, _ *websocket.Conn) { <-ctx.Done() },
		"stops halfway through a frame": func(ctx context.Context, conn *websocket.Conn) {
			w, err := conn.Writer(ctx, websocket.MessageText)
			if err != nil {
				return
			}
			w.Write([]byte(strings.Repeat("x", 64<<10))) // flushed as fragments, never finished
			<-ctx.Done()
		},
	} {
		t.Run(name, func(t *testing.T) {
			fake := newFakeDoor(t, serve)
			heard := newRecorder()
			open(t, fake, heard)
			if reason := heard.waitClosed(t); reason != "door_silent" {
				t.Fatalf("OnClose reason = %q, want door_silent", reason)
			}
			if _, closes := heard.heard(); len(closes) != 1 {
				t.Fatalf("OnClose called %d times", len(closes))
			}
		})
	}
}

func TestADoorThatAnswersPingsStaysOpen(t *testing.T) {
	quickPings(t)
	fake := newFakeDoor(t, readAll)
	heard := newRecorder()
	open(t, fake, heard)
	select {
	case <-heard.closed:
		_, closes := heard.heard()
		t.Fatalf("a door answering its pings was closed: %v", closes)
	case <-time.After(time.Second):
	}
}

// A pong queues behind a frame already on the wire. A long transcript
// arriving slowly is a door that is talking.
func TestASlowLongFrameIsNotSilence(t *testing.T) {
	quickPings(t)
	fake := newFakeDoor(t, func(ctx context.Context, conn *websocket.Conn) {
		go readAll(ctx, conn)
		w, err := conn.Writer(ctx, websocket.MessageText)
		if err != nil {
			return
		}
		for range 20 { // a megabyte over a second, in pieces
			w.Write([]byte(strings.Repeat("x", 50<<10)))
			time.Sleep(50 * time.Millisecond)
		}
		w.Close()
		<-ctx.Done()
	})
	heard := newRecorder()
	open(t, fake, heard)
	eventually(t, "the long frame heard", func() bool { frames, _ := heard.heard(); return len(frames) == 1 })
	if _, closes := heard.heard(); len(closes) != 0 {
		t.Fatalf("a door mid-frame was called silent: %v", closes)
	}
}

// A phone's wall clock is set by the network and jumps. Silence is measured
// on the monotonic clock, or a jump forward would close a door that is
// talking, and one back would keep a dead one.
func TestSilenceIsMeasuredOnTheMonotonicClock(t *testing.T) {
	c := &liveConn{}
	c.hear()
	heard := c.last.Load()
	if heard == nil || !strings.Contains(heard.String(), " m=") {
		t.Fatalf("the last byte was heard at %v, a time with no monotonic reading", heard)
	}
	if quiet := c.quietFor(); quiet < 0 || quiet > time.Second {
		t.Fatalf("quietFor = %s just after a byte", quiet)
	}
}
