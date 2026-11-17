// Package status is `connector status`: the four questions a
// person standing at a machine must be able to answer, and nothing else.
//
//   - Is this machine enrolled at all?
//   - Has it joined the overlay?
//   - Is the daemon running?
//   - What does it hold, and what is it serving?
//
// It answers all four from the state directory, because it has nothing else to
// answer from: `status` is a separate process from `serve`, so there is no live
// tsnet node to interrogate and no config cursor to read — config.Client's
// restarts at NoRevision in every process by design. What the daemon knows, the
// daemon writes down (state.Status), and this package composes that with the
// identity and the enrollment beside it.
//
// **None of the four is an error.** "Not enrolled" is the first step, not a
// failure, and a person who typed `status` to find out has not done anything
// wrong — so every one of them exits 0, and only a state directory that cannot
// be read is a non-zero.
//
// Snapshot is a plain struct and Render is pure. The clock arrives as an
// argument for the same reason: an age is the load-bearing number here, and one
// that could not be fixed could not be tested byte for byte.
package status

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/gentility-ai/gentility/connector/internal/state"
	"github.com/gentility-ai/gentility/connector/internal/version"
)

// StaleAfter is how old a snapshot has to be before it is presented as one
// nobody is refreshing.
//
// Three heartbeats (report.DefaultInterval is thirty seconds), because the
// daemon rewrites the snapshot on every beat as well as on every document: two
// would flag a node that lost a single beat to a slow network, and anything
// longer would present a dead daemon's document as the current one for minutes.
const StaleAfter = 90 * time.Second

// State is which of the four answers a snapshot is. They are ordered as a
// machine passes through them.
type State string

const (
	// NotEnrolled: nothing on disk. The answer is `gentility net join` for a
	// person's machine and `connector login --server` for a server.
	NotEnrolled State = "not_enrolled"
	// Enrolled: material earned and never spent. The answer is to start the
	// daemon, which is what exchanges it.
	Enrolled State = "enrolled"
	// Joined: an identity, and no snapshot beside it. The node is on the
	// overlay and the daemon is not running.
	Joined State = "joined"
	// Live: an identity and a snapshot. Everything below is populated.
	Live State = "live"
	// Unreadable: an identity this process may not open and no snapshot it
	// can read, which is a person without sudo reading a daemon that has
	// written nothing they may see. Whether the node joined is the daemon's to
	// say, and it has said nothing readable, so this does not claim Joined.
	Unreadable State = "unreadable"
)

// Snapshot is the whole of what `connector status` says, in one struct, so that
// `--json` and the rendered block cannot disagree.
//
// Fields below Revision are meaningful only in the state that fills them; the
// State field says which one that is, and Render reads nothing a state did not
// fill. Revision is state.NoRevision where there is no document, never zero:
// revision 0 is a real document.
type Snapshot struct {
	State      State  `json:"state"`
	Connector  string `json:"connector"`
	NodeCapver int    `json:"node_capver"`

	// The identity, from node.json.
	NodeID   string `json:"node_id,omitempty"`
	Hostname string `json:"hostname,omitempty"`
	HeadURL  string `json:"head_url,omitempty"`

	// EnrollmentID is the unspent enrollment's, and only Enrolled has one.
	EnrollmentID string `json:"enrollment_id,omitempty"`

	Revision int64 `json:"revision"`

	// Age is how long ago the daemon wrote the snapshot, measured against the
	// clock Load was handed. Stale says that is long enough to doubt it.
	//
	// It stays out of the wire form under its own name: a time.Duration
	// marshals as nanoseconds, and MarshalJSON restates it as age_seconds.
	Age   time.Duration `json:"-"`
	Stale bool          `json:"stale,omitempty"`

	// Name is this machine's 6net name, as the daemon last wrote it:
	// cedar.morgan.northwind.6net.io.
	Name string `json:"name,omitempty"`

	Relayed         bool             `json:"relayed,omitempty"`
	Endpoint        string           `json:"endpoint,omitempty"`
	Interface       string           `json:"interface,omitempty"`
	PeerPaths       []state.PeerPath `json:"peer_paths"`
	PathsIncomplete bool             `json:"paths_incomplete,omitempty"`

	Organizations []state.OrgStatus     `json:"organizations,omitempty"`
	Services      []state.ServiceStatus `json:"services,omitempty"`
	Egress        *state.EgressStatus   `json:"egress,omitempty"`

	// Partial says an identity file is there and this process may not read
	// it: a person reading the daemon's directory, where node.json and
	// enrollment.json are root's and 0600. The state is still right — the
	// file's existence is the answer — and the fields only it carries are
	// empty.
	Partial bool `json:"partial,omitempty"`
}

