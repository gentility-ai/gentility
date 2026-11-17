package serve

import (
	"cmp"
	"fmt"
	"slices"
	"strings"

	"github.com/gentility-ai/gentility/connector/internal/config"
	"github.com/gentility-ai/gentility/connector/internal/version"
)

// action is what a config document asks of this node's relay.
type action int

const (
	// noop is the answer nearly every document gets: an organization's
	// document is re-rendered whenever anything in it changes, and almost
	// nothing that changes is about this node's listeners.
	noop action = iota
	startRelay
	restartRelay
	stopRelay
	// stopAdvertising is the one action that leaves the relay alone. See
	// reconcile: the paths already up keep carrying traffic; what stops is
	// this node offering anything new.
	stopAdvertising
)

func (a action) String() string {
	switch a {
	case startRelay:
		return "start"
	case restartRelay:
		return "restart"
	case stopRelay:
		return "stop"
	case stopAdvertising:
		return "stop advertising"
	default:
		return "nothing"
	}
}

// listener is one bound relay: this node's own address in the organization that
// hosts the service, and the port.
//
// All three fields, because all three are what egress.StartServices binds on —
// it looks the address up by the service's org and listens on exactly that
// address and port. A re-granted address is as much a rebind as a new port, so
// a listener that compared only ports would call one of them nothing to do.
type listener struct {
	org     string
	address string
	port    int
}

// decision is an action and the set of listeners it is about. The set is
// meaningful only for startRelay and restartRelay; the rest name none, because
// "stop" is about the relay that is running and not about listeners anybody
// chose.
type decision struct {
	do        action
	listeners []listener
}

func (d decision) String() string {
	if d.do == startRelay || d.do == restartRelay {
		return fmt.Sprintf("%s on %s", d.do, bound(d.listeners))
	}
	return d.do.String()
}

// bound renders a set of listeners for a log line or a test failure.
func bound(listeners []listener) string {
	rendered := make([]string, 0, len(listeners))
	for _, l := range listeners {
		rendered = append(rendered, fmt.Sprintf("%s [%s]:%d", l.org, l.address, l.port))
	}
	return strings.Join(rendered, ", ")
}

// serving is what this node's relay is doing at the moment a document arrives:
// the set the running generation was started with, in the canonical order
// approvedEgress puts it in, so comparing it to a fresh document is a
// straight comparison.
//
// Not a bare set, because "not serving" and "serving nothing" are different
// facts and only one of them is possible.
type serving struct {
	up        bool
	listeners []listener
}

// reconcile decides what the relay should be doing, given the document that
// just arrived and what it is doing now.
//
// **The node never decides it is approved**. Approval
// is a person's, recorded by the mothership and delivered here in the document;
// a compromised node cannot promote itself into an organization's exit by
// deciding it was approved. So everything below reads the document and the
// relay's own state, and nothing else.
//
// The comparison is over the whole set. A node holds services in as many
// organizations as it has grants and the relay generation binds every approved
// one, so a decision that watched a single service would answer "nothing to do"
// to a second organization's service being approved, withdrawn, re-ported or
// re-granted — and that organization's listener would never come up.
//
// The capver floor is checked first and answers stopAdvertising rather than
// stopRelay: a binary below the mothership's floor is one that must not take on
// anything NEW, and tearing down the paths that are already carrying traffic
// would turn "this connector is out of date" into an outage. It is the one
// answer that is about the whole node rather than about its services, which is
// why it names no listeners.
//
// This is pure on purpose: it is the whole of the policy, and the goroutines,
// listeners and shutdowns that carry it out are in serve's apply, where they
// cannot be reasoned about at a glance.
func reconcile(document *config.Document, now serving) decision {
	if document.CapverFloor > version.NodeCapver {
		return decision{do: stopAdvertising}
	}

	approved := approvedEgress(document)

	switch {
	case len(approved) == 0 && now.up:
		return decision{do: stopRelay}
	case len(approved) == 0:
		return decision{do: noop}
	case !now.up:
		return decision{do: startRelay, listeners: approved}
	case !slices.Equal(now.listeners, approved):
		return decision{do: restartRelay, listeners: approved}
	default:
		return decision{do: noop}
	}
}

// approvedEgress is every listener this node is to bind: one per approved
// egress service, on this node's address in the organization that hosts it.
//
// The whole list is read rather than its first entry: a node can hold services
// in more than one organization and in more than one state, and reading only
// the first would make what this node serves depend on the order the renderer
// happened to write. A service in any state but `approved` is not one — a
// declared service is a request nobody has answered yet.
//
// The addresses come off the document's grants exactly as egress.NewConfig
// builds them, so the set compared here is the set StartServices binds. The
// result is sorted, so the same set rendered in a different order is the same
// set and not a rebind.
func approvedEgress(document *config.Document) []listener {
	addresses := make(map[string]string, len(document.Grants))
	for _, grant := range document.Grants {
		addresses[grant.Org] = grant.Address
	}

	var listeners []listener
	for _, service := range document.Services {
		if service.Kind != "egress" || service.State != "approved" {
			continue
		}
		listeners = append(listeners, listener{
			org:     service.Org,
			address: addresses[service.Org],
			port:    service.Port,
		})
	}

	slices.SortFunc(listeners, func(a, b listener) int {
		return cmp.Or(
			cmp.Compare(a.org, b.org),
			cmp.Compare(a.address, b.address),
			cmp.Compare(a.port, b.port),
		)
	})
	return listeners
}
