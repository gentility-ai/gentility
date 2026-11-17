package tsnode

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"strings"

	"tailscale.com/ipn"
	"tailscale.com/ipn/ipnstate"
	"tailscale.com/tsnet"
)

// ErrRejoinRefused is what a rejoin gets when the head no longer holds the
// identity this node came back on.
//
// It is a sentinel because the answer is neither "try again" nor "look at the
// log": the state directory is worthless and only a person can replace it. The
// message is the whole of what an operator gets — the process exits and the
// unit stops — so it says the remedy and not just the fault.
var ErrRejoinRefused = errors.New(
	"the head no longer holds this node's identity, so it answered the rejoin by asking for " +
		"an interactive login — and this node only ever registers with a pre-auth key, so " +
		"nobody is ever going to visit that URL. The state directory is stale. Re-enrol: mint a " +
		"new enrollment for this machine with `net_machine_enroll` on the organization's MCP " +
		"(or `admin_net_machine_enroll` as staff), export the OVERLAY_* variables it returns, " +
		"and run `sudo -E gentility-connector install --replace`",
)

// ErrNoIdentity is what Up says when a node has nothing to present: no auth
// key and no state a previous join persisted. A phone reads it as "pair this
// phone again" (phonenode.Rejoin), which is why it is a sentinel and not only
// a sentence. No packet has been sent when it is returned.
var ErrNoIdentity = errors.New("there is no identity to present")

// Options are what it takes to bring one node up on an org overlay.
//
// They are tsnet's own fields, named here so that nothing outside this package
// has to import tsnet to say them.
type Options struct {
	// ControlURL is the head. AuthKey is the single-use pre-auth key
	// Gentility minted for this enrollment; it expires in ten minutes.
	//
	// AuthKey is empty on a REJOIN, and that is not a mistake: a node that has
	// been up for a week has nothing left to present, and needs nothing —
	// tsnet re-presents the node key it persisted in Dir. See Up.
	ControlURL string
	AuthKey    string

	// Hostname is what the head records for this node. Gentility owns the
	// node's DNS name and composes it from the grant's label,
	// so this is an identity for an operator to read, not a name to resolve.
	Hostname string

	// Dir is where tsnet keeps its own state — the node key, the netmap. It
	// is not the connector's state directory and must not be shared with it:
	// one is ours to write and the other is the library's.
	Dir string

	// Ephemeral is true for a case node: the head forgets it when
	// it goes, so a finished case leaves nothing behind to revoke.
	Ephemeral bool

	// Interface puts the overlay on a real network interface: a TUN, the OS
	// router and the OS DNS configurator, so every program on the machine
	// reaches the overlay and resolves its names. It needs root, and Up
	// refuses without it (ErrNeedsRoot). False is userspace mode — tsnet over
	// a netstack, reachable only from inside this process. See upInterface.
	Interface bool

	// Logf is where tsnet's own chatter goes. Nil is silence, which is what a
	// connector wants unless someone asked for the noise.
	Logf func(format string, args ...any)
}

// stateFile is what tsnet writes the node key into, under Options.Dir.
//
// tsnet.Server.Start builds it as filepath.Join(rootPath, "tailscaled.state")
// when no Store was given (tsnet/tsnet.go:917), and rootPath is Dir
// (tsnet/tsnet.go:800). Naming the library's file here is a coupling, but the
// alternative — a node that cannot tell a first join from a rejoin — is worse.
const stateFile = "tailscaled.state"

// hasState reports whether dir already holds the identity a previous join
// earned: the node key tsnet can re-present, which is what makes an empty auth
// key sound.
//
// An empty file is no state. tsnet creates it before it has anything to put in
// it, so a first join that died halfway leaves a file and no identity, and a
// node reading that as "already enrolled" would come up asking for a login
// nobody is going to give it.
func hasState(dir string) bool {
	if dir == "" {
		return false
	}
	info, err := os.Stat(filepath.Join(dir, stateFile))
	return err == nil && info.Mode().IsRegular() && info.Size() > 0
}

