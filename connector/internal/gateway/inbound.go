package gateway

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/netip"
	"net/url"
	"strconv"
	"time"
)

const (
	// readHeaderTimeout bounds how long a connection may take to say what it
	// wants. There is deliberately no write timeout anywhere in this package:
	// the listener holds a config poll open for up to sixty seconds, and a
	// write deadline is how a long poll turns into a truncated response nobody
	// can explain.
	readHeaderTimeout = 30 * time.Second

	// idleTimeout is how long a keep-alive connection may sit between
	// requests before the gateway closes it.
	//
	// Above the sixty-second config poll on purpose. This is not a write
	// deadline and must not be mistaken for one: it starts only once a
	// response has been fully written, so a poll held open for its full minute
	// is never near it, and a node that polls in a loop resets it on every
	// turn. What it does bound is the connection nobody is using — which,
	// without it, is a connection this process holds until the peer or the
	// kernel gets bored.
	idleTimeout = 120 * time.Second

	// attributeTimeout bounds one WhoIs.
	//
	// Its own deadline rather than the request's: the request may be a
	// sixty-second long poll, and attribution is a call into this process's
	// own local API that either answers at once or is not going to. Inheriting
	// the poll's deadline would mean a wedged local API held a connection for
	// a minute before refusing it.
	attributeTimeout = 5 * time.Second

	// shutdownGrace is how long a listener's in-flight requests have to finish
	// once the gateway is on its way out.
	//
	// A few seconds and not a minute: SIGTERM has a deadline of its own from
	// whatever sent it, and a node whose long poll is cut short reconnects.
	// What this buys is the request that was nearly done finishing rather than
	// arriving at the node as a truncated body.
	shutdownGrace = 5 * time.Second
)

// peerKey carries the attributed address from the handler, where a refusal is
// still possible, to the proxy's rewrite, where it is not.
type peerKey struct{}

// Inbound is the gateway's inbound door: one handler, however many listeners.
//
// One instance serves every address this gateway holds. What differs per
// connection is the local address, which arrives on the request context and is
// what names the organization — so a second handler per organization would be
// a second copy of the same thing.
type Inbound struct {
	// Now is the clock the timestamp is minted from. Exported for the tests,
	// which would otherwise have to wait out a thirty-second window to prove
	// the mint is per request.
	Now func() time.Time

	attribute Attribute
	secret    []byte
	logf      func(string, ...any)
	proxy     *httputil.ReverseProxy
}

// NewInbound builds the door onto upstream.
//
// upstream is the app's overlay listener on the deploy's docker network,
// reached by its network alias rather than by an address: kamal replaces the
// app container on every deploy, and the alias is what survives the swap.
//
// logf is where a refusal is said out loud. Nil is silence, which is what a
// test wants. It is never given a secret, a bearer or a signature to print.
func NewInbound(upstream *url.URL, attribute Attribute, secret []byte, logf func(string, ...any)) *Inbound {
	if logf == nil {
		logf = func(string, ...any) {}
	}

	inbound := &Inbound{Now: time.Now, attribute: attribute, secret: secret, logf: logf}

	inbound.proxy = &httputil.ReverseProxy{
		Rewrite: func(request *httputil.ProxyRequest) {
			request.SetURL(upstream)
			// The client's own X-Forwarded-* replaced by what actually
			// happened. Nothing downstream reads them, and that is the point:
			// a header nobody reads is one nobody should be able to plant.
			request.SetXForwarded()

			// Delete before you set, on the request that is actually
			// forwarded. Out is a clone, so the inbound request keeps whatever
			// it arrived with and only what leaves here is cleaned.
			//
			// The trailers as well as the headers. A client may announce
			// `Trailer: X-Overlay-Peer` and send the value after the body,
			// which is a second way into the same map by a door nobody looks
			// at.
			stripPeerHeaders(request.Out.Header)
			stripPeerHeaders(request.Out.Trailer)

			peer, _ := request.In.Context().Value(peerKey{}).(string)
			if peer == "" {
				// Unreachable: ServeHTTP attributes before it forwards and
				// refuses when it cannot. If it ever happens, the one thing
				// that must not follow is a request upstream carrying a valid
				// signature over an empty address — the listener would resolve
				// that to no node at all, or to whichever one it decided ""
				// meant.
				//
				// A Rewrite cannot return an error, so it refuses the way it
				// can: a context that is already done fails the round trip
				// before a byte leaves, and ErrorHandler answers 502.
				request.Out = request.Out.WithContext(refusal(request.Out.Context()))
				return
			}

			unixTs := inbound.Now().Unix()

			request.Out.Header.Set(PeerHeader, peer)
			request.Out.Header.Set(PeerTsHeader, strconv.FormatInt(unixTs, 10))
			request.Out.Header.Set(PeerSigHeader, Sign(inbound.secret, peer, unixTs))
		},

		// -1 flushes each write straight through. The config channel is a
		// sixty-second long poll and the response has to arrive when the
		// mothership sends it, not when a buffer happens to fill.
		FlushInterval: -1,

		// The app container going away is a 502 for the seconds a rollout
		// takes, and nothing more: the listener stays bound, the process stays
		// up, and the next request finds the new container behind the same
		// alias. A gateway that died with the app it proxies would take the
		// whole fleet off the network for every deploy.
		ErrorHandler: func(writer http.ResponseWriter, request *http.Request, err error) {
			inbound.logf("gateway: %s %s: upstream: %v", request.Method, request.URL.Path, err)
			writer.WriteHeader(http.StatusBadGateway)
		},

		// Its own transport rather than the process-wide default, so what this
		// gateway does to its connection pool is this gateway's business.
		Transport: http.DefaultTransport.(*http.Transport).Clone(),
	}

	return inbound
}

