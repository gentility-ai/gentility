package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/gentility-ai/gentility/connector/internal/auth"
	"github.com/gentility-ai/gentility/connector/internal/exitcodes"
	"github.com/gentility-ai/gentility/connector/internal/mcp"
	"github.com/gentility-ai/gentility/connector/internal/state"
	"github.com/gentility-ai/gentility/connector/internal/supervise"
)

// placedBinary is where `net join` has the install copy this binary, owned by
// root: the root daemon runs a file only root can replace, and the person
// finds `gentility` on their PATH afterwards.
const placedBinary = "/usr/local/bin/gentility"

// The seams `net` reaches this machine through, held in variables as leaveWith
// is, so no test runs sudo, installs a unit or waits a minute.
var (
	// elevate runs an argv with the terminal attached, so a sudo password
	// prompt works. It is exec'd directly, never through a shell, so nothing
	// in it needs quoting.
	//
	// It takes no context on purpose: while it runs, this process ignores
	// Ctrl-C and waits. The terminal sends the signal to the whole foreground
	// process group, so sudo and the install get it themselves and decide on
	// their own how to stop; this process killing sudo instead would cut an
	// install off mid-step, and as a person it may not signal a root process
	// anyway. Whatever they do, this process waits for them, then cleans up.
	elevate = func(argv []string, stdin io.Reader, stdout, stderr io.Writer) error {
		cmd := exec.Command(argv[0], argv[1:]...)
		cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, stderr
		return cmd.Run()
	}
	lookSudo        = func() (string, error) { return exec.LookPath("sudo") }
	sudoUser        = func() string { return os.Getenv("SUDO_USER") }
	unitInstalled   = func() bool { return supervise.UnitInstalled(runtime.GOOS, "/", supervise.DefaultLabel) }
	hostname        = defaultHostname
	fetchEnrollment = auth.FetchEnrollment
	checkPlacement  = supervise.CheckPlacement

	// daemonStatus is the snapshot the installed daemon last wrote. It is
	// state.Status rather than status.Load's Snapshot because the wait needs
	// the instant it was written, exactly: Snapshot carries only an age,
	// truncated to the second, which cannot tell a snapshot from a second
	// before the join from one a second after it.
	daemonStatus = func() (state.Status, error) {
		return (&state.Store{Dir: supervise.DefaultStateDir}).LoadStatus()
	}

	// waitFor is how long `net join` waits for the daemon to say it is on
	// 6net, and pollEvery how often it looks.
	waitFor   = 60 * time.Second
	pollEvery = 500 * time.Millisecond
)

// netCommand dispatches the `net` verbs: a person's machine on 6net, and what
// it serves there.
func netCommand(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		netUsage(stderr)
		return exitcodes.Usage
	}

	switch args[0] {
	case "help", "-h", "--help":
		netUsage(stdout)
		return exitcodes.OK
	case "join":
		return netJoin(args[1:], stdout, stderr)
	case "leave":
		return netLeave(args[1:], stdout, stderr)
	case "status":
		return runStatus("net status", args[1:], stdout, stderr)
	case "check":
		return netCheck(args[1:], stdout, stderr)
	case "serve":
		return netServe(args[1:], stdout, stderr)
	case "unserve":
		return netUnserve(args[1:], stdout, stderr)
	default:
		diagnosticf(stderr, "%s net: unknown subcommand %q\n", invoked, args[0])
		netUsage(stderr)
		return exitcodes.Usage
	}
}

func netUsage(w io.Writer) {
	fmt.Fprintf(w, `usage: %[1]s net <command> [arguments]

commands:
  join     put this machine on 6net as you, asking sudo once [--label <label>] [--url <base>]
  leave    take this machine off 6net and forget its identity
  status   say whether this machine is on 6net, and as what [--json]
  check    read the daemon's UDP probe observations [--endpoints] [--json]
  serve    open this machine's ssh to your other machines: %[1]s net serve ssh [--port 22]
  unserve  close it again: %[1]s net unserve ssh [--port 22]
`, invoked)
}