// Up brings a node up on the overlay and waits for its netmap.
//
// The tsnet.Server it builds belongs to the returned Node, so Close shuts down
// the whole thing — unlike a Node built by New around a server the caller
// already owns.
//
// An auth key is a FIRST JOIN's requirement and not a node's. tsnet ignores it
// once its state directory holds a node key ("If the node is already enrolled
// (state found in Server.Store), the auth key is ignored" — tsnet/tsnet.go:74),
// and the pre-auth key a server joined on expired ten minutes later, so a
// server that survives a reboot has to come back on Dir alone. What Up refuses
// is the one case that cannot work: nothing to present and nothing persisted.
//
// A rejoin the head REFUSES is the second thing Up refuses, and it has to do
// that itself: tsnet reads a backend asking for a login as a state to keep
// waiting in ("TODO: in the future, return an error on ipn.NeedsLogin" —
// tsnet/tsnet.go:589), so a node whose key the head has forgotten blocks in
// there forever, printing an auth URL every five seconds. See watchForLogin.
//
// opts.Interface takes the other way up, upInterface; everything above holds
// for it too.
func Up(ctx context.Context, opts Options) (*Node, error) {
	quietLibrary()
	if opts.Interface {
		return upInterface(ctx, opts)
	}
	if err := checkJoin(opts); err != nil {
		return nil, err
	}

	srv := &tsnet.Server{
		ControlURL: opts.ControlURL,
		AuthKey:    opts.AuthKey,
		Hostname:   opts.Hostname,
		Dir:        opts.Dir,
		Ephemeral:  opts.Ephemeral,
		Logf:       opts.Logf,
		UserLogf:   opts.Logf,
	}

	// The watcher's context, and the one Up itself waits on, so that seeing a
	// login request is enough to end the wait.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Armed on a REJOIN only. On a first join the backend passes through
	// NeedsLogin briefly while the auth key is presented (tsnet says so itself
	// at tsnet/tsnet.go:592), so a watcher armed there would refuse every good
	// join. A rejoin presents no key and has no such moment: a backend asking
	// for a login is the head saying it does not know this node.
	var refused <-chan struct{}
	if opts.AuthKey == "" {
		watched, err := watchForLogin(ctx, srv, cancel)
		if err != nil {
			srv.Close()
			return nil, fmt.Errorf("tsnode: joining %s: %w", opts.ControlURL, err)
		}
		refused = watched
	}

	// Up blocks until the node is Running and the netmap has arrived, which is
	// exactly the point at which its addresses are known.
	if _, err := srv.Up(ctx); err != nil {
		srv.Close()

		// The channel is closed before the context is cancelled, so an Up that
		// ended because of the watcher always finds it closed here.
		select {
		case <-refused:
			return nil, fmt.Errorf(
				"tsnode: rejoining %s from %q: %w", opts.ControlURL, opts.Dir, ErrRejoinRefused,
			)
		default:
		}

		return nil, fmt.Errorf("tsnode: joining %s: %w", opts.ControlURL, err)
	}

	if netDebugWanted(os.LookupEnv) {
		if conn, ok := srv.Sys().MagicSock.GetOK(); ok {
			conn.SetDebugLoggingEnabled(true)
		} else if opts.Logf != nil {
			opts.Logf("tsnode: magicsock debug logging: no magicsock yet")
		}
	}

	node, err := New(srv)
	if err != nil {
		srv.Close()
		return nil, err
	}
	node.close = func() { srv.Close() }

	return node, nil
}

// checkJoin refuses the two joins that cannot work in either mode: no head to
// join, and nothing to present — no auth key and no state a previous join
// persisted.
func checkJoin(opts Options) error {
	if opts.ControlURL == "" {
		return fmt.Errorf("tsnode: a node needs a control URL to come up")
	}
	if opts.AuthKey == "" && !hasState(opts.Dir) {
		return fmt.Errorf(
			"tsnode: a first join needs an auth key, and a rejoin needs the state directory "+
				"the last one was persisted in — %q holds none: %w",
			opts.Dir, ErrNoIdentity,
		)
	}
	return nil
}

// loginBus is the half of the IPN notification bus this package reads. It is
// an interface so the decision below can be proved against a stream of
// notifications rather than against a head, a netstack and ninety seconds —
// the same seam Node keeps for listen and whois.
type loginBus interface {
	Next() (ipn.Notify, error)
}

// watchForLogin watches the IPN bus beside srv.Up and closes the returned
// channel if the backend asks for an interactive login. It calls stop when it
// does, which is what ends the Up that would otherwise wait for a person.
//
// The watch is attached with NotifyInitialState because the state it is looking
// for can already have been reached: Start runs StartLoginInteractive itself
// when the backend comes up in NeedsLogin (tsnet/tsnet.go:959), and that is
// before any watcher of ours could exist. NotifyInitialState makes the current
// state the first notification, so the race has no side to lose on.
func watchForLogin(ctx context.Context, srv *tsnet.Server, stop context.CancelFunc) (<-chan struct{}, error) {
	// LocalClient starts the server, which is what puts a bus there to watch.
	// srv.Up does the same thing a moment later and finds it already started.
	client, err := srv.LocalClient()
	if err != nil {
		return nil, fmt.Errorf("local client: %w", err)
	}

	watcher, err := client.WatchIPNBus(ctx, ipn.NotifyInitialState)
	if err != nil {
		return nil, fmt.Errorf("watching this node's own state: %w", err)
	}

	refused := make(chan struct{})
	go func() {
		defer watcher.Close()
		if awaitLogin(watcher) {
			close(refused)
			stop()
		}
	}()

	return refused, nil
}

// awaitLogin reads bus until the backend asks for a login, and reports whether
// that is what ended the read.
//
// An error ends it too and is not a refusal: it is the context being cancelled
// — by the caller stopping the node, or by Up having finished — and the node's
// fate is then whatever Up returned.
func awaitLogin(bus loginBus) bool {
	for {
		notify, err := bus.Next()
		if err != nil {
			return false
		}
		if needsLogin(notify) {
			return true
		}
	}
}