// plainSnapshot is Snapshot without its methods, so the two below can hand the
// struct to encoding/json without calling themselves.
type plainSnapshot Snapshot

// wireSnapshot is how a Snapshot travels: every field of the struct, plus the
// age restated as whole seconds — the unit the rest of this package measures
// in, rather than the nanoseconds a Duration would print.
type wireSnapshot struct {
	plainSnapshot
	AgeSeconds int64 `json:"age_seconds,omitempty"`
}

func (s Snapshot) MarshalJSON() ([]byte, error) {
	return json.Marshal(wireSnapshot{
		plainSnapshot: plainSnapshot(s),
		AgeSeconds:    int64(s.Age / time.Second),
	})
}

// UnmarshalJSON is the other half of it: `--json` has to read back as what it
// printed, and seconds only become a Duration again if something multiplies.
func (s *Snapshot) UnmarshalJSON(data []byte) error {
	var wire wireSnapshot
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	*s = Snapshot(wire.plainSnapshot)
	s.Age = time.Duration(wire.AgeSeconds) * time.Second
	return nil
}

// Load composes the snapshot from the three files that can be in a state
// directory, taking the first door that opens: an identity makes this a joined
// node, a snapshot beside it makes it a live one, and neither with an
// enrollment present makes it a machine waiting for its first start.
//
// now is the clock. It is a parameter and not a package variable so that a
// caller can fix it, which is the only way an age is testable.
//
// The only error it returns is a state directory it cannot read. Every absence
// is an answer, and so is an identity file it may not open: the snapshot is
// marked Partial and built from what it can read, because status.json is 0644
// precisely so that a person can read the daemon's without sudo.
func Load(store *state.Store, now time.Time) (Snapshot, error) {
	snapshot := Snapshot{
		State:      NotEnrolled,
		Connector:  version.Connector,
		NodeCapver: version.NodeCapver,
		Revision:   state.NoRevision,
	}

	node, err := loadNode(store)
	switch {
	case state.IsNotEnrolled(err):
		return enrolling(store, snapshot)
	case errors.Is(err, fs.ErrPermission):
		snapshot.Partial = true
	case err != nil:
		return Snapshot{}, err
	default:
		snapshot.NodeID = node.NodeID
		snapshot.Hostname = node.Hostname
		snapshot.HeadURL = node.HeadURL
		snapshot.Revision = node.LastRevision
	}
	snapshot.State = Joined

	last, err := loadStatus(store)
	switch {
	case snapshot.Partial && (state.IsNoStatus(err) || errors.Is(err, fs.ErrPermission)):
		snapshot.State = Unreadable
		return snapshot, nil
	case state.IsNoStatus(err):
		return snapshot, nil
	case err != nil:
		return Snapshot{}, err
	}

	// The snapshot's revision and not node.json's cursor: the daemon writes down
	// the document it just applied, where the cursor is only what a restart
	// resumes from.
	snapshot.State = Live
	snapshot.Revision = last.Revision
	snapshot.Age = age(last.WrittenAt, now)
	snapshot.Stale = snapshot.Age >= StaleAfter
	snapshot.Name = last.Name
	snapshot.Relayed = last.Relayed
	snapshot.Endpoint = last.Endpoint
	snapshot.PeerPaths = last.PeerPaths
	snapshot.PathsIncomplete = last.PathsIncomplete
	snapshot.Interface = last.Interface
	snapshot.Organizations = last.Organizations
	snapshot.Services = last.Services
	snapshot.Egress = last.Egress

	return snapshot, nil
}

// loadNode and loadEnrollment are the Store's readers, held in variables so a
// test can be refused the way a person is refused a root-owned 0600 file. A
// chmod would not do it: root reads through one, and tests may run as root.
var (
	loadNode       = (*state.Store).LoadNode
	loadEnrollment = (*state.Store).LoadEnrollment
	loadStatus     = (*state.Store).LoadStatus
)

