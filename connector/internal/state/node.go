package state

import (
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"

	"github.com/gentility-ai/gentility/connector/internal/tsnode"
)

// nodeFile holds what `connector join` earned: the node's identity on the
// overlay and the two secrets it presents with. It shares the directory, the
// 0711/0600 modes and the atomic write with the CLI's auth file — see the
// package doc — because it is the same class of thing and there is no second
// discipline worth having.
const nodeFile = "node.json"

// NoRevision means this node holds no config document.
//
// It is -1 and not 0 because **revision 0 is a real document**: the renderer
// numbers a node's first config from zero, so a zero-valued cursor would be a
// node claiming to be up to date with the very document it is waiting for. Any
// Node built by hand must say NoRevision; LoadNode fills it in for a file that
// does not mention the field.
const NoRevision int64 = -1

// ErrNotEnrolled reports that this state directory holds no node credential:
// the connector has never exchanged, or its state was lost. It is a state and
// not a failure — the answer is to enrol, which only the caller can decide to
// do.
var ErrNotEnrolled = errors.New("not enrolled")

// IsNotEnrolled reports whether err means "no node credential stored".
func IsNotEnrolled(err error) bool { return errors.Is(err, ErrNotEnrolled) }

// Node is everything the connector must survive a restart with.
//
// Nothing here is a secret the mothership can re-issue. The enrollment
// material it came from lives for one hour and is then
// purged, so a lost state directory past that hour means a re-enrollment —
// which is the stated, accepted answer and the reason this file is written
// 0600 in a 0711 directory and never logged.
//
// String redacts both secrets, so a Node reaches a log as an identity and a
// cursor and nothing else.
type Node struct {
	// Origin is the MCP login origin which minted this enrollment.
	Origin         string `json:"origin,omitempty"`
	NodeID         string `json:"node_id"`
	NodeCredential string `json:"node_credential"`
	// RuntimeToken is a case node's, and only a case node's: enrollment issues
	// one for an enrollment that names an agent run and nothing else.
	RuntimeToken string `json:"runtime_token,omitempty"`
	LastRevision int64  `json:"last_revision"`
	GatewayAddr  string `json:"gateway_addr,omitempty"`
	GatewayPort  int    `json:"gateway_port,omitempty"`
	// CAPEM is the internal CA the gateway's certificate chains to, as the
	// config document delivered it. Empty until the server issues one, which
	// makes the dev recipe's plaintext hop explicit rather than a fallback.
	CAPEM string `json:"ca_pem,omitempty"`
	// HeadURL and Hostname are what it takes to come back.
	//
	// The pre-auth key a node joined on is single-use and expires in ten
	// minutes, so it is long gone by the first reboot. What
	// brings a server back is the node key tsnet persisted in its own state
	// directory — but tsnet still has to be told which head to present it to
	// and which name to come back under, and neither is anywhere else on disk.
	// A node.json holding neither predates this and has to enrol again.
	//
	// Neither is a secret; String shows them both.
	HeadURL  string `json:"head_url"`
	Hostname string `json:"hostname"`
}

// String renders the node without either secret.
//
// The receiver is a value so that %v on a Node and on a *Node both reach it —
// a redaction only one of the two spellings honours is a redaction that will
// be missed.
func (n Node) String() string {
	return fmt.Sprintf(
		"node %s %q on %s (credential %s, runtime token %s, config revision %d, gateway [%s]:%d)",
		n.NodeID, n.Hostname, n.HeadURL,
		redacted(n.NodeCredential), redacted(n.RuntimeToken), n.LastRevision,
		n.GatewayAddr, n.GatewayPort,
	)
}

func redacted(secret string) string {
	if secret == "" {
		return "none"
	}
	return "REDACTED"
}

// LoadNode reads this node's identity. A missing file, or one naming no
// credential, is ErrNotEnrolled.
func (s *Store) LoadNode() (Node, error) {
	// The default before the decode, so a file that predates last_revision —
	// or one written by something that left it out — loads as "no document"
	// rather than as "revision 0 applied".
	node := Node{LastRevision: NoRevision}

	if err := s.readJSON(nodeFile, &node); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Node{}, ErrNotEnrolled
		}
		return Node{}, err
	}
	if node.NodeCredential == "" {
		return Node{}, ErrNotEnrolled
	}
	return node, nil
}

// SaveNode writes this node's identity, owner-readable only.
func (s *Store) SaveNode(node Node) error { return s.writeJSON(nodeFile, node, filePerm) }

// OriginMismatchError refuses identity changes learned through configuration.
type OriginMismatchError struct {
	NodeID     string
	Stored     string
	Advertised string
}

func (e *OriginMismatchError) Error() string {
	return fmt.Sprintf("node %s origin mismatch: stored %q, advertised %q; operator-reviewed identity recovery required", e.NodeID, e.Stored, e.Advertised)
}

