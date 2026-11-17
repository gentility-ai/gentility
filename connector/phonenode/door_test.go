package phonenode

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/coder/websocket"
)

const (
	doorAddress     = "fd06:6e7:0:2::4"
	helloAsk        = `{"id":1,"request":{"type":"hello"}}`
	helloReply      = `{"id":1,"response":{"type":"hello","version":"0.1.279","build":"522555a14","capabilities":["sessions"],"protocol":{"major":1,"minor":43}}}`
	transcriptReply = `{"id":2,"response":{"type":"transcript","items":[{"id":"i1","session_id":"s1","kind":"message","role":"user","content":"hi","created_at":"2026-10-24T09:00:00Z","ordinal":1,"turn":1,"message_item_id":null,"parent_call_id":null,"remote_id":null}]}}`
)

// recorder is a FrameListener that keeps what it hears, and can be made to
// hold every OnFrame until block is closed — Swift dispatching to a busy
// main thread.
type recorder struct {
	mu     sync.Mutex
	frames []string
	closes []string
	closed chan struct{}
	block  chan struct{}
}

func newRecorder() *recorder { return &recorder{closed: make(chan struct{})} }

func (r *recorder) OnFrame(_ string, frame string) {
	if r.block != nil {
		<-r.block
	}
	r.mu.Lock()
	r.frames = append(r.frames, frame)
	r.mu.Unlock()
}

func (r *recorder) OnClose(_ string, reason string) {
	r.mu.Lock()
	r.closes = append(r.closes, reason)
	first := len(r.closes) == 1
	r.mu.Unlock()
	if first {
		close(r.closed)
	}
}

func (r *recorder) heard() (frames, closes []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.frames), slices.Clone(r.closes)
}

func (r *recorder) waitClosed(t *testing.T) string {
	t.Helper()
	select {
	case <-r.closed:
	case <-time.After(5 * time.Second):
		t.Fatal("OnClose was never called")
	}
	_, closes := r.heard()
	return closes[0]
}

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if ok() {
			return
		}
	}
	t.Fatalf("never: %s", what)
}

// fakeDoor is a door in this process: /ws as the Mac's Desk serves it,
// refusing any Origin with 403, and serve playing the
// door's half of each connection until it returns or the test ends.
type fakeDoor struct {
	addr    string
	origins chan []string
}

func newFakeDoor(t *testing.T, serve func(ctx context.Context, conn *websocket.Conn)) *fakeDoor {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	fake := &fakeDoor{origins: make(chan []string, 64)}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/ws" {
			http.NotFound(w, r)
			return
		}
		fake.origins <- r.Header.Values("Origin")
		if r.Header.Get("Origin") != "" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		conn.SetReadLimit(largestFrame)
		serve(ctx, conn)
	}))
	t.Cleanup(func() { cancel(); server.Close() })
	fake.addr = server.Listener.Addr().String()
	return fake
}

// dial stands in for tsnode.Node.DialInOrg: whatever overlay address is
// asked for, the connection reaches this door.
func (d *fakeDoor) dial(ctx context.Context, _ netip.AddrPort) (net.Conn, error) {
	var dialer net.Dialer
	return dialer.DialContext(ctx, "tcp", d.addr)
}

// alwaysUp is a node that stays up while its door opens.
func alwaysUp() bool { return true }

func readAll(ctx context.Context, conn *websocket.Conn) {
	for {
		if _, _, err := conn.Read(ctx); err != nil {
			return
		}
	}
}

func open(t *testing.T, fake *fakeDoor, listener FrameListener) string {
	t.Helper()
	got := decode(t, openDoor(fake.dial, func(string, ...any) {}, doorAddress, 6446, listener, alwaysUp))
	id, _ := got["door_id"].(string)
	if id == "" {
		t.Fatalf("the door did not open: %v", got)
	}
	t.Cleanup(func() { closeDoors("test_over") })
	return id
}

func TestOpenDoorSendsNoOrigin(t *testing.T) {
	fake := newFakeDoor(t, readAll)
	open(t, fake, newRecorder())
	if origins := <-fake.origins; len(origins) != 0 {
		t.Fatalf("the upgrade said Origin %q; the door refuses any", origins)
	}
}

