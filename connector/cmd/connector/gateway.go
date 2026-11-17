package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/signal"
	"syscall"

	"github.com/gentility-ai/gentility/connector/internal/config"
	"github.com/gentility-ai/gentility/connector/internal/enroll"
	"github.com/gentility-ai/gentility/connector/internal/exitcodes"
	"github.com/gentility-ai/gentility/connector/internal/gateway"
	"github.com/gentility-ai/gentility/connector/internal/report"
	"github.com/gentility-ai/gentility/connector/internal/state"
	"github.com/gentility-ai/gentility/connector/internal/traffic"
)

// runGateway is the mothership's own node: the same binary a customer
// installs, run as a platform-owned machine node, terminating TLS on the
// overlay and forwarding what arrives to the app over the deploy's docker
// network.
//
// It is `join` plus a door. The join half is identical — come up, exchange,
// hold the config channel open, beat presence — and the three differences are
// all consequences of being the thing every other node is named by:
//
//   - it reaches the app directly by container alias rather than over the
//     overlay through a gateway, because it is the gateway;
//   - it names ITSELF in the signed peer header, with the same secret and the
//     same MAC it names everybody else with;
//   - it serves as soon as it holds addresses and a certificate, without
//     waiting for its own config document. "A node serves nothing until it
//     holds a config document" is about a node serving its own
//     services; this door is how every other node's document reaches it, and a
//     gateway that waited for its own would be half of a deadlock.
//
// It runs two doors. Inbound is TLS on 443 on every overlay address it holds;
// outbound is the lease API on OVERLAY_LEASE_ADDR, on the docker network and
// never on the overlay. The inbound door serves without a config document; the
// outbound one answers 503 until it has one, because a lease resolves a
// service out of the document and a gateway with none has no authority to say
// whether a service exists.
func runGateway(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet(invoked+" gateway", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	verbose := flags.Bool("verbose", false, "print the overlay library's own logging")

	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return help(stdout, invoked+" gateway [--verbose]", flags)
		}
		diagnosticf(stderr, "%s gateway: %v\n", invoked, err)
		return exitcodes.Usage
	}
	if flags.NArg() > 0 {
		diagnosticf(stderr, "%s gateway: unexpected argument %q\n", invoked, flags.Arg(0))
		return exitcodes.Usage
	}

	say, complain := narrate("gateway", stdout, stderr)

	cfg, err := gateway.Read()
	if err != nil {
		return complain(err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	node, addresses, err := enroll.Join(ctx, cfg.Join, libraryLogger(*verbose, stderr))
	if err != nil {
		return complain(err)
	}
	defer node.Close()
	say("joined as %s with %d address(es)", cfg.Join.Hostname, len(addresses))

	// The address this gateway names itself by, and — since it is its own
	// gateway — the one it writes down as the gateway it reaches Gentility
	// through. Every address it holds resolves to this same node, and the
	// lowest is stable across restarts.
	self, err := enroll.SelfPeer(addresses)
	if err != nil {
		return complain(err)
	}
	cfg.Join.GatewayAddr = netip.AddrPortFrom(self, gateway.Port)

	store := &state.Store{Dir: cfg.Join.StateDir}
	client := gateway.SelfClient(cfg.Secret, self)

	stored, err := gatewayIdentity(ctx, cfg, store, client)
	if err != nil {
		return complain(err)
	}
	say("exchanged: node %s", stored.NodeID)

	certs := gateway.NewCerts(cfg.Upstream, cfg.Secret, nil, say)

	leaf, err := certs.Fetch(ctx, addresses)
	if err != nil {
		return complain(firstCertFailure(err))
	}
	say("certificate issued, good until %s", leaf.NotAfter.Format("2006-01-02"))

	go func() {
		if err := certs.Run(ctx, node.Addresses); err != nil && ctx.Err() == nil {
			say("gateway: the renewal loop stopped: %v", err)
		}
	}()

	inbound := gateway.NewInbound(cfg.Upstream, gateway.Attributor(node.WhoIs), cfg.Secret, say)

	// TLS 1.3 and nothing older. Both ends of this handshake are ours — the
	// connector on one side, a leaf our own CA issued on the other — so there
	// is no old client to keep working and no reason to offer a version with
	// renegotiation, static RSA or a cipher suite anybody argues about.
	//
	// http/1.1 and only http/1.1, because that is what the door behind this
	// speaks: `Inbound` is an httputil.ReverseProxy over http.Transport, so a
	// client that negotiated h2 would be talking a protocol nothing here
	// serves.
	go gateway.Listen(ctx, node, &tls.Config{
		GetCertificate: certs.GetCertificate,
		MinVersion:     tls.VersionTLS13,
		NextProtos:     []string{"http/1.1"},
	}, inbound, say)

	// The outbound door. It is bound before the config loop starts so that an
	// address already in use is a refusal at boot rather than a gateway that
	// looked healthy and had no way onto the overlay.
	//
	// One secret in the list today: this container holds a single
	// OVERLAY_INGRESS_SECRET, and the list is what a rotation would grow.
	services := &gateway.Services{}
	leases := gateway.NewLease([][]byte{cfg.Secret}, services, node.DialBound, say)

	leaseListener, err := net.Listen("tcp", cfg.LeaseAddr.String())
	if err != nil {
		return complain(fmt.Errorf("binding the lease API on %s: %w", cfg.LeaseAddr, err))
	}
	say("lease API on %s", leaseListener.Addr())

	go func() {
		if err := gateway.Serve(ctx, leaseListener, leases); err != nil && ctx.Err() == nil {
			say("gateway: the lease API stopped: %v", err)
		}
	}()

	configs := config.New(cfg.Upstream.String(), stored, store, client)
	configs.Log = say

	presence := &report.Reporter{
		BaseURL:    cfg.Upstream.String(),
		Credential: stored.NodeCredential,
		HTTP:       client,
		Status:     heartbeat(node, say),
		Ready:      configs.Ready,
		Log:        func(format string, args ...any) { say(format, args...) },
	}
	go presence.Run(ctx)

	// What the gateway carries for every organization it serves, per peer
	// (internal/traffic). The 6net traffic design's A4 check is this
	// sampler's CPU on the busiest node there is.
	counted := &traffic.Reporter{
		BaseURL:    cfg.Upstream.String(),
		Credential: stored.NodeCredential,
		HTTP:       client,
		Read:       node.PeerCounters,
		Ready:      configs.Ready,
		Log:        func(format string, args ...any) { say(format, args...) },
	}
	go counted.Run(ctx)

	err = configs.Run(ctx, func(document *config.Document) error {
		grants, err := document.AdoptGrants(node)
		if err != nil {
			return err
		}

		// The same grants the dialler will pin a source from, so the directory
		// and the dial cannot disagree about which organizations this gateway
		// is in.
		services.Apply(document, grants)

		say("config rev %d applied", document.Revision)
		return nil
	})

	if errors.Is(err, context.Canceled) {
		return exitcodes.OK
	}
	return complain(err)
}

// gatewayIdentity answers who this gateway is, exchanging the nonce only if it
// has to — the same rule `join` follows, and for the same reason: the material
// lives an hour, and a restarted process that exchanged again past it would be
// a 410 that kills a gateway that was working perfectly.
//
// The environment above is read on the same rule, and has to be: this function
// is far too late to save a container that was refused at `gateway.Read` for
// not having the spent variables set (enroll.ReadJoinOrRejoin). The store is
// asked the same question twice at boot, which is two small reads of one file
// and the price of the two decisions staying where they belong.
func gatewayIdentity(ctx context.Context, cfg gateway.Config, store *state.Store, client *http.Client) (state.Node, error) {
	stored, err := store.LoadNode()
	switch {
	case err == nil:
		return stored, nil
	case !state.IsNotEnrolled(err):
		return state.Node{}, err
	}

	return enroll.Exchange(ctx, client, cfg.Upstream.String(), cfg.Join, store)
}

// firstCertFailure names the one refusal at boot that is nobody's to retry.
//
// A 401 from the cert endpoint means the app and this container do not hold
// the same OVERLAY_INGRESS_SECRET, and that is not a state that improves by
// starting again: the same mismatch would refuse every peer header too, so a
// gateway that came up would forward requests the listener would then refuse.
// Everything else — the app not up yet, a 422 about the addresses — is
// retryable, and a supervisor restarting into it is the retry.
func firstCertFailure(err error) error {
	var refused *gateway.RefusedError
	if errors.As(err, &refused) && refused.StatusCode == http.StatusUnauthorized {
		return exitcodes.Wrap(exitcodes.Usage, fmt.Errorf(
			"%w — the app did not accept this container's internal bearer, which means the two "+
				"do not hold the same %s",
			err, gateway.VarIngressSecret,
		))
	}

	return err
}