// netJoin puts this machine on 6net as the person signed in: Desk's Join
// button, with sudo where Desk has osascript.
//
// The join material is fetched here, at the moment of joining, rather than at
// login, for two reasons. The pre-auth key in it dies ten minutes after it is
// minted, so material taken at login is dead by the time anyone installs on
// it; and a person who is already signed in has no login to take it from.
//
// It runs as the person, and refuses to run under sudo, because the bearer it
// fetches with is the person's: the process holding the bearer is the process
// whose bearer it is. Root is handed only the material, in a directory of the
// person's that only they can read, and never reads auth.json. As a root login
// (euid 0 and no $SUDO_USER) the bearer is root's own, so the same steps run
// without the sudo.
//
// Every refusal comes before the fetch, because each fetch mints a single-use
// key.
func netJoin(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet(invoked+" net join", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	labelFlag := flags.String("label", "", "the name this machine joins under (default: the first label of its hostname)")
	host := flags.String("url", "", "the Gentility MCP host to fetch the join material from")

	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return help(stdout, invoked+" net join [--label <label>] [--url <base>]", flags)
		}
		diagnosticf(stderr, "%s net join: %v\n", invoked, err)
		return exitcodes.Usage
	}
	if flags.NArg() > 0 {
		diagnosticf(stderr, "%s net join: unexpected argument %q\n", invoked, flags.Arg(0))
		return exitcodes.Usage
	}
	refuseJoin := func(format string, a ...any) int {
		diagnosticf(stderr, "%s net join: "+format+"\n", append([]any{invoked}, a...)...)
		return exitcodes.Usage
	}
	warningBinary := ""
	defer func() {
		if warningBinary == "" {
			warnCurrentPath(stderr)
		} else {
			warnPathShadow(stderr, warningBinary, placedBinary)
		}
	}()

	root := geteuid() == 0
	if root && sudoUser() != "" {
		return refuseJoin("run it as yourself, without sudo: `%s net join` — it signs in as you and asks for sudo itself", invoked)
	}
	if unitInstalled() {
		return refuseJoin("this machine is on 6net already; `%s net leave` first", invoked)
	}

	label, given := strings.TrimSpace(*labelFlag), false
	flags.Visit(func(f *flag.Flag) { given = given || f.Name == "label" })
	if !given {
		label = auth.MachineLabelFor(hostname())
	}
	if label == "" {
		return refuseJoin("say what to call this machine: `%s net join --label <label>`", invoked)
	}

	var prefix []string
	if !root {
		sudo, err := lookSudo()
		if err != nil {
			return refuseJoin("joining needs root for one step, and there is no sudo on the PATH to ask it with: %v", err)
		}
		prefix = []string{sudo, "--"}
	}
	self, err := thisBinary()
	if err != nil {
		diagnosticf(stderr, "%s net join: %v\n", invoked, err)
		return 1
	}
	// The install would refuse to place the binary under a directory another
	// account owns — /usr/local/bin on an Intel Mac whose Homebrew gave it to
	// the person — and by then the key is spent. Asking first costs a Stat.
	if err := checkPlacement(placedBinary); err != nil {
		return refuseJoin("%v", err)
	}

	store := state.New()
	origin, err := auth.ResolveOrigin(*host, store)
	if err != nil {
		return refuseJoin("%v", err)
	}
	signedIn, err := store.LoadAuthFor(origin.URL)
	switch {
	case state.IsNotLoggedIn(err):
		return refuseJoin("not signed in to %s — run `%s login` first", origin.URL, invoked)
	case err != nil:
		return refuseJoin("%v", err)
	}

	// From here on an interrupt is the wait's to answer: the staging directory
	// is removed on every way out, which a process killed by a Ctrl-C, or by
	// its terminal closing (SIGHUP), would skip.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()

	material, err := fetchEnrollment(ctx, nil, origin.URL, signedIn.AccessToken, label)
	if err != nil {
		diagnosticf(stderr, "%s net join: %v\n", invoked, err)
		return 1
	}

	material.Origin = origin.URL
	staging, err := stageMaterial(store.Dir, material)
	if staging != "" {
		defer os.RemoveAll(staging)
	}
	if err != nil {
		diagnosticf(stderr, "%s net join: %v\n", invoked, err)
		return 1
	}

	started := time.Now()
	argv := append(prefix, self, "install", "--interface", "--source-state-dir", staging, "--place-binary", placedBinary)
	if err := elevate(argv, os.Stdin, stdout, stderr); err != nil {
		// An exit status means sudo or the install ran and said why on the
		// terminal; anything else is sudo never starting, which said nothing.
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			diagnosticf(stderr, "%s net join: running sudo: %v; nothing was joined\n", invoked, err)
			return 1
		}
		diagnosticf(stderr, "%s net join: sudo or the install failed (see above); nothing was joined\n", invoked)
		return 1
	}

	warningBinary = placedBinary
	joined, err := awaitJoin(ctx, started)
	switch {
	case errors.Is(err, context.Canceled):
		diagnosticf(stderr, "%s net join: stopped waiting; the daemon is installed — check `%s net status`\n", invoked, invoked)
		return 1
	case err != nil:
		diagnosticf(stderr, "%s net join: the daemon is installed but has not said it is on 6net after %s — check `%s net status` and `journalctl -u gentility-connector`\n", invoked, seconds(waitFor), invoked)
		return 1
	}
	fmt.Fprint(stdout, joinedLines(joined))
	return exitcodes.OK
}

