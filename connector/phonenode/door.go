package phonenode

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/coder/websocket"

	"github.com/gentility-ai/gentility/connector/internal/tsnode"
)

// FrameListener hears one door: each text frame in the order it arrived,
// then exactly one OnClose. Both are called from a goroutine of the door's
// own, never while the door is reading, so a slow listener holds up only
// itself. The door's protocol is text frames only: a binary frame is not
// delivered, and closes the door with reason "not_text".
type FrameListener interface {
	OnFrame(doorID string, frame string)
	OnClose(doorID string, reason string)
}

// largestFrame is the door's own limit, which the Mac's Desk enforces on its
// side too. A transcript is one frame, and coder/websocket's default of
// 32 KiB would end the connection on the first long one.
const largestFrame = 32 << 20

// queueDepth and queueBytes bound what a door holds for a listener still
// busy with an earlier frame: so many frames, or so many bytes of them,
// counting the one in the listener's hands. Past either the listener is not
// coming back, and the door is closed rather than its memory grown until
// iOS kills the app for it. queueBytes is two of the longest frames.
const (
	queueDepth = 4096
	queueBytes = 64 << 20
)

var (
	// dialWithin bounds OpenDoor: the dial over DERP, the door's gate (the
	// lobby has up to ten seconds to name this phone) and the upgrade.
	dialWithin = 30 * time.Second
	sendWithin = 10 * time.Second
)

// The words OnClose's reason starts with.
const (
	reasonClosed     = "closed"           // the app called Close
	reasonNodeDown   = "node_down"        // Down, or the node ended
	reasonDoorClosed = "door_closed"      // the door said goodbye: "door_closed: <status> <text>"
	reasonLost       = "lost"             // the connection ended with no goodbye: "lost: <error>"
	reasonStalled    = "listener_stalled" // the listener fell queueDepth frames or queueBytes behind
	reasonNotText    = "not_text"         // a binary frame, which is not this protocol
)

type dialFunc func(ctx context.Context, dst netip.AddrPort) (net.Conn, error)

type door struct {
	id       string
	conn     *websocket.Conn
	listener FrameListener
	frames   chan string
	stop     context.CancelFunc
	logf     func(string, ...any)
	read     atomic.Int64
	queued   atomic.Int64 // bytes read and not yet heard by the listener

	wire          *liveConn
	every, within time.Duration

	once   sync.Once
	reason string // written once, in end, before frames is closed
}

var (
	doorsMu sync.Mutex
	doors   = map[string]*door{}
	doorSeq atomic.Uint64
)

// OpenDoor dials a Mac's door, ws://[address]:port/ws, over this node and
// from this node's own address in the door's organisation, and starts
// delivering its frames to listener.
//
// A Down while the door is still opening wins: the door is not opened over
// a node that has gone, and OpenDoor says node_down, whether the dial got
// through or failed for it.
func OpenDoor(address string, port int, listener FrameListener) string {
	mu.Lock()
	s, isJoining := current, joining != nil
	mu.Unlock()
	switch {
	case s != nil:
		return openDoor(s.node.DialInOrg, s.logf, address, port, listener, func() bool { return current == s })
	case isJoining:
		return failure(codeJoining, errors.New("the node is still joining: open the door when Status says up"))
	default:
		return failure(codeNodeDown, errors.New("no node is up: Join or Rejoin first"))
	}
}

// openDoor opens the door, and registers it only if stillUp, called with mu
// held, says the node it was dialled over is still the current one.
func openDoor(dial dialFunc, logf func(string, ...any), address string, port int, listener FrameListener, stillUp func() bool) string {
	if listener == nil {
		return failure(codeMalformed, errors.New("OpenDoor needs a listener"))
	}
	addr, err := netip.ParseAddr(address)
	if err != nil || !addr.Is6() || addr.Is4In6() || addr.Zone() != "" {
		return failure(codeMalformed, fmt.Errorf("%q is not a 6net address", address))
	}
	if port < 1 || port > 65535 {
		return failure(codeMalformed, fmt.Errorf("%d is not a port", port))
	}
	dst := netip.AddrPortFrom(addr, uint16(port))

	// The one connection this transport makes is the door's, handed to the
	// WebSocket at the upgrade. wire and source are written before the dial
	// returns the connection to the transport, so both are set when Dial
	// succeeds. The transport keeps no connection idle: a dial over DERP that
	// lands after dialWithin is closed, not pooled for a request never made.
	var (
		wire   *liveConn
		source string
	)
	transport := &http.Transport{
		DisableKeepAlives: true,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			conn, err := dial(ctx, dst)
			if err != nil {
				return nil, err
			}
			wire = &liveConn{Conn: conn}
			wire.hear()
			source = conn.LocalAddr().String()
			return wire, nil
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), dialWithin)
	defer cancel()
	conn, response, err := websocket.Dial(ctx, "ws://"+dst.String()+"/ws", &websocket.DialOptions{
		// No HTTPHeader, so no Origin: the door refuses any (door.rs
		// admit_door), and coder/websocket sends one only when told to.
		HTTPClient: &http.Client{Transport: transport},
	})
	if err != nil {
		// A Down mid-dial closes the engine under it, and the dial fails as
		// any dial might; the app is told the node went, not the door.
		mu.Lock()
		up := stillUp()
		mu.Unlock()
		code := dialCode(response, err)
		if !up {
			code = codeNodeDown
		}
		logf("phonenode: the door at %s did not open (%s): %v", dst, code, err)
		return failure(code, fmt.Errorf("the door at %s: %w", dst, err))
	}
	conn.SetReadLimit(largestFrame)

	runCtx, stop := context.WithCancel(context.Background())
	d := &door{
		id:       "door-" + strconv.FormatUint(doorSeq.Add(1), 10),
		conn:     conn,
		listener: listener,
		frames:   make(chan string, queueDepth),
		stop:     stop,
		logf:     logf,
		wire:     wire,
		every:    pingEvery,
		within:   pingWithin,
	}
	// Under mu, so a Down either comes after and closes this door with the
	// rest, or came before and this door does not open.
	mu.Lock()
	up := stillUp()
	if up {
		doorsMu.Lock()
		doors[d.id] = d
		doorsMu.Unlock()
	}
	mu.Unlock()
	if !up {
		conn.CloseNow()
		stop()
		logf("phonenode: the door at %s opened after the node went down; closed", dst)
		return failure(codeNodeDown, errors.New("the node went down while the door was opening"))
	}

	go d.deliver()
	go d.readLoop(runCtx)
	go d.ping(runCtx)
	logf("phonenode: %s open to %s from %s", d.id, dst, source)
	return answer(map[string]any{"door_id": d.id, "source": source})
}