// needsLogin reports whether notify is the backend waiting on a person.
//
// Two tells, because the bus can deliver either first. NeedsLogin is the state
// the backend settles in when the netmap is nil and the auth cannot continue
// without a URL visit (ipnlocal/local.go:6611); BrowseToURL is the URL itself,
// and NotifyInitialState delivers both in its first message.
func needsLogin(notify ipn.Notify) bool {
	if notify.State != nil && *notify.State == ipn.NeedsLogin {
		return true
	}
	return notify.BrowseToURL != nil && *notify.BrowseToURL != ""
}

// AdoptAddresses records this node's netmap addresses as its grants, and
// returns them.
//
// It is what a node does between joining and holding a config document. The
// document is where grants come from (SetGrants, ParseGrants) and the exchange
// is what earns one — but the exchange itself has to be dialled, and DialBound
// refuses a source no grant names. Until the document arrives the netmap IS
// the grant list: every address on it was put there by a node_grant the head
// was told about.
//
// Org is left empty because only the config document names organizations, and
// nothing matches on it — SourceFor picks a source by the destination's /64,
// which is the only thing an org is on the wire.
func (n *Node) AdoptAddresses(ctx context.Context) ([]netip.Addr, error) {
	addresses, err := n.Addresses(ctx)
	if err != nil {
		return nil, err
	}
	if len(addresses) == 0 {
		return nil, fmt.Errorf(
			"tsnode: the netmap gave this node no overlay address, so it holds no grant " +
				"anywhere and has nothing to dial from",
		)
	}

	grants := make([]Grant, 0, len(addresses))
	for _, address := range addresses {
		grants = append(grants, Grant{Address: address})
	}
	n.SetGrants(grants)

	return addresses, nil
}

// Report is what the node can say about its own path to the overlay: the
// version and capver are the caller's to add, because they are the binary's
// and not the netmap's.
type Report struct {
	UDPProbe        *UDPProbe
	PeerDiagnostics []PeerDiagnostic
	PeerPaths       []PeerPath
	LocalAddresses  []LocalAddress
	// ObservationsErr says why PeerPaths or LocalAddresses may be incomplete.
	// It is the caller's to log, never a reason to skip a heartbeat: presence
	// is what makes the node visible, and these are only context.
	ObservationsErr     error
	PeerPathsIncomplete bool
	HomeDERP            string
	// Name is this node's 6net name as the head gave it, with no trailing dot:
	// cedar.morgan.northwind.6net.io. The head takes Gentility's name verbatim, so
	// it is the name the overlay's resolver answers for. Empty until the
	// netmap names the node.
	Name string
	// Endpoint and Relayed are the legacy self-summary, retained for older
	// consumers. Self.Relay is a home DERP region and is not proof that any
	// particular peer is using a relay. PeerPaths is the per-peer observation.
	Endpoint  string
	Relayed   bool
	Addresses []netip.Addr
	// Interface is the kernel device the overlay is on, or "" in userspace
	// mode. See Node.Interface.
	Interface string
}

// Status reports what this node currently knows about its own connectivity.
func (n *Node) Status(ctx context.Context) (Report, error) {
	status, err := n.selfStatus(ctx)
	if err != nil {
		return Report{}, err
	}

	r := report(status)
	if n.probe != nil {
		r.UDPProbe = udpProbe(n.probe(ctx))
	}
	locals, err := localAddresses(n.iface)
	r.LocalAddresses, r.ObservationsErr = locals, errors.Join(r.ObservationsErr, err)
	r.Interface = n.iface
	return r, nil
}

// DiagnosticStatus includes private peer endpoints for the daemon's local snapshot.
// Ordinary heartbeat and enrollment readers use Status without loading the peer map.
func (n *Node) DiagnosticStatus(ctx context.Context) (Report, error) {
	r, err := n.Status(ctx)
	if err != nil {
		return Report{}, err
	}
	r.PeerDiagnostics = peerDiagnostics(nil, r.PeerPaths)
	if n.peerMap != nil {
		peers, err := n.peerMap(ctx)
		if err == nil && peers == nil {
			err = errors.New("peer network map unavailable")
		}
		if err != nil {
			r.ObservationsErr = errors.Join(r.ObservationsErr, fmt.Errorf("peer candidates: %w", err))
		} else {
			r.PeerDiagnostics = peerDiagnostics(peers, r.PeerPaths)
		}
	}
	return r, nil
}

// report preserves the legacy self-summary and reports each peer separately.
// The netmap's DNS name is fully qualified, so it ends in a dot no person types.
func report(status *ipnstate.Status) Report {
	self := status.Self
	paths, err := peerPaths(status)
	r := Report{
		PeerPaths:           paths,
		ObservationsErr:     err,
		PeerPathsIncomplete: err != nil,
		HomeDERP:            self.Relay,
		Name:                strings.TrimSuffix(self.DNSName, "."),
		Addresses:           overlayAddresses(self.TailscaleIPs),
	}

	switch {
	case self.CurAddr != "":
		r.Endpoint = self.CurAddr
	case self.Relay != "":
		r.Endpoint, r.Relayed = self.Relay, true
	}
	return r
}