// RecordOrigin fills only an absent origin, preserving the latest identity.
// Callers validate the advertised origin before calling this method.
func (s *Store) RecordOrigin(origin string) error {
	if origin == "" {
		return nil
	}
	node, err := s.LoadNode()
	if err != nil {
		return err
	}
	if node.Origin == origin {
		return nil
	}
	if node.Origin != "" {
		return &OriginMismatchError{NodeID: node.NodeID, Stored: node.Origin, Advertised: origin}
	}
	node.Origin = origin
	return s.SaveNode(node)
}

// AdvanceRevision records the config revision this node has applied.
//
// Read-modify-write on purpose: the cursor moves on every config pull while
// the two secrets move once, and a caller holding a stale Node to write back
// would be a caller able to overwrite a rotated credential with the one it
// happened to load with.
func (s *Store) AdvanceRevision(revision int64) error {
	node, err := s.LoadNode()
	if err != nil {
		return err
	}
	node.LastRevision = revision
	return s.SaveNode(node)
}

// RecordGateway writes down the gateway the config document names.
//
// The CA is the half that is read back: the next start pins it, so a node that
// has been handed the gateway's CA verifies against it rather than falling
// back to the plaintext hop its first join was allowed. The address and port
// are the record of where the mothership said it answers — where this node
// actually reaches it is the join environment's to say, because it is the
// Builder that puts a node in front of a gateway.
//
// Read-modify-write for the same reason AdvanceRevision is, and a no-op when
// nothing moved: a document arrives whenever anything in the organization
// changes and the gateway is the part of it that almost never does.
func (s *Store) RecordGateway(address string, port int, caPEM string) error {
	if err := ValidateGateway(address, port); err != nil {
		return err
	}
	node, err := s.LoadNode()
	if err != nil {
		return err
	}

	if node.GatewayAddr == address && node.GatewayPort == port && node.CAPEM == caPEM {
		return nil
	}

	node.GatewayAddr = address
	node.GatewayPort = port
	node.CAPEM = caPEM

	return s.SaveNode(node)
}

// ValidateGateway checks configuration before either persisting it or adopting
// the document's other effects. Development loopback belongs to the join
// environment, never to a gateway supplied by a config document.
func ValidateGateway(address string, port int) error {
	addr, err := netip.ParseAddr(address)
	if err != nil || !tsnode.OverlayRange().Contains(addr) {
		return fmt.Errorf("config: gateway address must be in the overlay range")
	}
	if port < 1 || port > 65535 {
		return fmt.Errorf("config: gateway port must be between 1 and 65535")
	}
	return nil
}

// ClearNode forgets this node's identity. Forgetting what is already forgotten
// is not an error.
func (s *Store) ClearNode() error {
	if err := os.Remove(filepath.Join(s.Dir, nodeFile)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("removing %s: %w", nodeFile, err)
	}
	return nil
}

// TSNetDir is the directory tsnet keeps its own state in, inside the state
// directory but never among the connector's own files: one is the library's to
// write and the other is ours, and a library that tidied up its directory would
// take the node's credential with it.
//
// The name lives here because an identity has two halves stored by two owners —
// the credential in node.json is ours, the node key in this directory is
// tsnet's — and ClearIdentity is where that fact stops being invisible. A name
// spelled twice is a name that can be spelled differently.
const TSNetDir = "tailscale"

// HoldsMachine reports whether anything here ties this directory to a node:
// the identity, tailscale's node key, or an enrollment staged to join on. It is
// what `leave` asks before deciding there is nothing to forget — the unit alone
// would not say, because a leave that removed the unit and then failed has
// left the identity behind it.
func (s *Store) HoldsMachine() bool {
	for _, name := range []string{nodeFile, TSNetDir, enrollmentFile} {
		if _, err := os.Stat(filepath.Join(s.Dir, name)); err == nil {
			return true
		}
	}
	return false
}

// ClearIdentity forgets everything that makes this state directory a
// particular node: the credential, the node key tsnet persisted, and the status
// snapshot written about the node that is going.
//
// It is `connector install --replace`'s half of a re-enrolment, and it takes
// both halves because either one left behind is a node that cannot come up.
// node.json alone is a credential the mothership has revoked; the tsnet
// directory alone is the node key a head that has forgotten this node answers
// by asking for an interactive login, which is tsnode.ErrRejoinRefused and the
// wreck the flag exists to clear.
//
// Everything else stays, the directory itself included. The bearer, the
// remembered organization and an enrollment staged a moment ago are not this
// node's identity, and an install that removed the material it was about to
// join on would be an install that undid itself.
//
// Forgetting what is already forgotten is not an error.
func (s *Store) ClearIdentity() error {
	if err := s.ClearNode(); err != nil {
		return err
	}
	if err := os.RemoveAll(filepath.Join(s.Dir, TSNetDir)); err != nil {
		return fmt.Errorf("removing %s: %w", TSNetDir, err)
	}
	if err := os.Remove(filepath.Join(s.Dir, statusFile)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("removing %s: %w", statusFile, err)
	}
	if err := os.Remove(filepath.Join(s.Dir, netCheckFile)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("removing %s: %w", netCheckFile, err)
	}
	return nil
}