// seconds says a wait the way a person reads it, "60s" rather than Duration's
// "1m0s".
func seconds(d time.Duration) string { return fmt.Sprintf("%.0fs", d.Seconds()) }

// thisBinary is the file this process runs, symlinks resolved: what the
// elevated install copies to placedBinary.
func thisBinary() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("finding this binary: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(exe)
	if err != nil {
		return "", fmt.Errorf("resolving %s: %w", exe, err)
	}
	return resolved, nil
}

// stageMaterial writes the material into a fresh directory under the person's
// state directory, for the elevated install to take and clear. The directory
// is 0700 and the file 0600, so between the fetch and the install nobody but
// the person and root can read the key. It is not /tmp, where the name alone
// would be visible to everyone.
//
// SaveEnrollment's write opens its directory to 0711, so the mode is set back
// after it; the file is 0600 throughout. The directory's path is returned even
// when the write failed, so the caller removes what was made.
func stageMaterial(stateDir string, material state.Enrollment) (string, error) {
	if err := os.MkdirAll(stateDir, 0o711); err != nil {
		return "", fmt.Errorf("creating %s: %w", stateDir, err)
	}
	dir, err := os.MkdirTemp(stateDir, "net-join-")
	if err != nil {
		return "", fmt.Errorf("staging the join material in %s: %w", stateDir, err)
	}
	if err := (&state.Store{Dir: dir}).SaveEnrollment(material); err != nil {
		return dir, fmt.Errorf("staging the join material: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return dir, fmt.Errorf("securing %s: %w", dir, err)
	}
	return dir, nil
}

// errNotYet is a wait that ran out.
var errNotYet = errors.New("the daemon has not said it is on 6net")

// awaitJoin waits for the daemon to write a snapshot after started. A snapshot
// from before it is an earlier join's — one a leave did not clear, or one a
// crash left — and reading it as success would claim a join that may never
// happen.
func awaitJoin(ctx context.Context, started time.Time) (state.Status, error) {
	deadline := time.NewTimer(waitFor)
	defer deadline.Stop()
	poll := time.NewTicker(pollEvery)
	defer poll.Stop()

	for {
		if written, err := daemonStatus(); err == nil && !written.WrittenAt.Before(started) {
			return written, nil
		}
		select {
		case <-ctx.Done():
			return state.Status{}, ctx.Err()
		case <-deadline.C:
			return state.Status{}, errNotYet
		case <-poll.C:
		}
	}
}

// joinedLines is what a successful join prints: this machine's 6net name, its
// address in the first organisation and the interface the overlay is on, then
// one line for each further organisation.
func joinedLines(s state.Status) string {
	first := []string{"on 6net"}
	if s.Name != "" {
		first[0] += " as " + s.Name
	}
	orgs := s.Organizations
	if len(orgs) > 0 {
		first = append(first, fmt.Sprintf("%s (%s)", orgs[0].Address, orgs[0].Org))
		orgs = orgs[1:]
	}
	if s.Interface != "" {
		first = append(first, "interface "+s.Interface)
	}

	var b strings.Builder
	b.WriteString(strings.Join(first, "  ") + "\n")
	for _, org := range orgs {
		fmt.Fprintf(&b, "  %s (%s)\n", org.Address, org.Org)
	}
	return b.String()
}

// netLeave is leave, with the sudo asked for here: `sudo -- <binary> leave`,
// the placed binary when there is one, since that is the one the daemon runs.
func netLeave(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet(invoked+" net leave", flag.ContinueOnError)
	flags.SetOutput(io.Discard)

	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return help(stdout, invoked+" net leave", flags)
		}
		diagnosticf(stderr, "%s net leave: %v\n", invoked, err)
		return exitcodes.Usage
	}
	if flags.NArg() > 0 {
		diagnosticf(stderr, "%s net leave: unexpected argument %q\n", invoked, flags.Arg(0))
		return exitcodes.Usage
	}
	if geteuid() == 0 {
		return leave(nil, stdout, stderr)
	}

	sudo, err := lookSudo()
	if err != nil {
		diagnosticf(stderr, "%s net leave: leaving needs root, and there is no sudo on the PATH to ask it with: %v\n", invoked, err)
		return exitcodes.Usage
	}
	binary := placedBinary
	if _, err := os.Stat(placedBinary); err != nil {
		if binary, err = thisBinary(); err != nil {
			diagnosticf(stderr, "%s net leave: %v\n", invoked, err)
			return 1
		}
	}

	if err := elevate([]string{sudo, "--", binary, "leave"}, os.Stdin, stdout, stderr); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return exit.ExitCode()
		}
		diagnosticf(stderr, "%s net leave: %v\n", invoked, err)
		return 1
	}
	return exitcodes.OK
}

