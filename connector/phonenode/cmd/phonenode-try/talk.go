package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/netip"
	"time"

	"github.com/gentility-ai/gentility/connector/phonenode"
)

// doorAPI is phonenode's door half as the door command uses it; a test
// stands in for it.
type doorAPI interface {
	OpenDoor(address string, port int, listener phonenode.FrameListener) string
	Send(doorID, frame string) string
	Close(doorID string) string
}

type liveDoors struct{}

func (liveDoors) OpenDoor(address string, port int, listener phonenode.FrameListener) string {
	return phonenode.OpenDoor(address, port, listener)
}
func (liveDoors) Send(doorID, frame string) string { return phonenode.Send(doorID, frame) }
func (liveDoors) Close(doorID string) string       { return phonenode.Close(doorID) }

// printer is the node's Logger: its lines, indented under the command's.
type printer struct{ out io.Writer }

func (p printer) Log(line string) { fmt.Fprintln(p.out, "  log:", line) }

func failed(answer string) bool {
	var a struct {
		Error string `json:"error"`
	}
	return json.Unmarshal([]byte(answer), &a) != nil || a.Error != ""
}

func joinCommand(args []string, out io.Writer) error {
	flags := flag.NewFlagSet("join", flag.ContinueOnError)
	dir := flags.String("state", "", "the directory claim wrote phone.json to")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *dir == "" {
		return errors.New("join needs --state")
	}
	p, err := readPaired(*dir)
	if err != nil {
		return err
	}
	if len(p.Enrollment) == 0 {
		return errors.New("phone.json holds no join material: an earlier join spent it; run door, or claim again")
	}

	joined := phonenode.Join(nodeDir(*dir), p.PhoneName, string(p.Enrollment), printer{out})
	fmt.Fprintln(out, "join:", joined)
	if failed(joined) {
		return errors.New("the join failed")
	}
	defer func() { fmt.Fprintln(out, "down:", phonenode.Down()) }()
	fmt.Fprintln(out, "status:", phonenode.Status())

	return spendEnrollment(*dir, p)
}

// spendEnrollment drops the join material from phone.json once a join has
// used it: the pre-auth key is single-use, and the node's identity is in
// nodeDir now, which is what door rejoins on.
func spendEnrollment(dir string, p paired) error {
	p.Enrollment = nil
	if err := writePaired(dir, p); err != nil {
		return fmt.Errorf("the node joined, but the spent join material was left in phone.json, which could not be rewritten (%w): run door, not join, next", err)
	}
	return nil
}

func doorCommand(ctx context.Context, args []string, out io.Writer) error {
	flags := flag.NewFlagSet("door", flag.ContinueOnError)
	dir := flags.String("state", "", "the directory join enrolled the node in")
	address := flags.String("address", "", "the Mac's 6net address in the organisation to dial in")
	port := flags.Int("port", 6446, "the door's port (the Mac's Desk listens on 6446)")
	session := flags.String("session", "", "the session to ask the transcript of (Desk: Copy session id)")
	lastTurns := flags.Int("last-turns", 20, "how many turns of transcript to ask for")
	watch := flags.Duration("watch", 60*time.Second, "how long to print what the door says after the transcript")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *dir == "" || *address == "" || *session == "" {
		return errors.New("door needs --state, --address and --session")
	}
	// The door listens on the Mac's 6net address alone, and 6net is IPv6.
	if a, err := netip.ParseAddr(*address); err != nil || !a.Is6() || a.Is4In6() {
		return fmt.Errorf("--address %q is not an IPv6 address: give the Mac's 6net address (fd06:6e7:…), without brackets or a port", *address)
	}
	p, err := readPaired(*dir)
	if err != nil {
		return err
	}

	rejoined := phonenode.Rejoin(nodeDir(*dir), p.PhoneName, printer{out})
	fmt.Fprintln(out, "rejoin:", rejoined)
	if failed(rejoined) {
		return errors.New("the rejoin failed")
	}
	defer func() { fmt.Fprintln(out, "down:", phonenode.Down()) }()
	fmt.Fprintln(out, "status:", phonenode.Status())

	return talk(ctx, liveDoors{}, out, talkOptions{
		Address:     *address,
		Port:        *port,
		Session:     *session,
		LastTurns:   *lastTurns,
		HelloWithin: 5 * time.Second, // the window's own client waits this long
		Watch:       *watch,
	})
}

type talkOptions struct {
	Address     string
	Port        int
	Session     string
	LastTurns   int
	HelloWithin time.Duration
	Watch       time.Duration
}

