// Package forward is `connector serve --forward`: one service the server
// declared for this node, and marked forwardable, carried byte for byte from
// this node's own overlay address to a unix socket on the same machine.
//
// It exists for a host that runs loop computers. The runner listens on a unix
// socket and nowhere else; the platform's gateway leases a connection to the
// host node's `vmrunner` service; this package joins the two. It reads no
// byte it carries — no TLS, no HTTP — because the runner's signed envelope is
// the authentication, and the gateway is the only peer it lets in.
//
// What may be forwarded is decided twice, and both must say yes:
//
//   - this binary's own list, Kinds, which a flag cannot widen;
//   - the server, which declares the service for this node and marks it
//     `forwardable` in the config document. No heartbeat can declare it.
//
// So a connector is not a way to reach any local socket by naming a kind.
//
// The rule, per document: the forward is bound only while the current
// document carries exactly one approved, marked entry of the kind, and the
// first document must carry the entry or the process refuses to start. A
// later document without it unbinds and closes every open connection, and the
// connector runs on; the entry coming back binds again. A refusal is exit 2,
// which the unit does not restart, so it is kept for the one moment a flag on
// the wrong host should be loud: the first document.
package forward

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/netip"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gentility-ai/gentility/connector/internal/config"
	"github.com/gentility-ai/gentility/connector/internal/exitcodes"
	"github.com/gentility-ai/gentility/connector/internal/gateway"
)

// Kinds is every service kind this binary will forward. It is a positive
// list: a kind the server marks but that is not here is still refused.
var Kinds = []string{"vmrunner"}

// DialTimeout bounds the dial to the socket. A runner that is down answers at
// once; one that does not answer within this is treated as down too.
const DialTimeout = 5 * time.Second

// Spec is the flag's value: the kind to forward and the socket to carry it to.
type Spec struct {
	Kind   string
	Socket string
}

func (s Spec) String() string { return s.Kind + "=" + s.Socket }

// Parse reads `<kind>=<absolute unix socket path>`. Every refusal is a usage
// error, made before the node joins anything.
func Parse(value string) (Spec, error) {
	kind, socket, ok := strings.Cut(value, "=")
	if !ok || kind == "" || !filepath.IsAbs(socket) || filepath.Clean(socket) != socket {
		return Spec{}, usage("--forward wants <kind>=<socket>, with an absolute socket path, not %q", value)
	}
	if !slices.Contains(Kinds, kind) {
		return Spec{}, usage("--forward forwards only %s, not %q", strings.Join(Kinds, ", "), kind)
	}
	return Spec{Kind: kind, Socket: socket}, nil
}

// Forwarder carries one Spec. Apply it every config document; it binds,
// rebinds and unbinds as the documents say.
type Forwarder struct {
	Spec Spec

	// Listen binds one explicit listener on this node's overlay address.
	// tsnode.Node.Listen, outside the tests.
	Listen func(addr netip.Addr, port uint16) (net.Listener, error)

	// Attribute is who a connection came from: gateway.Attributor over
	// tsnode.Node.WhoIs, outside the tests.
	Attribute gateway.Attribute

	// Dial reaches the socket. A unix dial bounded by DialTimeout when nil.
	Dial func(ctx context.Context, socket string) (net.Conn, error)

	// Log is where the forwarder says what it is doing. Nil is silence.
	Log func(format string, args ...any)

	// mu orders Apply and Stop. The gateway has its own atomic, because a
	// connection reads it while Apply, holding mu, waits for connections.
	mu      sync.Mutex
	applied bool
	current *binding
	gateway atomic.Pointer[netip.Addr]
}

// target is where the forward listens: this node's own address in the
// service's organization, and the service's port.
type target struct {
	address netip.Addr
	port    uint16
}