// The fake is faithful to the door, and a refusal is reported as one.
func TestADoorThatRefusesIsReportedRefused(t *testing.T) {
	fake := newFakeDoor(t, readAll)
	_, _, err := websocket.Dial(context.Background(), "ws://"+fake.addr+"/ws", &websocket.DialOptions{
		HTTPHeader: http.Header{"Origin": {"https://elsewhere.test"}},
	})
	if err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("the fake door let a page in: %v", err)
	}

	refusing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	t.Cleanup(refusing.Close)
	heard := newRecorder()
	dial := func(ctx context.Context, _ netip.AddrPort) (net.Conn, error) {
		var dialer net.Dialer
		return dialer.DialContext(ctx, "tcp", refusing.Listener.Addr().String())
	}
	if got := decode(t, openDoor(dial, func(string, ...any) {}, doorAddress, 6446, heard, alwaysUp)); got["code"] != "refused" {
		t.Fatalf("OpenDoor = %v, want code refused", got)
	}
	if frames, closes := heard.heard(); len(frames)+len(closes) != 0 {
		t.Fatal("a door that never opened called its listener")
	}
}

// The gate drops a dialler the lobby will not name before a byte of HTTP.
func TestADoorThatHangsUpBeforeHTTPIsDropped(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			conn.Close()
		}
	}()
	dial := func(ctx context.Context, _ netip.AddrPort) (net.Conn, error) {
		var dialer net.Dialer
		return dialer.DialContext(ctx, "tcp", listener.Addr().String())
	}
	if got := decode(t, openDoor(dial, func(string, ...any) {}, doorAddress, 6446, newRecorder(), alwaysUp)); got["code"] != "dropped" {
		t.Fatalf("OpenDoor = %v, want code dropped", got)
	}
}

// Review focus 1: a gate that holds HTTP and never answers.
func TestADoorThatNeverAnswersHTTPTimesOut(t *testing.T) {
	within := dialWithin
	dialWithin = 200 * time.Millisecond
	t.Cleanup(func() { dialWithin = within })

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			defer conn.Close() // held open, never written to
		}
	}()
	dial := func(ctx context.Context, _ netip.AddrPort) (net.Conn, error) {
		var dialer net.Dialer
		return dialer.DialContext(ctx, "tcp", listener.Addr().String())
	}
	start := time.Now()
	if got := decode(t, openDoor(dial, func(string, ...any) {}, doorAddress, 6446, newRecorder(), alwaysUp)); got["code"] != "timed_out" {
		t.Fatalf("OpenDoor = %v, want code timed_out", got)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("OpenDoor outlived dialWithin")
	}
}

// The dial over DERP does not always give up when OpenDoor does. A
// connection it hands back after dialWithin is closed, not kept idle in the
// transport for no one.
func TestAConnectionDialledTooLateIsClosed(t *testing.T) {
	within := dialWithin
	dialWithin = 100 * time.Millisecond
	t.Cleanup(func() { dialWithin = within })

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	accepted := make(chan net.Conn, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		t.Cleanup(func() { conn.Close() })
		accepted <- conn
	}()
	dial := func(context.Context, netip.AddrPort) (net.Conn, error) {
		time.Sleep(300 * time.Millisecond) // past dialWithin, and deaf to it
		var dialer net.Dialer
		return dialer.Dial("tcp", listener.Addr().String())
	}
	if got := decode(t, openDoor(dial, func(string, ...any) {}, doorAddress, 6446, newRecorder(), alwaysUp)); got["code"] != "timed_out" {
		t.Fatalf("OpenDoor = %v, want code timed_out", got)
	}

	var conn net.Conn
	select {
	case conn = <-accepted:
	case <-time.After(5 * time.Second):
		t.Fatal("the late dial never arrived")
	}
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := conn.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
		t.Fatalf("the late connection was kept open: read = %v, want EOF", err)
	}
}

func TestADoorsFramesArriveInOrderAndWhole(t *testing.T) {
	sent := []string{helloReply, transcriptReply}
	for i := range 100 {
		sent = append(sent, fmt.Sprintf(`{"event":{"type":"notice","text":"%d"}}`, i))
	}
	// Thirty-two times coder/websocket's default read limit.
	sent = append(sent, `{"id":3,"response":{"type":"transcript","items":[],"pad":"`+strings.Repeat("x", 1<<20)+`"}}`)

	fake := newFakeDoor(t, func(ctx context.Context, conn *websocket.Conn) {
		for _, frame := range sent {
			if conn.Write(ctx, websocket.MessageText, []byte(frame)) != nil {
				return
			}
		}
		conn.Close(websocket.StatusNormalClosure, "done")
	})
	heard := newRecorder()
	open(t, fake, heard)

	if reason := heard.waitClosed(t); reason != "door_closed: 1000 done" {
		t.Fatalf("OnClose reason = %q", reason)
	}
	frames, closes := heard.heard()
	if !slices.Equal(frames, sent) {
		t.Fatalf("heard %d frames, want the %d sent, in order and whole", len(frames), len(sent))
	}
	if len(closes) != 1 {
		t.Fatalf("OnClose called %d times", len(closes))
	}
}