// netServe opens this machine's ssh to its owner's other machines on 6net:
// `net serve ssh` declares it in every organization the daemon's status.json
// lists, naming this machine by its address there.
//
// The declaration goes to the mothership as the person, with their bearer, and
// not into the daemon. Whether a port is open is the person's decision, and
// the mothership is where a decision is recorded, checked against whose machine
// this is and approved; a heartbeat states what the daemon runs, and may not
// claim ssh at all. So nothing here needs sudo, and nothing restarts or
// rejoins. sshd needs nothing from the connector either: it already listens on
// gentility0, and once the filter admits the SYN the kernel answers it.
//
// An admin's declaration is approved in the same call; a member's waits for
// an admin, and the line says which and how.
func netServe(args []string, stdout, stderr io.Writer) int {
	return netService("serve", "net_service_declare", args, stdout, stderr,
		func(org state.OrgStatus, port int, payload map[string]any) string {
			name, _ := payload["name"].(string)
			if name == "" {
				name = nodeName(org)
			}
			if approved, _ := payload["approved"].(bool); approved {
				return fmt.Sprintf("ssh on %s:%d — approved; your other machines can reach it", name, port)
			}
			service, _ := payload["service"].(map[string]any)
			id, _ := service["id"].(string)
			return fmt.Sprintf("ssh on %s:%d — declared; an %s admin must approve it (net_service_approve %s)", name, port, org.Org, id)
		})
}

// netUnserve withdraws what netServe declared, and with it every path to it.
func netUnserve(args []string, stdout, stderr io.Writer) int {
	return netService("unserve", "net_service_remove", args, stdout, stderr,
		func(org state.OrgStatus, port int, _ map[string]any) string {
			return fmt.Sprintf("ssh on %s:%d withdrawn", nodeName(org), port)
		})
}

