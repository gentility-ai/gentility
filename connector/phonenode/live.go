package phonenode

import (
	"context"
	"math"
	"net"
	"sync/atomic"
	"time"
)

// A door suspended with the app comes back to a socket that may be dead at
// the far end: the door's heartbeat gave up on this phone while it slept and
// its goodbye never arrived. The door pings this side; this side pings the
// door, and a ping with no pong and no byte heard for pingWithin is a door
// that has gone.
var (
	pingEvery  = 20 * time.Second
	pingWithin = 20 * time.Second
)

const reasonDoorSilent = "door_silent"

// liveConn is the door's socket, noting when it last heard a byte. A pong
// waits behind any frame already on the wire, so a ping can fail while a
// long transcript is still arriving; the bytes say the door is there.
//
// last is a time.Now, monotonic reading and all: a phone's wall clock is
// set by the network and jumps, and silence measured on it would too.
type liveConn struct {
	net.Conn
	last atomic.Pointer[time.Time]
}

func (c *liveConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if n > 0 {
		c.hear()
	}
	return n, err
}

func (c *liveConn) hear() {
	now := time.Now()
	c.last.Store(&now)
}

// quietFor is how long since the last byte; a socket never heard has been
// quiet forever.
func (c *liveConn) quietFor() time.Duration {
	heard := c.last.Load()
	if heard == nil {
		return math.MaxInt64
	}
	return time.Since(*heard)
}

func (d *door) ping(ctx context.Context) {
	tick := time.NewTicker(d.every)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		within, cancel := context.WithTimeout(ctx, d.within)
		err := d.conn.Ping(within)
		cancel()
		if err == nil || ctx.Err() != nil || d.wire.quietFor() < d.within {
			continue
		}
		d.end(reasonDoorSilent, false)
		return
	}
}