func TestSendReachesTheDoorAndConcurrentSendsAllArrive(t *testing.T) {
	got := make(chan string, 64)
	fake := newFakeDoor(t, func(ctx context.Context, conn *websocket.Conn) {
		for {
			_, data, err := conn.Read(ctx)
			if err != nil {
				return
			}
			got <- string(data)
		}
	})
	id := open(t, fake, newRecorder())

	if answer := decode(t, Send(id, helloAsk)); answer["ok"] != true {
		t.Fatalf("Send = %v", answer)
	}
	if frame := <-got; frame != helloAsk {
		t.Fatalf("the door read %q", frame)
	}

	var wg sync.WaitGroup
	for i := range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			Send(id, fmt.Sprintf(`{"id":%d,"request":{"type":"hello"}}`, i+10))
		}()
	}
	wg.Wait()
	seen := map[string]bool{}
	for range 20 {
		select {
		case frame := <-got:
			seen[frame] = true
		case <-time.After(5 * time.Second):
			t.Fatalf("the door read %d of 20 concurrent sends", len(seen))
		}
	}
	if len(seen) != 20 {
		t.Fatalf("the door read %d distinct frames of 20", len(seen))
	}
}

func TestCloseIsHeardOnceAndSendAfterItIsRefused(t *testing.T) {
	fake := newFakeDoor(t, readAll)
	heard := newRecorder()
	id := open(t, fake, heard)

	if got := decode(t, Close(id)); got["ok"] != true {
		t.Fatalf("Close = %v", got)
	}
	if reason := heard.waitClosed(t); reason != "closed" {
		t.Fatalf("OnClose reason = %q, want closed", reason)
	}
	if got := decode(t, Send(id, helloAsk)); got["code"] != "no_door" {
		t.Fatalf("Send after Close = %v, want code no_door", got)
	}
	if got := decode(t, Close(id)); got["code"] != "no_door" {
		t.Fatalf("a second Close = %v, want code no_door", got)
	}
	time.Sleep(200 * time.Millisecond) // a second OnClose would land in here
	if _, closes := heard.heard(); len(closes) != 1 {
		t.Fatalf("OnClose called %d times: %v", len(closes), closes)
	}
}

func TestAConnectionThatEndsWithoutACloseFrameIsLost(t *testing.T) {
	fake := newFakeDoor(t, func(context.Context, *websocket.Conn) {}) // returns: CloseNow, no close frame
	heard := newRecorder()
	open(t, fake, heard)
	if reason := heard.waitClosed(t); !strings.HasPrefix(reason, "lost: ") {
		t.Fatalf("OnClose reason = %q, want lost: …", reason)
	}
}

// Review focus 4.
func TestAListenerThatBlocksDoesNotStallTheDoor(t *testing.T) {
	pinged := make(chan error, 1)
	fake := newFakeDoor(t, func(ctx context.Context, conn *websocket.Conn) {
		go readAll(ctx, conn)
		for i := range 3 {
			conn.Write(ctx, websocket.MessageText, []byte(fmt.Sprintf(`{"event":{"type":"notice","text":"%d"}}`, i)))
		}
		// Answered only if the phone is still reading: the door's heartbeat.
		within, cancel := context.WithTimeout(ctx, 2*time.Second)
		pinged <- conn.Ping(within)
		cancel()
		<-ctx.Done()
	})
	heard := newRecorder()
	heard.block = make(chan struct{})
	open(t, fake, heard)

	if err := <-pinged; err != nil {
		t.Fatalf("the door's ping went unanswered while the listener was busy: %v", err)
	}
	close(heard.block)
	eventually(t, "three frames heard", func() bool { frames, _ := heard.heard(); return len(frames) == 3 })
	frames, closes := heard.heard()
	for i, frame := range frames {
		if want := fmt.Sprintf(`{"event":{"type":"notice","text":"%d"}}`, i); frame != want {
			t.Fatalf("frame %d = %q, want %q", i, frame, want)
		}
	}
	if len(closes) != 0 {
		t.Fatalf("a busy listener's door was closed: %v", closes)
	}
}

