package main

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gentility-ai/gentility/connector/phonenode"
)

const (
	helloReply      = `{"id":1,"response":{"type":"hello","version":"0.1.279","build":"522555a14","capabilities":[]}}`
	transcriptReply = `{"id":2,"response":{"type":"transcript","items":[]}}`
	saidEvent       = `{"event":{"type":"said","session_id":"s1","turn":3,"said":{}}}`
)

// fakeDoors stands in for phonenode's door half: it answers what it is sent
// by calling the listener, as the real door's frames would arrive.
type fakeDoors struct {
	mu       sync.Mutex
	opened   string
	sent     []string
	answer   func(listener phonenode.FrameListener, frame string)
	listener phonenode.FrameListener
}

func (f *fakeDoors) OpenDoor(_ string, _ int, listener phonenode.FrameListener) string {
	f.listener = listener
	return f.opened
}

func (f *fakeDoors) Send(_, frame string) string {
	f.mu.Lock()
	f.sent = append(f.sent, frame)
	f.mu.Unlock()
	if f.answer != nil {
		go f.answer(f.listener, frame)
	}
	return `{"ok":true}`
}

func (f *fakeDoors) Close(string) string { return `{"ok":true}` }

func options() talkOptions {
	return talkOptions{
		Address:     "fd06:6e7:0:2::4",
		Port:        6446,
		Session:     "s1",
		LastTurns:   20,
		HelloWithin: 50 * time.Millisecond,
		Watch:       100 * time.Millisecond,
	}
}

const openedInOrg = `{"door_id":"door-1","source":"[fd06:6e7:0:2::9]:40001"}`

// Review focus 1, from the harness's side.
func TestTalkGivesUpOnAHelloNeverAnswered(t *testing.T) {
	doors := &fakeDoors{opened: openedInOrg}
	var out bytes.Buffer
	err := talk(context.Background(), doors, &out, options())
	if err == nil || !strings.Contains(err.Error(), "did not answer within 50ms") {
		t.Fatalf("talk = %v", err)
	}
	if len(doors.sent) != 1 || doors.sent[0] != `{"id":1,"request":{"type":"hello","wants":["view"]}}` {
		t.Fatalf("sent %q, want the hello alone", doors.sent)
	}
}

func TestTalkSaysHelloThenAsksForTheTranscriptAndPrintsInOrder(t *testing.T) {
	doors := &fakeDoors{opened: openedInOrg}
	doors.answer = func(listener phonenode.FrameListener, frame string) {
		switch {
		case strings.Contains(frame, `"hello"`):
			listener.OnFrame("door-1", helloReply)
		case strings.Contains(frame, `"session_transcript"`):
			listener.OnFrame("door-1", transcriptReply)
			listener.OnFrame("door-1", saidEvent)
		}
	}
	var out bytes.Buffer
	if err := talk(context.Background(), doors, &out, options()); err != nil {
		t.Fatalf("talk: %v\n%s", err, out.String())
	}
	if want := `{"id":2,"request":{"last_turns":20,"session_id":"s1","type":"session_transcript"}}`; doors.sent[1] != want {
		t.Fatalf("asked %s, want %s", doors.sent[1], want)
	}
	text := out.String()
	hello, transcript, said := strings.Index(text, helloReply), strings.Index(text, transcriptReply), strings.Index(text, saidEvent)
	if hello < 0 || transcript < hello || said < transcript {
		t.Fatalf("frames out of order or missing:\n%s", text)
	}
	if !strings.Contains(text, "is in the door's organisation") {
		t.Fatalf("the source line is missing:\n%s", text)
	}
}

func TestTalkSaysTheDoorClosedBeforeHello(t *testing.T) {
	doors := &fakeDoors{opened: openedInOrg}
	doors.answer = func(listener phonenode.FrameListener, _ string) {
		listener.OnClose("door-1", "door_closed: 1008 say hello first")
	}
	var out bytes.Buffer
	err := talk(context.Background(), doors, &out, options())
	if err == nil || !strings.Contains(err.Error(), "the door closed: door_closed: 1008") {
		t.Fatalf("talk = %v", err)
	}
}

func TestTalkSaysWhenTheDoorDidNotOpen(t *testing.T) {
	doors := &fakeDoors{opened: `{"error":"the door at [fd06:6e7:0:2::4]:6446: …","code":"dropped"}`}
	var out bytes.Buffer
	err := talk(context.Background(), doors, &out, options())
	if err == nil || !strings.Contains(err.Error(), "dropped") {
		t.Fatalf("talk = %v", err)
	}
}

func TestSameOrganisationComparesTheSlash64(t *testing.T) {
	if got := sameOrganisation("fd06:6e7:0:2::4", "[fd06:6e7:0:2::9]:40001"); !strings.Contains(got, "is in the door's organisation") {
		t.Errorf("same /64: %s", got)
	}
	if got := sameOrganisation("fd06:6e7:0:2::4", "[fd06:6e7:0:3::9]:40001"); !strings.Contains(got, "is NOT in the door's organisation") {
		t.Errorf("another /64: %s", got)
	}
}

// A Mac's door is on its 6net address; anything else is refused before the
// node comes up.
func TestDoorRefusesAnAddressThatIsNotIPv6(t *testing.T) {
	for _, address := range []string{"100.64.0.4", "::ffff:100.64.0.4", "driftwood.local", "[fd06:6e7:0:2::4]:6446"} {
		dir := filepath.Join(t.TempDir(), "state")
		if err := writePaired(dir, paired{Origin: "https://mcp.example", PhoneName: "harness"}); err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		err := doorCommand(context.Background(), []string{"--state", dir, "--address", address, "--session", "s1"}, &out)
		if err == nil || !strings.Contains(err.Error(), "is not an IPv6 address") {
			t.Errorf("door --address %s = %v", address, err)
		}
		if strings.Contains(out.String(), "rejoin:") {
			t.Errorf("door --address %s rejoined before refusing it:\n%s", address, out.String())
		}
	}
}

func TestDoorTakesA6netAddress(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	if err := writePaired(dir, paired{Origin: "https://mcp.example", PhoneName: "harness"}); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	err := doorCommand(context.Background(), []string{"--state", dir, "--address", "fd06:6e7:0:2::4", "--session", "s1"}, &out)
	if err == nil || strings.Contains(err.Error(), "not an IPv6 address") || !strings.Contains(out.String(), "rejoin:") {
		t.Fatalf("door = %v, want it past the address to the rejoin (unenrolled here)\n%s", err, out.String())
	}
}