// Apply takes one config document. It answers an error only for the first
// document, and only when that document does not carry exactly one marked
// entry of the kind: that error is fatal, exit 2.
func (f *Forwarder) Apply(ctx context.Context, document *config.Document) error {
	marked := f.marked(document)

	f.mu.Lock()
	defer f.mu.Unlock()

	first := !f.applied
	if first && len(marked) != 1 {
		return f.refusal(len(marked))
	}
	f.applied = true

	// Each connection reads this when it arrives, so a gateway that moved is
	// the only one let in from the next connection on.
	gateway, _ := netip.ParseAddr(document.Gateway.Address)
	f.gateway.Store(&gateway)

	want, reason := f.target(document, marked)
	if f.current != nil && f.current.target == want {
		return nil
	}
	if f.current != nil {
		f.current.stop()
		f.current = nil
		if want != (target{}) {
			reason = fmt.Sprintf("to bind [%s]:%d instead", want.address, want.port)
		}
		f.logf("forward %s: unbound, %s", f.Spec, reason)
	}
	if want == (target{}) {
		if first {
			f.logf("forward %s: waiting, %s", f.Spec, reason)
		}
		return nil
	}

	ln, err := f.Listen(want.address, want.port)
	if err != nil {
		// Not fatal: the next poll tries again (serve's OnUnchanged), and
		// until then nothing is bound, which is the closed answer.
		f.logf("forward %s: could not bind [%s]:%d: %v", f.Spec, want.address, want.port, err)
		return nil
	}
	f.current = f.serve(ctx, want, ln)
	f.logf("forward %s: serving [%s]:%d", f.Spec, want.address, want.port)
	return nil
}

// Stop unbinds and closes every open connection.
func (f *Forwarder) Stop() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.current != nil {
		f.current.stop()
		f.current = nil
	}
}

// marked is every entry of this node's own services that is of the kind and
// marked forwardable. Leasable is never read: it is other nodes' services.
func (f *Forwarder) marked(document *config.Document) []config.Service {
	var marked []config.Service
	for _, service := range document.Services {
		if service.Kind == f.Spec.Kind && service.Forwardable {
			marked = append(marked, service)
		}
	}
	return marked
}

func (f *Forwarder) refusal(count int) error {
	if count == 0 {
		return usage("the server declares no forwardable %s service on this node, so --forward %s is "+
			"refused and nothing is bound", f.Spec.Kind, f.Spec)
	}
	return usage("the server marks %d forwardable %s services on this node, so --forward %s is "+
		"refused and none is picked", count, f.Spec.Kind, f.Spec)
}

// target is where the document says to listen, or the zero target and why
// nothing is to be bound.
func (f *Forwarder) target(document *config.Document, marked []config.Service) (target, string) {
	switch {
	case len(marked) == 0:
		return target{}, "the document marks no " + f.Spec.Kind + " service forwardable"
	case len(marked) > 1:
		return target{}, fmt.Sprintf("the document marks %d %s services forwardable", len(marked), f.Spec.Kind)
	}

	service := marked[0]
	if service.State != "approved" {
		return target{}, fmt.Sprintf("the %s service is %s, not approved", f.Spec.Kind, service.State)
	}
	if service.Port < 1 || service.Port > 65535 {
		return target{}, fmt.Sprintf("the %s service's port %d is not a TCP port", f.Spec.Kind, service.Port)
	}

	for _, grant := range document.Grants {
		if grant.Org != service.Org {
			continue
		}
		if address, err := netip.ParseAddr(grant.Address); err == nil {
			return target{address: address, port: uint16(service.Port)}, ""
		}
	}
	return target{}, "this node holds no address in the service's organization " + service.Org
}

func (f *Forwarder) logf(format string, args ...any) {
	if f.Log != nil {
		f.Log(format, args...)
	}
}

func (f *Forwarder) currentGateway() netip.Addr {
	if gateway := f.gateway.Load(); gateway != nil {
		return *gateway
	}
	return netip.Addr{}
}

func (f *Forwarder) dial(ctx context.Context) (net.Conn, error) {
	if f.Dial != nil {
		return f.Dial(ctx, f.Spec.Socket)
	}
	return dialUnix(ctx, f.Spec.Socket)
}