func TestAListenerThatNeverCatchesUpIsCutOff(t *testing.T) {
	fake := newFakeDoor(t, func(ctx context.Context, conn *websocket.Conn) {
		go readAll(ctx, conn)
		for range queueDepth + 100 {
			if conn.Write(ctx, websocket.MessageText, []byte(`{"event":{"type":"notice","text":"x"}}`)) != nil {
				return
			}
		}
		<-ctx.Done()
	})
	heard := newRecorder()
	heard.block = make(chan struct{})
	open(t, fake, heard)

	eventually(t, "the door gives up on its listener", func() bool { return openDoors() == 0 })
	close(heard.block)
	if reason := heard.waitClosed(t); reason != "listener_stalled" {
		t.Fatalf("OnClose reason = %q, want listener_stalled", reason)
	}
	if frames, _ := heard.heard(); len(frames) == 0 || len(frames) > queueDepth+1 {
		t.Fatalf("heard %d frames; the queue holds %d and one is in hand", len(frames), queueDepth)
	}
}

// A transcript is one frame of up to largestFrame, so a queue held only to
// queueDepth frames could grow to gigabytes and get the app killed for its
// memory. It is held to queueBytes too.
func TestAListenerFallingBehindByBytesIsCutOff(t *testing.T) {
	long := `{"id":2,"response":{"type":"transcript","items":[],"pad":"` + strings.Repeat("x", 30<<20) + `"}}`
	fake := newFakeDoor(t, func(ctx context.Context, conn *websocket.Conn) {
		go readAll(ctx, conn)
		for range 3 {
			if conn.Write(ctx, websocket.MessageText, []byte(long)) != nil {
				return
			}
		}
		<-ctx.Done()
	})
	heard := newRecorder()
	heard.block = make(chan struct{})
	open(t, fake, heard)

	eventually(t, "the door gives up on its listener", func() bool { return openDoors() == 0 })
	close(heard.block)
	if reason := heard.waitClosed(t); reason != "listener_stalled" {
		t.Fatalf("OnClose reason = %q, want listener_stalled", reason)
	}
	// Two are 60 MiB, one in hand and one queued; the third passes the bound.
	if frames, _ := heard.heard(); len(frames) != 2 {
		t.Fatalf("heard %d frames, want the 2 under queueBytes", len(frames))
	}
}

// Review focus 5.
func TestOpenDoorBeforeJoinSaysSo(t *testing.T) {
	down(t)
	heard := newRecorder()
	if got := decode(t, OpenDoor(doorAddress, 6446, heard)); got["code"] != "node_down" {
		t.Fatalf("OpenDoor with no node = %v, want code node_down", got)
	}

	entered := holdJoins(t)
	joined := make(chan string, 1)
	go func() { joined <- Join(t.TempDir(), "iphone", enrollment, nil) }()
	<-entered
	if got := decode(t, OpenDoor(doorAddress, 6446, heard)); got["code"] != "joining" {
		t.Fatalf("OpenDoor during a join = %v, want code joining", got)
	}
	Down()
	<-joined // the join has let go of its claim on the package before the next ask
	if got := decode(t, OpenDoor(doorAddress, 6446, heard)); got["code"] != "node_down" {
		t.Fatalf("OpenDoor after Down = %v, want code node_down", got)
	}
	if frames, closes := heard.heard(); len(frames)+len(closes) != 0 {
		t.Fatal("a refused OpenDoor called its listener")
	}
}

func TestDownClosesEveryDoor(t *testing.T) {
	fake := newFakeDoor(t, readAll)
	first, second := newRecorder(), newRecorder()
	open(t, fake, first)
	open(t, fake, second)

	Down()
	for _, heard := range []*recorder{first, second} {
		if reason := heard.waitClosed(t); reason != "node_down" {
			t.Fatalf("OnClose reason = %q, want node_down", reason)
		}
	}
}

// Down while a door is still dialling: closeDoors has already run, and the
// node the door was dialled over is gone. The door must not open after it.
func TestADoorDialledAcrossDownDoesNotOpen(t *testing.T) {
	ended := make(chan struct{})
	fake := newFakeDoor(t, func(ctx context.Context, conn *websocket.Conn) {
		readAll(ctx, conn)
		close(ended)
	})
	var up atomic.Bool
	up.Store(true)
	dial := func(ctx context.Context, dst netip.AddrPort) (net.Conn, error) {
		conn, err := fake.dial(ctx, dst)
		up.Store(false)            // Down: the node is no longer current…
		closeDoors(reasonNodeDown) // …and its doors are closed
		return conn, err
	}
	t.Cleanup(func() { closeDoors("test_over") })
	heard := newRecorder()

	if got := decode(t, openDoor(dial, func(string, ...any) {}, doorAddress, 6446, heard, up.Load)); got["code"] != "node_down" {
		t.Fatalf("OpenDoor across Down = %v, want code node_down", got)
	}
	if n := openDoors(); n != 0 {
		t.Fatalf("%d door(s) open after Down", n)
	}
	select {
	case <-ended:
	case <-time.After(5 * time.Second):
		t.Fatal("the door's connection was left open")
	}
	if frames, closes := heard.heard(); len(frames)+len(closes) != 0 {
		t.Fatal("a door that never opened called its listener")
	}
}

