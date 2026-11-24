package state

import (
	"errors"
	"os"
	"time"
)

// statusFile is what the daemon leaves behind for `connector status` to read.
//
// `status` is a SEPARATE PROCESS from `serve`. It cannot ask the live tsnet
// node anything, and config.Client's cursor deliberately restarts at NoRevision
// in every new process, so there is no way for one to see what the other holds
// except for the daemon to write it down. This file is that writing down, and
// the reason `status` needs no IPC.
//
// It is observability and nothing else: no secret goes in it, nothing reads it
// back to decide anything, and a write that fails is a line in the log rather
// than a node that stops serving. That is why it alone is 0644: the daemon
// writes it as root and the person reads it as themselves. It shares the
// directory and the atomic write with the rest of the state, because there is
// no second discipline worth having.
const statusFile = "status.json"

// ErrNoStatus reports that no snapshot has been written: `serve` has never run
// against this state directory, or has not applied a document yet.
//
// It is a state and not a failure, and it is deliberately none of the other
// three absences — ErrNotLoggedIn is answered by logging in, ErrNoEnrollment by
// `login --server`, ErrNotEnrolled by exchanging, and this one by starting the
// daemon.
var ErrNoStatus = errors.New("no status snapshot")

// IsNoStatus reports whether err means "no snapshot written".
func IsNoStatus(err error) bool { return errors.Is(err, ErrNoStatus) }

// Status is what the daemon was doing the last time it said so: the document it
// holds, the path it holds it over, and the instant it wrote that down.
//
// WrittenAt is the load-bearing field. Everything else is a fact about a moment
// that has passed, and it is the age of that moment which tells a reader
// whether the daemon is still there at all.
type Status struct {
	WrittenAt time.Time `json:"written_at"`
	Revision  int64     `json:"revision"`

	// UnheardSince is when the first of the heartbeats the mothership has not
	// taken failed, and zero while the last one landed. The daemon rewrites
	// this file on every beat whether or not the beat lands, so WrittenAt
	// alone reads the same on a node nothing can reach as on one that is well.
	UnheardSince time.Time `json:"unheard_since,omitzero"`

	// Connector is the version of the connector that wrote this, so a reader
	// of the file alone — Desk — can say which one is running.
	Connector string `json:"connector,omitempty"`

	// Name is this node's 6net name as the head gave it, with no trailing
	// dot: cedar.morgan.northwind.6net.io.
	Name string `json:"name,omitempty"`

	// Legacy self-summary, retained for older readers. A home relay is not
	// evidence of a peer's path. New readers use PeerPaths.
	Relayed  bool   `json:"relayed"`
	Endpoint string `json:"endpoint,omitempty"`
	// Self-only probe results; peer endpoints live in the private check file.
	UDPProbe *UDPProbe `json:"udp_probe,omitempty"`

	// Nil means an older writer supplied no observations; an empty list means
	// this writer observed no peers. Never use tsnode.PeerPath here: its Via
	// contains endpoint addresses that must not enter this public file.
	PeerPaths []PeerPath `json:"peer_paths"`
	// Incomplete observations must not look like an exhaustive inventory.
	PathsIncomplete bool `json:"paths_incomplete,omitempty"`

	// Interface is the kernel device the overlay is on — utun4, gentility0 —
	// when the daemon runs in interface mode, and empty in userspace mode,
	// where the overlay is a netstack inside the process and on no device.
	Interface string `json:"interface,omitempty"`

	// Addresses is every overlay address tsnet says this node holds. It is the
	// record rather than the rendering — Organizations is the same set with an
	// organization against each, which is what a person reads.
	Addresses []string `json:"addresses,omitempty"`

	Organizations []OrgStatus     `json:"organizations,omitempty"`
	Services      []ServiceStatus `json:"services,omitempty"`

	// Egress is the relay this node's organization selected, and nil where it
	// selected none. A node with no egress does not fall back to anything, so
	// the absence is a fact worth keeping distinct from a zero one.
	Egress *EgressStatus `json:"egress,omitempty"`

	// AdvertisedPort is the port this node last offered its egress relay on.
	// What it actually binds is the approved service's port, in Services; this
	// is the declaration, and the two differing is worth being able to see.
	AdvertisedPort int `json:"advertised_port,omitempty"`
}

// UDPProbe is the public self-only summary. Keep this allowlist separate from
// tsnode.UDPProbe so future diagnostic fields cannot become public.
type UDPProbe struct {
	ObservedAt time.Time `json:"observed_at"`
	UDP        bool      `json:"udp"`
	IPv4       bool      `json:"ipv4"`
	IPv6       bool      `json:"ipv6"`
	GlobalV4   string    `json:"global_v4,omitempty"`
	GlobalV6   string    `json:"global_v6,omitempty"`
}

// PeerPath is the public observation of one overlay address. It deliberately
// has no peer endpoint field. Relay names a region, not a network address.
type PeerPath struct {
	Address string `json:"address"`
	Path    string `json:"path"`
	Relay   string `json:"relay,omitempty"`
}

// OrgStatus is one organization this node holds a grant in, and its address
// there.
type OrgStatus struct {
	Org     string `json:"org"`
	Address string `json:"address"`
	Label   string `json:"label,omitempty"`

	// Name is this node's 6net name in the organization, <label>.<org>.<base>
	// with no trailing dot, and empty where the daemon could not tell the base.
	Name string `json:"name,omitempty"`
}

// ServiceStatus is one listener the mothership has recorded for this node.
// State is the mothership's word and never the node's: `declared` is a request
// nobody has answered yet, and only `approved` is a permission.
type ServiceStatus struct {
	Kind  string `json:"kind"`
	Org   string `json:"org"`
	Port  int    `json:"port"`
	State string `json:"state"`
}

// EgressStatus is the relay an organization selected and where it answers.
type EgressStatus struct {
	Org  string `json:"org"`
	Via  string `json:"via"`
	Port int    `json:"port"`
}

// LoadStatus reads the last snapshot the daemon wrote. A missing file, or one
// carrying no instant, is ErrNoStatus: a snapshot whose age cannot be told is
// one nothing wrote.
func (s *Store) LoadStatus() (Status, error) {
	var status Status

	if err := s.readJSON(statusFile, &status); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Status{}, ErrNoStatus
		}
		return Status{}, err
	}
	if status.WrittenAt.IsZero() {
		return Status{}, ErrNoStatus
	}
	return status, nil
}

// SaveStatus writes the snapshot, readable by anyone who knows its path.
func (s *Store) SaveStatus(status Status) error {
	return s.writeJSON(statusFile, status, publicFilePerm)
}
