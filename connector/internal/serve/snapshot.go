package serve

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/gentility-ai/gentility/connector/internal/config"
	"github.com/gentility-ai/gentility/connector/internal/report"
	"github.com/gentility-ai/gentility/connector/internal/state"
	"github.com/gentility-ai/gentility/connector/internal/tsnode"
	"github.com/gentility-ai/gentility/connector/internal/version"
)

// The status snapshot: what this daemon writes down so that `connector status`,
// which is a different process on the same machine, can say what it is doing.
//
// It is written in two places and for two reasons. Every applied document
// refreshes what this node HOLDS, and every heartbeat refreshes how OLD that is
// — a document arrives only when something in the organization changes, so a
// snapshot only an apply touched would read as minutes stale on a node that is
// perfectly well. `status` calls anything older than three beats stale, and the
// beat is what keeps that honest.
//
// Nothing here is ever fatal. Status is observability, and a node that stopped
// serving because it could not write the file that watches it would have traded
// the thing that matters for the thing that reports on it.

// lastDocument is the half of the snapshot a config document decides. The
// config loop writes it and the heartbeat reads it, so it is guarded.
//
// What crosses the mutex is a value whose slices are built once at set and
// never mutated after, so the copy is a copy in every sense that matters here.
type lastDocument struct {
	mu   sync.Mutex
	half state.Status
}

func newLastDocument() *lastDocument { return &lastDocument{half: nothingApplied()} }

func (l *lastDocument) set(half state.Status) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.half = half
}

func (l *lastDocument) get() state.Status {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.half
}

// nothingApplied is the document half of a node that holds no document yet.
//
// NoRevision and not zero, because revision 0 is a real document: a zero here
// would be a node claiming to hold the very first document it is still waiting
// for, which is the same mistake state.NoRevision exists to prevent.
func nothingApplied() state.Status { return state.Status{Revision: state.NoRevision} }

// fromDocument is what the document says: which organizations this node is in,
// which services the mothership has recorded for it, and which relay its
// organization selected.
func fromDocument(document *config.Document) state.Status {
	organizations := make([]state.OrgStatus, 0, len(document.Grants))
	for _, grant := range document.Grants {
		organizations = append(organizations, state.OrgStatus{
			Org: grant.Org, Address: grant.Address, Label: grant.Label,
		})
	}

	services := make([]state.ServiceStatus, 0, len(document.Services))
	for _, service := range document.Services {
		services = append(services, state.ServiceStatus{
			Kind: service.Kind, Org: service.Org, Port: service.Port, State: service.State,
		})
	}

	// Nil stays nil: an organization that selected no relay has none, and a
	// zero-valued one in the snapshot would render as a relay on port 0.
	var egress *state.EgressStatus
	if document.Egress != nil {
		egress = &state.EgressStatus{
			Org: document.Egress.Org, Via: document.Egress.Via, Port: document.Egress.Port,
		}
	}

	return state.Status{
		Revision:      document.Revision,
		Organizations: organizations,
		Services:      services,
		Egress:        egress,
	}
}

// record completes the document half with the path this node currently holds,
// and writes the whole of it down.
//
// A node that has applied no document writes nothing: the heartbeat starts
// beating around the same moment the first document lands, and a snapshot
// claiming revision 0 would be a lie a person would read as the truth.
func record(
	store *state.Store,
	half state.Status,
	seen tsnode.Report,
	offered []report.Service,
	now time.Time,
) error {
	if half.Revision == state.NoRevision {
		return nil
	}

	half.WrittenAt = now
	half.Connector = version.Connector
	half.Name = seen.Name
	half.Organizations = namedOrganizations(seen.Name, half.Organizations)
	half.Relayed = seen.Relayed
	half.Endpoint = seen.Endpoint
	half.PeerPaths = make([]state.PeerPath, 0, len(seen.PeerPaths))
	for _, peer := range seen.PeerPaths {
		half.PeerPaths = append(half.PeerPaths, state.PeerPath{
			Address: peer.Address, Path: peer.Path, Relay: peer.Relay,
		})
	}
	half.PathsIncomplete = seen.PeerPathsIncomplete
	half.UDPProbe = nil
	if p := seen.UDPProbe; p != nil {
		half.UDPProbe = &state.UDPProbe{ObservedAt: p.ObservedAt, UDP: p.UDP,
			IPv4: p.IPv4, IPv6: p.IPv6, GlobalV4: p.GlobalV4, GlobalV6: p.GlobalV6}
	}
	half.Interface = seen.Interface
	half.AdvertisedPort = advertisedPort(offered)

	half.Addresses = make([]string, 0, len(seen.Addresses))
	for _, address := range seen.Addresses {
		half.Addresses = append(half.Addresses, address.String())
	}

	// Neither file's failure suppresses the other; both failures reach the log.
	publicErr := store.SaveStatus(half)
	privateErr := store.SaveNetCheck(state.NetCheck{
		WrittenAt: now, Peers: seen.PeerDiagnostics, Incomplete: seen.PeerPathsIncomplete,
	})
	return errors.Join(publicErr, privateErr)
}

// namedOrganizations gives each organization this node's name in it,
// <label>.<org>.<base>, which is what Desk shows per organization.
//
// The netmap names the node once, in one organization, and neither it nor the
// document carries the base domain on its own. So the base is what is left of
// the netmap's name once some grant's <label>.<org>. is taken off the front,
// and every grant's name is built on it. Where no grant prefixes the name, the
// base cannot be told and every name stays empty, which is the old reading.
//
// It returns a fresh slice: the one it is given is shared with the heartbeat
// and must not be written to.
func namedOrganizations(self string, organizations []state.OrgStatus) []state.OrgStatus {
	base := ""
	for _, org := range organizations {
		if org.Label == "" {
			continue
		}
		if rest, ok := strings.CutPrefix(self, org.Label+"."+org.Org+"."); ok && rest != "" {
			base = rest
			break
		}
	}

	named := make([]state.OrgStatus, len(organizations))
	copy(named, organizations)
	if base == "" {
		return named
	}
	for i := range named {
		if named[i].Label != "" {
			named[i].Name = named[i].Label + "." + named[i].Org + "." + base
		}
	}
	return named
}

// advertisedPort is the port this node is currently offering its egress relay
// on, or 0 while it is offering nothing — which is what a node below the
// mothership's capver floor has been told to do.
func advertisedPort(offered []report.Service) int {
	for _, service := range offered {
		if service.Kind == "egress" {
			return service.Port
		}
	}
	return 0
}

// note writes the snapshot after a document has been applied, so a person who
// runs `connector status` a second later sees the document that just landed
// rather than waiting out a heartbeat.
//
// It costs one extra Status call per document, which is a call this node makes
// every thirty seconds anyway and a document it sees far less often than that.
func note(
	ctx context.Context,
	a Attendance,
	advertised *advertisement,
	documents *lastDocument,
	logf func(string, ...any),
) {
	seen, err := a.Session.Node.DiagnosticStatus(ctx)
	if err != nil {
		logf("could not read this node's path for the status snapshot: %v", err)
		return
	}
	if err := record(a.Store, documents.get(), seen, advertised.offered(), time.Now()); err != nil {
		logf("could not write the status snapshot: %v", err)
	}
}