// refusal is a context that is already done, and it is how the Rewrite above
// stops a request it has no signature for.
func refusal(parent context.Context) context.Context {
	ctx, cancel := context.WithCancel(parent)
	cancel()

	return ctx
}

// ServeHTTP attributes the connection and forwards the request, or refuses it.
//
// Attribution comes first and nothing is forwarded without it. The failure is
// a 502 rather than a 403 because it is not the caller's refusal to earn: a
// peer this gateway cannot attribute is a gateway that cannot do its job for
// that connection, and the connection is closed rather than left open to try
// again on the same doubt.
func (in *Inbound) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	peer, err := in.peer(request)
	if err != nil {
		in.logf("gateway: refusing %s: %v", request.RemoteAddr, err)
		writer.Header().Set("Connection", "close")
		http.Error(writer, "the overlay peer could not be attributed", http.StatusBadGateway)
		return
	}

	in.proxy.ServeHTTP(writer, request.WithContext(
		context.WithValue(request.Context(), peerKey{}, peer.String()),
	))
}

// peer is everything that has to be true before a request is forwarded.
func (in *Inbound) peer(request *http.Request) (netip.Addr, error) {
	remote, err := netip.ParseAddrPort(request.RemoteAddr)
	if err != nil {
		return netip.Addr{}, fmt.Errorf("gateway: %q is not an address and port: %w", request.RemoteAddr, err)
	}

	local, err := localAddr(request)
	if err != nil {
		return netip.Addr{}, err
	}

	ctx, cancel := context.WithTimeout(request.Context(), attributeTimeout)
	defer cancel()

	peer, err := in.attribute(ctx, remote, local)
	if err != nil {
		return netip.Addr{}, err
	}
	if !peer.IsValid() {
		return netip.Addr{}, errors.New("gateway: attribution answered no address at all")
	}

	return peer, nil
}

// localAddr is the address of the listener this request reached — the half of
// the attribution that names the organization.
//
// net/http puts it on the connection's context, so it is there for every
// request an http.Server served and absent for one that reached this handler
// some other way. Absent is a refusal: a gateway that cannot tell which
// organization a connection is in cannot decide whether the peer belongs in
// it.
func localAddr(request *http.Request) (netip.Addr, error) {
	value, ok := request.Context().Value(http.LocalAddrContextKey).(net.Addr)
	if !ok {
		return netip.Addr{}, errors.New(
			"gateway: this request carries no local address, so there is no organization to attribute it in",
		)
	}

	local, err := netip.ParseAddrPort(value.String())
	if err != nil {
		return netip.Addr{}, fmt.Errorf("gateway: the listener's address %q: %w", value, err)
	}

	return local.Addr(), nil
}

// Serve runs handler over ln until ctx is done or ln is closed.
//
// A closed listener is not an error here: closing one is how the reconcile
// loop unbinds an address a grant was revoked from, and the goroutine serving
// it is meant to end quietly when it happens.
//
// **Quietly is not the same as slowly.** Closing a listener stops it accepting
// and does nothing at all to the connections it already accepted — so a node
// whose organization lost its grant would keep a keep-alive connection open
// and keep being served on it, through a revocation, for as long as it cared
// to hold the socket. The revocation is the point of the unbind, so the server
// goes with the listener and the accepted connections go with the server.
//
// The two exits differ, because they mean different things:
//
//   - the listener closed under us, which is a revocation, and the accepted
//     connections are exactly what has to stop: Close, at once;
//   - ctx is done, which is this process shutting down and nobody's access
//     being withdrawn: Shutdown, so what is in flight has shutdownGrace to
//     finish.
func Serve(ctx context.Context, ln net.Listener, handler http.Handler) error {
	server := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: readHeaderTimeout,
		IdleTimeout:       idleTimeout,
	}

	drained := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		defer close(drained)

		// WithoutCancel because ctx is already done: the grace period is the
		// whole point and a deadline derived from a cancelled parent is no
		// grace at all.
		grace, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownGrace)
		defer cancel()

		// Grace, then not. A request still running when the grace is up is one
		// that was never going to finish, and shutdownGrace is a bound or it
		// is nothing.
		if err := server.Shutdown(grace); err != nil {
			_ = server.Close()
		}
	})

	defer func() {
		if stop() {
			// The shutdown never started, so this is the listener having
			// closed. Nothing is draining and nothing should be.
			_ = server.Close()
			return
		}

		// It did start. Serve returned the moment Shutdown closed the
		// listener; waiting here is what gives the in-flight requests the
		// grace this function promised them.
		<-drained
	}()

	err := server.Serve(ln)
	if errors.Is(err, http.ErrServerClosed) || errors.Is(err, net.ErrClosed) {
		return nil
	}

	return err
}