// dialUnix is the real dial: the socket, bounded by DialTimeout.
func dialUnix(ctx context.Context, socket string) (net.Conn, error) {
	return (&net.Dialer{Timeout: DialTimeout}).DialContext(ctx, "unix", socket)
}

// binding is one bound listener and every connection open through it.
type binding struct {
	target target
	cancel context.CancelFunc
	ln     net.Listener

	mu     sync.Mutex
	closed bool
	conns  map[net.Conn]struct{}
	wg     sync.WaitGroup
}

func (f *Forwarder) serve(ctx context.Context, t target, ln net.Listener) *binding {
	ctx, cancel := context.WithCancel(ctx)
	b := &binding{target: t, cancel: cancel, ln: ln, conns: map[net.Conn]struct{}{}}

	b.wg.Add(1)
	go func() {
		defer b.wg.Done()
		<-ctx.Done()
		b.closeAll()
	}()

	b.wg.Add(1)
	go func() {
		defer b.wg.Done()
		for {
			peer, err := ln.Accept()
			if err != nil {
				return
			}
			if !b.track(peer) {
				_ = peer.Close()
				return
			}
			b.wg.Add(1)
			go func() {
				defer b.wg.Done()
				f.carry(ctx, b, peer)
			}()
		}
	}()
	return b
}

// carry lets the gateway in, and nobody else, and then copies. Both checks
// pass before the socket is dialled and before a byte is read from the peer.
func (f *Forwarder) carry(ctx context.Context, b *binding, peer net.Conn) {
	defer b.untrack(peer)
	defer peer.Close()

	if err := f.admit(ctx, peer); err != nil {
		f.logf("forward %s: closed %s: %v", f.Spec, peer.RemoteAddr(), err)
		return
	}

	local, err := f.dial(ctx)
	if err != nil {
		// No retry: the gateway's caller asks again, and a runner that is
		// down should be seen to be down.
		f.logf("forward %s: closed %s: the socket did not answer: %v", f.Spec, peer.RemoteAddr(), err)
		return
	}
	if !b.track(local) {
		_ = local.Close()
		return
	}
	defer b.untrack(local)
	defer local.Close()

	// Either side ending ends both, as the egress relay's tunnel does.
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(local, peer); done <- struct{}{} }()
	go func() { _, _ = io.Copy(peer, local); done <- struct{}{} }()
	<-done
	_ = local.Close()
	_ = peer.Close()
	<-done
}

// admit is the second check after the reach rules: the head attributes the
// connection to the address it came from, and that address is the platform
// gateway's, as the current document names it.
func (f *Forwarder) admit(ctx context.Context, peer net.Conn) error {
	remote, err := netip.ParseAddrPort(peer.RemoteAddr().String())
	if err != nil {
		return fmt.Errorf("the peer %q is not an overlay address and port", peer.RemoteAddr())
	}
	local, err := netip.ParseAddrPort(peer.LocalAddr().String())
	if err != nil {
		return fmt.Errorf("the listener %q is not an overlay address and port", peer.LocalAddr())
	}

	attributed, err := f.Attribute(ctx, remote, local.Addr())
	if err != nil {
		return err
	}
	gateway := f.currentGateway()
	if !gateway.IsValid() || attributed != gateway {
		return fmt.Errorf("%s is not the gateway (%s)", attributed, gateway)
	}
	return nil
}

func (b *binding) track(conn net.Conn) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return false
	}
	b.conns[conn] = struct{}{}
	return true
}

func (b *binding) untrack(conn net.Conn) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.conns, conn)
}

func (b *binding) closeAll() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.closed = true
	_ = b.ln.Close()
	for conn := range b.conns {
		_ = conn.Close()
	}
}

// stop closes the listener and every connection, and waits for them.
func (b *binding) stop() {
	b.cancel()
	b.wg.Wait()
}

func usage(format string, args ...any) error {
	return exitcodes.Wrap(exitcodes.Usage, fmt.Errorf(format, args...))
}
