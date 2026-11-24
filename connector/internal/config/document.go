// Package config is the node's half of the config channel: it pulls the
// document Gentility renders for this node, at a revision, and hands it to
// whoever applies it.
//
// **A node serves nothing until it holds a config document**.
// That is what Ready reports and it is the reason this package has a
// loop rather than a single call: the mothership long-polls the request for up
// to sixty seconds and answers 304 when there is nothing new, 409 when it has
// not rendered for this node yet, and the document itself the moment there is
// one. None of those three is a failure — all of them mean "come back".
//
// The document is refused rather than half-read when its renderer_version is
// one this binary does not know. A config document is small and arrives every
// time, so there is no merge to get wrong; `since` is a cursor for deciding,
// never for diffing.
package config

import (
	"errors"

	"github.com/gentility-ai/gentility/connector/internal/mcp"
	"github.com/gentility-ai/gentility/connector/internal/state"
	"github.com/gentility-ai/gentility/connector/internal/tsnode"
)

// RendererVersion is the config document generation this binary can read.
// It must match the document's renderer_version field.
const RendererVersion = 1

// Document is version 1 of the node's configuration response.
//
// Origin is additive in v1 and may be absent from older documents.
// Every other field is present in every document — the renderer writes them all,
// including the two that carry null — so a missing key is not a smaller
// document but a renderer this binary does not understand, which is what
// RendererVersion is checked for before anything here is read.
type Document struct {
	Origin          string `json:"origin,omitempty"`
	RendererVersion int    `json:"renderer_version"`
	Revision        int64  `json:"revision"`
	CapverFloor     int    `json:"capver_floor"`
	NodeID          string `json:"node_id"`

	// Grants are this node's addresses, one per organization it holds a live
	// grant in. tsnode.ConfigGrant is the wire shape and tsnode.ParseGrants is
	// what turns it into something a dial can be pinned to — the /64 is what
	// picks a source address, so a grant that does not parse is a document the
	// node refuses rather than a node that dials from the wrong org.
	Grants   []tsnode.ConfigGrant `json:"grants"`
	Services []Service            `json:"services"`

	// Leasable is the gateway's directory: every approved service on the far
	// end of a live path the gateway holds as grantee. It is [] for every node
	// that is not the gateway. The gateway's lease API resolves against this
	// list and only this list, distinct from Services (this node's own
	// listeners). Entries have the same shape as Services.
	Leasable []Service `json:"leasable"`

	// Egress is nil where the organization has selected no relay. That is a
	// refusal and not a default: a node with no egress does not fall back to
	// the mothership.
	Egress  *Egress `json:"egress"`
	Gateway Gateway `json:"gateway"`
	Update  Update  `json:"update"`
}

// AdoptGrants takes on the addresses this document says the node holds. Every
// applier does this and it is the first thing each of them does: the grants are
// what a dial is pinned from, so a document whose grants do not parse is
// refused before anything else in it is acted on.
//
// It hands the grants back for the gateway, which resolves its service
// directory against the same ones — one parse, so the directory and the dial
// cannot disagree about which organizations it is in.
func (d *Document) AdoptGrants(node *tsnode.Node) ([]tsnode.Grant, error) {
	grants, err := tsnode.ParseGrants(d.Grants)
	if err != nil {
		return nil, err
	}
	node.SetGrants(grants)

	return grants, nil
}

// Adopt is AdoptGrants and the gateway: everything a document says about the
// overlay itself, taken on whole or not at all.
//
// The gateway is written down BEFORE the caller says the document was applied,
// because a gateway CA this node failed to write down is one it would come back
// from a restart without.
//
// `join`, `egress` and `serve` adopt the whole of it. `proxy` and `gateway`
// adopt only the grants, deliberately: the preflight is case-side and has no
// gateway of its own to record, and the gateway is the one every other node
// records.
func (d *Document) Adopt(node *tsnode.Node, store *state.Store) error {
	if err := state.ValidateGateway(d.Gateway.Address, d.Gateway.Port); err != nil {
		return err
	}
	// Validate grants before writing state, and origin before any effect.
	grants, err := tsnode.ParseGrants(d.Grants)
	if err != nil {
		return err
	}
	if d.Origin != "" {
		origin, err := mcp.NormalizeBaseURL(d.Origin)
		if err != nil || origin != d.Origin {
			return errors.New("config: invalid canonical login origin")
		}
		if err := store.RecordOrigin(origin); err != nil {
			return err
		}
	}
	node.SetGrants(grants)

	return store.RecordGateway(d.Gateway.Address, d.Gateway.Port, d.Gateway.CAPEM)
}

// Service is one listener this node advertises, as the mothership recorded it.
// `State` is the mothership's word, not the node's: a service is rendered
// while it is declared or approved and never once it is removed.
//
// Address is where the service listens, in its own organization — the address
// of the node HOSTING it, which for a node's own service is that node's own.
// It is what the gateway's lease API resolves a `node_service_id` to
// (`internal/gateway`.Services), and it is the one field here the renderer
// does not write yet: an empty address is a service no lease can reach, which
// is the fail-closed answer while the mothership's half catches up.
type Service struct {
	ID      string `json:"id"`
	Kind    string `json:"kind"`
	Org     string `json:"org"`
	Port    int    `json:"port"`
	State   string `json:"state"`
	Address string `json:"address"`

	// Forwardable is the server's mark on a service `connector serve
	// --forward` may carry to a local socket (internal/forward). The renderer
	// writes it only when true, and only on this node's own services, so an
	// absent mark reads false and refuses.
	Forwardable bool `json:"forwardable,omitempty"`
}

// Egress is the relay this node's organization selected, and the path it holds
// to it. PreflightAllow is display and preflight only — the relay's admit call
// is what is authoritative.
//
// LiteralAllow is different: unlike PreflightAllow it is a permission the relay
// acts on. It is the org's allowlist entries that are IP literals, and the
// relay's CheckTarget refuses a bare IP-literal CONNECT target unless it is one
// of these. The renderer emits it as egress.literal_allow; service wiring
// copies it into Relay.LiteralAllow.
type Egress struct {
	Org            string   `json:"org"`
	Via            string   `json:"via"`
	Port           int      `json:"port"`
	PreflightAllow []string `json:"preflight_allow"`
	LiteralAllow   []string `json:"literal_allow"`
}

// Gateway is where Gentility answers on the overlay. CAPEM is the internal CA
// its certificate chains to, and is empty until the server issues one.
type Gateway struct {
	Address string `json:"address"`
	Port    int    `json:"port"`
	CAPEM   string `json:"ca_pem"`
}

// Update is what this node should be running. The config channel carries it
// so a running node learns when an update becomes available.
type Update struct {
	LatestVersion string `json:"latest_version"`
	MinVersion    string `json:"min_version"`
	ManifestURL   string `json:"manifest_url"`
}