// notServed is how net_service_remove says there is nothing on that port: a
// prefix of the message the network-service tool gives
// :service_not_found, which names this constant back. The tool's refusal
// carries only the sentence, so the sentence is what is read, and the two
// change together.
const notServed = "No service is open on that port"

// netService is serve and unserve's shared body: the argument, the refusals
// that need no request, then one call per organization. A tool's refusal is
// that organization's and the rest are still asked; any other failure — the
// sign-in, the session, the network — would be the same answer from each, so
// it is said once and the run stops.
func netService(verb, tool string, args []string, stdout, stderr io.Writer,
	line func(org state.OrgStatus, port int, payload map[string]any) string) int {
	command := "net " + verb
	flags := flag.NewFlagSet(invoked+" "+command, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	port := flags.Int("port", 22, "the port the service listens on")

	refuse := func(format string, a ...any) int {
		diagnosticf(stderr, "%s %s: "+format+"\n", append([]any{invoked, command}, a...)...)
		return exitcodes.Usage
	}

	// The kind comes first and the flags after it, as a person types them,
	// and the flag package stops at the first word: parse around it.
	kind := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		kind, args = args[0], args[1:]
	}
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return help(stdout, invoked+" "+command+" ssh [--port 22]", flags)
		}
		return refuse("%v", err)
	}
	if flags.NArg() > 0 {
		return refuse("unexpected argument %q", flags.Arg(0))
	}
	switch kind {
	case "ssh":
	case "":
		return refuse("say what to %s: `%s %s ssh`", verb, invoked, command)
	default:
		return refuse("ssh is the only service a person's machine can %s, not %q", verb, kind)
	}
	if *port < 1 || *port > 65535 {
		return refuse("--port must be from 1 to 65535, not %d", *port)
	}

	// The bearer is the person's, so the process holding it is theirs, as
	// `net join` has it.
	if geteuid() == 0 && sudoUser() != "" {
		return refuse("run it as yourself, without sudo: `%s %s ssh`", invoked, command)
	}

	joined, err := daemonStatus()
	switch {
	case errors.Is(err, state.ErrNoStatus) || err == nil && len(joined.Organizations) == 0:
		return refuse("this machine is not on 6net — `%s net join` first", invoked)
	case err != nil:
		diagnosticf(stderr, "%s %s: reading what the daemon last said: %v\n", invoked, command, err)
		return 1
	}

	store := state.New()
	code := exitcodes.OK
	for _, org := range joined.Organizations {
		// An organization whose address the daemon has not learned yet has
		// no name for this machine to send; an empty node would only come
		// back as "not found", so say what is actually wrong.
		if org.Address == "" {
			diagnosticf(stderr, "%s %s: %s: the daemon does not know this machine's address there yet; try again once `%s net status` shows it\n", invoked, command, org.Org, invoked)
			code = 1
			continue
		}

		// Both tools are told the kind. Withdrawing names it too, so that
		// `net unserve ssh` withdraws ssh and never whatever else holds the
		// port.
		arguments := map[string]any{"organization": org.Org, "node": org.Address, "kind": kind, "port": *port}

		payload, err := callTool(store, tool, arguments)
		var refused *mcp.ToolRefusedError
		switch {
		case err == nil:
			fmt.Fprintln(stdout, line(org, *port, payload))
		case !errors.As(err, &refused):
			diagnosticf(stderr, "%s %s: %v\n", invoked, command, err)
			return 1
		case verb == "unserve" && strings.HasPrefix(fmt.Sprint(refused.Value), notServed):
			// Withdrawing what is already withdrawn has the outcome asked for.
			fmt.Fprintf(stdout, "ssh was not being served on %s:%d\n", nodeName(org), *port)
		default:
			diagnosticf(stderr, "%s %s: %s: %v\n", invoked, command, org.Org, err)
			code = 1
		}
	}
	return code
}

// nodeName is this machine's 6net name in an organization, or its address
// there when the daemon could not tell the name.
func nodeName(org state.OrgStatus) string {
	if org.Name != "" {
		return org.Name
	}
	return org.Address
}