// enrolling is the pre-identity half: material a person earned and the daemon
// has not spent, or nothing at all.
func enrolling(store *state.Store, snapshot Snapshot) (Snapshot, error) {
	enrollment, err := loadEnrollment(store)
	switch {
	case state.IsNoEnrollment(err):
		return snapshot, nil
	case errors.Is(err, fs.ErrPermission):
		snapshot.State = Enrolled
		snapshot.Partial = true
		return snapshot, nil
	case err != nil:
		return Snapshot{}, err
	}

	snapshot.State = Enrolled
	snapshot.EnrollmentID = enrollment.EnrollmentID
	snapshot.HeadURL = enrollment.HeadURL
	return snapshot, nil
}

// age is how long ago the daemon wrote, to the second. A snapshot dated ahead
// of the clock is a clock that moved, not a snapshot from the future, so it
// reads as new rather than as a negative.
func age(written, now time.Time) time.Duration {
	since := now.Sub(written)
	if since < 0 {
		return 0
	}
	return since.Truncate(time.Second)
}

// Render is the block a person reads. Pure, and the only thing that decides
// what a state shows.
func Render(snapshot Snapshot) string {
	var b strings.Builder
	fmt.Fprintf(&b, "connector %s (node_capver %d)\n", snapshot.Connector, snapshot.NodeCapver)

	switch snapshot.State {
	case NotEnrolled:
		b.WriteString(`
this machine is not enrolled in any organization.

join 6net as yourself:

  gentility net join

or enrol it as a server — the organization is chosen in the browser:

  connector login --server
`)

	case Enrolled:
		if snapshot.Partial {
			needsSudo(&b)
		} else {
			field(&b, "enrollment", snapshot.EnrollmentID)
			field(&b, "head", snapshot.HeadURL)
		}
		b.WriteString(`
this machine is enrolled and has not joined the overlay yet.

the daemon has not written a status yet. On a server, start it, which is what
spends the enrollment:

  connector serve
`)

	case Joined:
		identity(&b, snapshot)
		field(&b, "config", applied(snapshot))
		b.WriteString(`
the daemon has not written a status yet. On a server, start it:

  connector serve
`)

	case Live:
		if snapshot.Name != "" {
			field(&b, "name", snapshot.Name)
		}
		identity(&b, snapshot)
		field(&b, "config", freshness(snapshot))
		field(&b, "peers", path(snapshot))
		// Only in interface mode: a userspace node is on no device, and a line
		// that said so would be noise on every machine that never asked.
		if snapshot.Interface != "" {
			field(&b, "interface", snapshot.Interface)
		}
		if len(snapshot.PeerPaths) > 0 {
			rows := make([][]string, 0, len(snapshot.PeerPaths))
			for _, peer := range snapshot.PeerPaths {
				rows = append(rows, []string{peer.Address, peerPath(peer)})
			}
			section(&b, "peer paths", rows, "")
		}
		section(&b, "organizations", organizations(snapshot), "none granted")
		section(&b, "services", services(snapshot), "none advertised")
		section(&b, "egress", egress(snapshot), "no relay selected")

	case Unreadable:
		field(&b, "note", "the daemon's status could not be read")
		b.WriteString(`
this machine holds an identity this account may not open, and the daemon has
written no status it can read. Ask as root:

  sudo gentility status
`)

	default:
		fmt.Fprintf(&b, "\nthis connector cannot describe the state %q.\n", snapshot.State)
	}

	return b.String()
}

// identity is the two lines every state that has an identity opens with, or
// the one line saying why they are missing.
func identity(b *strings.Builder, snapshot Snapshot) {
	if snapshot.Partial {
		needsSudo(b)
		return
	}
	name := snapshot.NodeID
	if snapshot.Hostname != "" {
		name += "  (" + snapshot.Hostname + ")"
	}
	field(b, "node", name)
	field(b, "head", snapshot.HeadURL)
}

// needsSudo stands where the lines from a file this process may not read
// would be.
func needsSudo(b *strings.Builder) { field(b, "note", "identity details need sudo") }

// applied is the config line of a node whose daemon is not running: what it
// last got to, with no claim about now.
func applied(snapshot Snapshot) string {
	if snapshot.Revision == state.NoRevision {
		return "no document applied yet"
	}
	return fmt.Sprintf("revision %d, last applied", snapshot.Revision)
}

