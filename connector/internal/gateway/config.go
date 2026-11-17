package gateway

import (
	"fmt"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"strconv"
	"time"

	"github.com/gentility-ai/gentility/connector/internal/enroll"
	"github.com/gentility-ai/gentility/connector/internal/exitcodes"
	"github.com/gentility-ai/gentility/connector/internal/tsnode"
)

// Port is the port the gateway answers on, in every organization.
//
// It is a constant and not a variable. The World's packet filters permit every
// node → gateway on 443 and nothing else, and every node's config
// document says `gateway.port: 443`, so a gateway that listened elsewhere
// would be a gateway nothing could reach and nothing could tell it so.
const Port uint16 = 443

// Variable names, in one place, for the same reason enroll keeps its own
// there: they are a contract with the server's deployment of the gateway and
// with its development tooling.
const (
	VarUpstream      = "OVERLAY_UPSTREAM"
	VarIngressSecret = "OVERLAY_INGRESS_SECRET"
	VarLeaseAddr     = "OVERLAY_LEASE_ADDR"
)

// Config is what `connector gateway` needs from its environment.
type Config struct {
	// Join is the node half: this gateway is a platform-owned machine node and
	// joins the overlay the way any other node does.
	Join enroll.Env

	// Upstream is the app's overlay listener on the deploy's docker network —
	// `http://gentility-web:<port>`, a container alias rather than an address,
	// because kamal replaces the app container on every deploy and the alias
	// is what survives the swap.
	Upstream *url.URL

	// Secret is OVERLAY_INGRESS_SECRET: the value the app also holds, and the
	// only thing that makes the peer header worth believing. It is also what
	// authenticates this gateway's fetch of its own TLS key, so a mismatch is
	// not merely every request refused — it is a gateway that never obtains a
	// certificate and therefore never serves.
	Secret []byte

	// LeaseAddr is where the lease API binds — the app's one door onto the
	// overlay, and the only listener in this process that is NOT on the
	// overlay.
	//
	// An address and a port rather than a host and a port: this is a bind, and
	// a name that resolved to two addresses would be a bind that silently
	// picked one. The kamal accessory sets `0.0.0.0:8079` because a container
	// learns its docker address at runtime and cannot name it in an env file;
	// a worktree sets a loopback address and its own slot port. Neither is a
	// weakening — what a lease answers with is the address the app's own
	// request arrived at, never the wildcard, and the bearer is the boundary.
	LeaseAddr netip.AddrPort
}

// Read reads the gateway environment from the process.
func Read() (Config, error) { return ReadEnv(os.LookupEnv) }

// ReadEnv reads the gateway environment through lookup.
//
// Every refusal about a variable here is exitcodes.Usage: it is the operator's,
// and it will be just as wrong on the next start, so a supervisor must not
// restart into it.
//
// The join half is read through ReadJoinOrRejoin rather than ReadJoin, which is
// the difference between a gateway that survives being re-created and one that
// does not. The kamal accessory's three one-shot secrets are minted for a FIRST
// boot and dead within the hour; what brings this container back afterwards is
// the state volume, and demanding them of a gateway that already has an
// identity is a refusal with nothing behind it — and every node this one names
// off the overlay for as long as it lasts.
func ReadEnv(lookup func(string) (string, bool)) (Config, error) {
	join, err := enroll.ReadJoinOrRejoin(lookup)
	if err != nil {
		return Config{}, err
	}

	raw, err := required(lookup, VarUpstream)
	if err != nil {
		return Config{}, err
	}

	// The value is never echoed as it was written. A URL may carry userinfo,
	// and an operator who put a credential in this variable would otherwise
	// have it printed back at them on stderr and into whatever collects that —
	// which is how a secret nobody logged ends up in a log.
	//
	// url.Redacted() replaces a password with `xxxxx`, so a value that parsed
	// can still be quoted. One that did not parse is not quoted at all: there
	// is no structure to redact, and the operator has the variable name and
	// the form it wants, which is enough to find it.
	upstream, err := url.Parse(raw)
	if err != nil {
		return Config{}, usage(fmt.Errorf(
			"%s is not a URL to the app's overlay listener — it wants the form "+
				"http://gentility-web:24081",
			VarUpstream,
		))
	}
	if upstream.Scheme == "" || upstream.Host == "" {
		return Config{}, usage(fmt.Errorf(
			"%s is %q, which is not a URL to the app's overlay listener — it wants the form "+
				"http://gentility-web:24081",
			VarUpstream, upstream.Redacted(),
		))
	}
	if upstream.User != nil {
		return Config{}, usage(fmt.Errorf(
			"%s carries userinfo, and this hop has no use for it: the app authenticates the "+
				"gateway by %s and nothing else",
			VarUpstream, VarIngressSecret,
		))
	}

	secret, err := required(lookup, VarIngressSecret)
	if err != nil {
		return Config{}, err
	}

	leaseAddr, err := leaseBind(lookup)
	if err != nil {
		return Config{}, err
	}

	return Config{Join: join, Upstream: upstream, Secret: []byte(secret), LeaseAddr: leaseAddr}, nil
}