// gonetReset is how gonet, the netstack's net.Conn that DialInOrg hands
// back, reports a reset: a bare errors.New of these words, not ECONNRESET.
// serverClosedIdle is how net/http reports a connection the far end closed
// before the request went out, when its reader sees the close first: an
// unexported errors.New of these words, not io.EOF.
const (
	gonetReset       = "connection reset by peer"
	serverClosedIdle = "http: server closed idle connection"
)

// dialCode names a failed open in the word the app switches on.
func dialCode(response *http.Response, err error) string {
	switch {
	case response != nil:
		return codeRefused
	case errors.Is(err, tsnode.ErrNoGrant):
		return codeNoGrant
	case errors.Is(err, context.DeadlineExceeded):
		return codeTimedOut
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF), errors.Is(err, syscall.ECONNRESET),
		strings.Contains(err.Error(), gonetReset), strings.Contains(err.Error(), serverClosedIdle):
		return codeDropped
	default:
		return codeUnreachable
	}
}

// readLoop reads until the connection ends and never waits on the listener:
// a reader that stopped would leave the door's pings unanswered, and the
// door hangs up on a peer that says nothing. A binary frame ends the door as
// not_text: nothing the door sends is binary, so one is a peer that is not
// speaking this protocol.
func (d *door) readLoop(ctx context.Context) {
	defer close(d.frames)
	for {
		kind, data, err := d.conn.Read(ctx)
		if err != nil {
			d.end(endedBy(err), false)
			return
		}
		if kind != websocket.MessageText {
			d.end(reasonNotText, false)
			return
		}
		d.read.Add(1)
		if d.queued.Add(int64(len(data))) > queueBytes {
			d.end(reasonStalled, false)
			return
		}
		select {
		case d.frames <- string(data):
		default:
			d.end(reasonStalled, false)
			return
		}
	}
}

// deliver hands frames to the listener in order, then the one close. The
// close follows readLoop's close of frames, which follows end, so reason is
// set and every queued frame is delivered before it.
func (d *door) deliver() {
	for frame := range d.frames {
		d.listener.OnFrame(d.id, frame)
		d.queued.Add(-int64(len(frame)))
	}
	d.listener.OnClose(d.id, d.reason)
}

// end records why the door ends, once, takes it out of the table and closes
// the socket. Whatever ends a door — the app, the node, the door itself, a
// stalled listener — comes through here, which is what makes OnClose once.
func (d *door) end(reason string, graceful bool) {
	d.once.Do(func() {
		d.reason = reason
		doorsMu.Lock()
		delete(doors, d.id)
		doorsMu.Unlock()
		d.logf("phonenode: %s ending: %s (%d frame(s) read)", d.id, reason, d.read.Load())
		if graceful {
			// The close handshake waits up to five seconds on the door's
			// answer; Close is the app's call and does not wait with it.
			go func() {
				d.conn.Close(websocket.StatusNormalClosure, "")
				d.stop()
			}()
			return
		}
		d.conn.CloseNow()
		d.stop()
	})
}

func endedBy(err error) string {
	var goodbye websocket.CloseError
	if errors.As(err, &goodbye) {
		return fmt.Sprintf("%s: %d %s", reasonDoorClosed, goodbye.Code, goodbye.Reason)
	}
	return reasonLost + ": " + err.Error()
}

func lookup(doorID string) *door {
	doorsMu.Lock()
	defer doorsMu.Unlock()
	return doors[doorID]
}

// Send writes one text frame to an open door.
func Send(doorID, frame string) string {
	d := lookup(doorID)
	if d == nil {
		return failure(codeNoDoor, fmt.Errorf("no door %s is open", doorID))
	}
	ctx, cancel := context.WithTimeout(context.Background(), sendWithin)
	defer cancel()
	if err := d.conn.Write(ctx, websocket.MessageText, []byte(frame)); err != nil {
		d.end(reasonLost+": "+err.Error(), false)
		return failure(codeSendFailed, err)
	}
	return answer(map[string]any{"ok": true})
}

// Close says goodbye to an open door. OnClose(doorID, "closed") follows,
// after any frame already queued for the listener.
func Close(doorID string) string {
	d := lookup(doorID)
	if d == nil {
		return failure(codeNoDoor, fmt.Errorf("no door %s is open", doorID))
	}
	d.end(reasonClosed, true)
	return answer(map[string]any{"ok": true})
}

// closeDoors ends every open door for reason.
func closeDoors(reason string) {
	doorsMu.Lock()
	open := make([]*door, 0, len(doors))
	for _, d := range doors {
		open = append(open, d)
	}
	doorsMu.Unlock()
	for _, d := range open {
		d.end(reason, false)
	}
}

func openDoors() int {
	doorsMu.Lock()
	defer doorsMu.Unlock()
	return len(doors)
}