// freshness is the config line of a live node: the document, and how long ago
// the daemon said so.
func freshness(snapshot Snapshot) string {
	return fmt.Sprintf("revision %d, as of %s ago", snapshot.Revision, snapshot.Age)
}

// path summarizes observed peers, never the legacy home-relay flag.
func path(snapshot Snapshot) string {
	if snapshot.PeerPaths == nil {
		line := "peer paths unavailable — update the Connector"
		if snapshot.Stale {
			line += " (stale observations — is the daemon still running?)"
		}
		return line
	}
	counts := map[string]int{}
	for _, peer := range snapshot.PeerPaths {
		switch peer.Path {
		case "direct", "idle":
			counts[peer.Path]++
		case "derp", "peer_relay":
			counts["relay"]++
		default:
			counts["unknown"]++
		}
	}
	var parts []string
	for _, kind := range []string{"direct", "relay", "idle", "unknown"} {
		if counts[kind] > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", counts[kind], kind))
		}
	}
	line := strings.Join(parts, ", ")
	if line == "" {
		line = "no peers observed"
	}
	if snapshot.PathsIncomplete {
		line += " (incomplete)"
	}
	if snapshot.Stale {
		line += " (stale observations — is the daemon still running?)"
	}
	return line
}

func peerPath(peer state.PeerPath) string {
	switch peer.Path {
	case "direct", "idle":
		return peer.Path
	case "derp":
		if peer.Relay != "" {
			return "relay (" + peer.Relay + ")"
		}
		return "relay"
	case "peer_relay":
		return "peer relay"
	default:
		return "unknown"
	}
}

func organizations(snapshot Snapshot) [][]string {
	rows := make([][]string, 0, len(snapshot.Organizations))
	for _, org := range snapshot.Organizations {
		rows = append(rows, []string{org.Org, org.Address, org.Label})
	}
	return rows
}

func services(snapshot Snapshot) [][]string {
	rows := make([][]string, 0, len(snapshot.Services))
	for _, service := range snapshot.Services {
		rows = append(rows, []string{
			service.Kind, ":" + strconv.Itoa(service.Port), service.State, service.Org,
		})
	}
	return rows
}

func egress(snapshot Snapshot) [][]string {
	if snapshot.Egress == nil {
		return nil
	}
	// JoinHostPort rather than a colon, so an IPv6 relay does not read as an
	// address with one more group than it has.
	via := net.JoinHostPort(snapshot.Egress.Via, strconv.Itoa(snapshot.Egress.Port))
	return [][]string{{snapshot.Egress.Org, "via " + via}}
}

// fieldWidth is the label column of the header lines. It is one wider than the
// longest label, so `enrollment` has a space after it like the rest.
const fieldWidth = 11

func field(b *strings.Builder, label, value string) {
	fmt.Fprintf(b, "%-*s%s\n", fieldWidth, label, value)
}

// section writes one titled block, or the sentence that says it is empty. An
// empty section is printed rather than dropped: "no relay selected" is a
// refusal a person needs to see, and a missing heading reads as a bug.
func section(b *strings.Builder, title string, rows [][]string, empty string) {
	fmt.Fprintf(b, "\n%s\n", title)
	if len(rows) == 0 {
		fmt.Fprintf(b, "  %s\n", empty)
		return
	}
	for _, line := range align(rows) {
		fmt.Fprintf(b, "  %s\n", line)
	}
}

// align pads each column to the widest cell in it, so a block of addresses
// reads as a table. The last cell is never padded and no line ends in a space —
// trailing whitespace is invisible until it is in a diff.
func align(rows [][]string) []string {
	var widths []int
	for _, row := range rows {
		for i, cell := range row {
			for len(widths) <= i {
				widths = append(widths, 0)
			}
			if len(cell) > widths[i] {
				widths[i] = len(cell)
			}
		}
	}

	lines := make([]string, 0, len(rows))
	for _, row := range rows {
		var line strings.Builder
		for i, cell := range row {
			if i == len(row)-1 {
				line.WriteString(cell)
				break
			}
			fmt.Fprintf(&line, "%-*s  ", widths[i], cell)
		}
		lines = append(lines, strings.TrimRight(line.String(), " "))
	}
	return lines
}