// leaseBind reads where the lease API listens, and refuses to put it on the
// overlay.
//
// The refusal is the point. The lease API's authentication is a bearer the app
// holds and nothing else, and it hands out one-connection listeners onto every
// organization this gateway has a grant in — so bound on an overlay address it
// would be a door every node could knock on, guarded by a secret that was
// never meant to travel there. It belongs on the deploy's docker network, and
// an operator who reaches for an overlay address here is told so at boot
// rather than after.
func leaseBind(lookup func(string) (string, bool)) (netip.AddrPort, error) {
	raw, err := required(lookup, VarLeaseAddr)
	if err != nil {
		return netip.AddrPort{}, err
	}

	bind, err := netip.ParseAddrPort(raw)
	if err != nil {
		return netip.AddrPort{}, usage(fmt.Errorf(
			"%s is %q, which is not an address and port for the lease API to bind on — it "+
				"wants the form 0.0.0.0:8079",
			VarLeaseAddr, raw,
		))
	}

	if _, err := tsnode.OrgPrefix(bind.Addr()); err == nil {
		return netip.AddrPort{}, usage(fmt.Errorf(
			"%s is %s, which is an overlay address. The lease API is the app's door on the "+
				"deploy's docker network and must never be reachable from the overlay",
			VarLeaseAddr, bind.Addr(),
		))
	}

	return bind, nil
}

// SelfClient is the client the gateway reaches the app's overlay listener with
// — for its own enrollment exchange, its own config pulls and its own presence
// heartbeat.
//
// The gateway is the only node that attributes itself. Every other node is
// named by a gateway; this one IS the gateway, so it signs its own address
// with the same secret and the same MAC it signs everybody else's with. That
// is not a bypass: the signed-peer verifier believes the header
// because whoever set it held OVERLAY_INGRESS_SECRET, and that is exactly what
// is true here.
//
// The hop is plain HTTP over the deploy's docker network, which is the same
// hop every forwarded request takes.
func SelfClient(secret []byte, peer netip.Addr) *http.Client {
	return &http.Client{Transport: selfSigner{
		peer:   peer.String(),
		secret: secret,
		inner:  http.DefaultTransport.(*http.Transport).Clone(),
	}}
}

// selfSigner sets the signed trio on every request, per request.
type selfSigner struct {
	peer   string
	secret []byte
	inner  http.RoundTripper
}

// RoundTrip signs a copy: a RoundTripper may not modify the request it is
// handed, and the caller may still be holding it.
//
// The timestamp is minted here rather than once per client, so a config poll
// held open for sixty seconds and the heartbeat that follows it are each
// inside their own thirty-second window.
func (s selfSigner) RoundTrip(request *http.Request) (*http.Response, error) {
	cloned := request.Clone(request.Context())
	stripPeerHeaders(cloned.Header)

	unixTs := time.Now().Unix()
	cloned.Header.Set(PeerHeader, s.peer)
	cloned.Header.Set(PeerTsHeader, strconv.FormatInt(unixTs, 10))
	cloned.Header.Set(PeerSigHeader, Sign(s.secret, s.peer, unixTs))

	return s.inner.RoundTrip(cloned)
}

func required(lookup func(string) (string, bool), name string) (string, error) {
	value, _ := lookup(name)
	if value == "" {
		return "", usage(fmt.Errorf("%s is not set, and the gateway cannot run without it", name))
	}
	return value, nil
}

func usage(err error) error { return exitcodes.Wrap(exitcodes.Usage, err) }