// heard is the listener: frames and the close on one channel, so they are
// read in the order phonenode delivered them.
type heard struct{ events chan heardEvent }

type heardEvent struct {
	frame  string
	closed bool
	reason string
}

func (h *heard) OnFrame(_ string, frame string) { h.events <- heardEvent{frame: frame} }
func (h *heard) OnClose(_ string, reason string) {
	h.events <- heardEvent{closed: true, reason: reason}
}

func talk(ctx context.Context, doors doorAPI, out io.Writer, o talkOptions) error {
	h := &heard{events: make(chan heardEvent, 1024)}
	opened := doors.OpenDoor(o.Address, o.Port, h)
	fmt.Fprintln(out, "open:", opened)
	var door struct {
		DoorID string `json:"door_id"`
		Source string `json:"source"`
	}
	if err := json.Unmarshal([]byte(opened), &door); err != nil || door.DoorID == "" {
		return fmt.Errorf("the door did not open: %s", opened)
	}
	defer doors.Close(door.DoorID)
	fmt.Fprintln(out, sameOrganisation(o.Address, door.Source))

	// A personal host's door takes a hello with no token (pairing.rs: only a
	// bound host asks for one); view is all the phone's first slice reads.
	if err := ask(doors, door.DoorID, 1, map[string]any{"type": "hello", "wants": []string{"view"}}); err != nil {
		return err
	}
	if _, err := await(ctx, h, out, 1, o.HelloWithin); err != nil {
		return fmt.Errorf("hello: %w", err)
	}
	if err := ask(doors, door.DoorID, 2, map[string]any{
		"type": "session_transcript", "session_id": o.Session, "last_turns": o.LastTurns,
	}); err != nil {
		return err
	}
	if _, err := await(ctx, h, out, 2, 15*time.Second); err != nil {
		return fmt.Errorf("session_transcript: %w", err)
	}

	fmt.Fprintf(out, "watching for %s: say something in the session on the Mac\n", o.Watch)
	return watch(ctx, h, out, o.Watch)
}

func ask(doors doorAPI, doorID string, id uint64, request map[string]any) error {
	frame, err := json.Marshal(map[string]any{"id": id, "request": request})
	if err != nil {
		return err
	}
	if sent := doors.Send(doorID, string(frame)); failed(sent) {
		return fmt.Errorf("send: %s", sent)
	}
	return nil
}

type replied struct {
	ID       *uint64 `json:"id"`
	Response struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"response"`
}

// await prints frames until the reply to id, the door's close, or within.
func await(ctx context.Context, h *heard, out io.Writer, id uint64, within time.Duration) (replied, error) {
	timer := time.NewTimer(within)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return replied{}, ctx.Err()
		case <-timer.C:
			return replied{}, fmt.Errorf("the door did not answer within %s", within)
		case e := <-h.events:
			if e.closed {
				return replied{}, fmt.Errorf("the door closed: %s", e.reason)
			}
			show(out, e.frame)
			var r replied
			if json.Unmarshal([]byte(e.frame), &r) == nil && r.ID != nil && *r.ID == id {
				if r.Response.Type == "error" {
					return r, fmt.Errorf("the door refused: %s", r.Response.Message)
				}
				return r, nil
			}
		}
	}
}

func watch(ctx context.Context, h *heard, out io.Writer, within time.Duration) error {
	timer := time.NewTimer(within)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-timer.C:
			return nil
		case e := <-h.events:
			if e.closed {
				return fmt.Errorf("the door closed: %s", e.reason)
			}
			show(out, e.frame)
		}
	}
}

func show(out io.Writer, frame string) {
	const most = 600
	if len(frame) > most {
		fmt.Fprintf(out, "<- %s… (%d bytes)\n", frame[:most], len(frame))
		return
	}
	fmt.Fprintf(out, "<- %s\n", frame)
}

// sameOrganisation says whether the dial left from the door's organisation:
// the /64 is what an organisation is on the wire (tsnode.OrgPrefix).
func sameOrganisation(address, source string) string {
	door, err := netip.ParseAddr(address)
	if err != nil {
		return fmt.Sprintf("source %s: the door address %q does not parse", source, address)
	}
	from, err := netip.ParseAddrPort(source)
	if err != nil {
		return fmt.Sprintf("source %q does not parse", source)
	}
	want, _ := door.Prefix(64)
	if got, _ := from.Addr().Prefix(64); got == want {
		return fmt.Sprintf("source %s is in the door's organisation (%s)", source, want)
	}
	return fmt.Sprintf("source %s is NOT in the door's organisation (%s)", source, want)
}
