package egress

import (
	"context"
	"fmt"
	"net"
	"net/netip"
)

// ServiceConfig is one entry of the config document's `services` array, reduced
// to what the egress wiring acts on. State is the mothership's word, not the
// node's: only an `approved` egress service is served, because approval is a
// human's decision and a compromised node cannot promote itself into an org's
// exit.
type ServiceConfig struct {
	Kind  string
	Org   string
	Port  int
	State string
}

// Config is everything StartServices needs, in two halves.
//
// The data half — Services and Addresses — is read straight off the node's
// config document. The seam half is filled by the connector with the real
// tsnet-backed implementations (cmd/connector), and left nil by the listener
// test, which never drives a CONNECT through a started relay and so never
// reaches Admit, Peers, Dial or Lookup.
type Config struct {
	// Services is the config document's `services` array.
	Services []ServiceConfig

	// Addresses maps an organization slug to this node's own overlay address in
	// that org. A relay listens on this address — its own, in the org that hosts
	// the service — never on a wildcard.
	Addresses map[string]string

	// Admit is the authoritative admit client, already pointed at the overlay
	// gateway and carrying this node's credential. Every relay shares it.
	Admit *AdmitClient

	// Peers builds the peer attribution for a relay bound to local. It mirrors
	// internal/gateway's Attributor (fix A): it returns the address the
	// connection actually came from, having verified the head attributes it to
	// the peer, and refuses anything else — never a substitute address.
	Peers func(local netip.Addr) PeerResolver

	// Dial opens the outbound connection to an internet-routable destination.
	// The relay forwards to the public internet ONLY (a grant to the
	// relay is not a grant to the LAN), so this is an ordinary dialler and not
	// the overlay netstack — an overlay address is meaningless on the internet,
	// and CheckResolved refuses the overlay's own ULA anyway.
	Dial func(ctx context.Context, network, addr string) (net.Conn, error)

	// Lookup resolves the destination hostname once per CONNECT.
	Lookup func(ctx context.Context, host string) ([]netip.Addr, error)

	// LiteralAllow is the org's allowlist entries that are IP literals; a bare
	// IP-literal CONNECT target is refused unless it is one of these.
	LiteralAllow []string
}

// ListenerFactory binds a listener on one explicit overlay address and port.
//
// Explicit per address, never a wildcard: a wildcard tsnet listener on a
// multi-address node answers on exactly one of them, chosen by netmap order.
// tsnode.Node implements this; the test fakes it, which is the
// whole reason the tsnet-shaped bind is behind an interface.
type ListenerFactory interface {
	Listen(addr netip.Addr, port uint16) (net.Listener, error)
}

// StartServices binds and serves every approved `egress` service in cfg, each
// on its own overlay address, and returns the relays so the caller can drain
// and Shutdown them on teardown.
//
// A declared-but-unapproved service is skipped in silence: it is not yet a
// relay anyone selected, and serving it would be the node approving its own
// exit. Any other kind is skipped too — this is the egress wiring, not the
// gateway's service directory.
//
// When ctx is cancelled every relay's listener is closed and its in-flight
// tunnels are force-closed, so the accept loop returns and the close reports
// drain; the caller then WaitIdles the returned relays so a byte count is not
// lost to exit.
func StartServices(ctx context.Context, cfg Config, factory ListenerFactory) ([]*Relay, error) {
	var relays []*Relay

	for _, svc := range cfg.Services {
		if svc.Kind != "egress" || svc.State != "approved" {
			continue
		}

		raw, ok := cfg.Addresses[svc.Org]
		if !ok {
			return relays, fmt.Errorf(
				"egress: no overlay address for org %q, which hosts an approved egress service", svc.Org,
			)
		}
		addr, err := netip.ParseAddr(raw)
		if err != nil {
			return relays, fmt.Errorf("egress: address %q for org %q: %w", raw, svc.Org, err)
		}

		ln, err := factory.Listen(addr, uint16(svc.Port))
		if err != nil {
			return relays, fmt.Errorf("egress: listen on [%s]:%d: %w", addr, svc.Port, err)
		}

		relay := &Relay{
			Admit:        cfg.Admit,
			Dial:         cfg.Dial,
			Lookup:       cfg.Lookup,
			LiteralAllow: cfg.LiteralAllow,
		}
		if cfg.Peers != nil {
			relay.Peers = cfg.Peers(addr)
		}
		relays = append(relays, relay)

		go func() {
			_ = relay.Serve(ln)
		}()

		go func() {
			<-ctx.Done()
			_ = ln.Close()
			_ = relay.Shutdown(context.Background())
		}()
	}

	return relays, nil
}