func TestOpenDoorRefusesWhatIsNotADoor(t *testing.T) {
	fake := newFakeDoor(t, readAll)
	for name, c := range map[string]struct {
		address  string
		port     int
		listener FrameListener
	}{
		"a v4 address": {"10.0.0.1", 6446, newRecorder()},
		"no port":      {doorAddress, 0, newRecorder()},
		"too big":      {doorAddress, 70000, newRecorder()},
		"no listener":  {doorAddress, 6446, nil},
	} {
		t.Run(name, func(t *testing.T) {
			if got := decode(t, openDoor(fake.dial, func(string, ...any) {}, c.address, c.port, c.listener, alwaysUp)); got["code"] != "malformed" {
				t.Fatalf("OpenDoor = %v, want code malformed", got)
			}
		})
	}
}

// gonet, the netstack's net.Conn, reports a reset as a bare string rather
// than ECONNRESET, and net/http a hang-up before the request as "server
// closed idle connection", so dialCode matches their words too.
func TestDialCodeNamesAHangUpDroppedHoweverItIsSpelled(t *testing.T) {
	for name, err := range map[string]error{
		"EOF":                    io.EOF,
		"unexpected EOF":         io.ErrUnexpectedEOF,
		"ECONNRESET":             fmt.Errorf("read: %w", syscall.ECONNRESET),
		"gonet's reset":          errors.New("connection reset by peer"),
		"gonet's reset wrapped":  fmt.Errorf("failed to WebSocket dial: %w", errors.New("connection reset by peer")),
		"net/http's closed idle": fmt.Errorf("failed to send handshake request: %w", errors.New("http: server closed idle connection")),
	} {
		t.Run(name, func(t *testing.T) {
			if code := dialCode(nil, err); code != codeDropped {
				t.Fatalf("dialCode(%v) = %s, want %s", err, code, codeDropped)
			}
		})
	}
	if code := dialCode(nil, errors.New("no route to host")); code != codeUnreachable {
		t.Fatalf("dialCode(no route) = %s, want %s", code, codeUnreachable)
	}
}

// Down while the door is still dialling, and the dial fails because the node
// closed under it: what the app is told is that the node went down, not that
// the door was unreachable.
func TestADownThatBreaksADialIsNodeDown(t *testing.T) {
	var up atomic.Bool
	up.Store(true)
	dial := func(context.Context, netip.AddrPort) (net.Conn, error) {
		up.Store(false)
		closeDoors(reasonNodeDown)
		return nil, errors.New("no route to host") // the engine closed under the dial
	}
	if got := decode(t, openDoor(dial, func(string, ...any) {}, doorAddress, 6446, newRecorder(), up.Load)); got["code"] != "node_down" {
		t.Fatalf("OpenDoor across Down = %v, want code node_down", got)
	}
}

// Close and Down are the app's calls, often from the main thread, and the
// app's Logger hops to the main thread. Neither may wait on a Log.
func TestCloseAndDownReturnWhileTheLoggerIsBusy(t *testing.T) {
	down(t)
	block := make(chan struct{})
	log := &lineLogger{block: block}
	logf := logger(log)
	fake := newFakeDoor(t, readAll)
	t.Cleanup(func() { closeDoors("test_over") })
	release := sync.OnceFunc(func() { close(block) })
	t.Cleanup(release)

	var first, second string
	returnsPromptly(t, "OpenDoor", func() {
		first, _ = decode(t, openDoor(fake.dial, logf, doorAddress, 6446, newRecorder(), alwaysUp))["door_id"].(string)
		second, _ = decode(t, openDoor(fake.dial, logf, doorAddress, 6446, newRecorder(), alwaysUp))["door_id"].(string)
	})
	if first == "" || second == "" {
		t.Fatal("the doors did not open")
	}
	returnsPromptly(t, "Close", func() { Close(first) })
	returnsPromptly(t, "Down", func() { Down() })

	release()
	eventually(t, "the held lines heard", func() bool { lines, _ := log.heard(); return len(lines) >= 4 })
	if _, from := log.heard(); from[goroutineID()] {
		t.Fatal("Log was called on the test's own goroutine")
	}
}
