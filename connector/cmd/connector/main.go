// Command connector is the Gentility Connector: one node on the org overlay.
//
// Subcommands cover login, enrollment, network services and task tools.
//
// All the behaviour lives in run; main only turns its exit code into an exit.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	"github.com/gentility-ai/gentility/connector/internal/auth"
	"github.com/gentility-ai/gentility/connector/internal/exitcodes"
	"github.com/gentility-ai/gentility/connector/internal/mcp"
	"github.com/gentility-ai/gentility/connector/internal/serve"
	"github.com/gentility-ai/gentility/connector/internal/state"
	"github.com/gentility-ai/gentility/connector/internal/status"
	"github.com/gentility-ai/gentility/connector/internal/supervise"
	"github.com/gentility-ai/gentility/connector/internal/tasks"
	"github.com/gentility-ai/gentility/connector/internal/terminal"
	"github.com/gentility-ai/gentility/connector/internal/version"
)

// DefaultName is what this binary is called when nothing says otherwise: the
// name a server installs, and the name the `gentility` symlink points at.
const DefaultName = "connector"

// invoked is the name this run was called by.
//
// One binary answers to two names — `connector` is what a server installs,
// `gentility` is what a person types (connector/README.md) — and they are the same
// file, not two builds. So everything this layer says about itself is built
// from the name in front of the reader: a `gentility` that answered `--help`
// with `connector task add …` would be telling a person to type a command
// they did not install.
var invoked = commandName(os.Args)

// commandName reduces an argv to the name a person sees. A `.exe` is trimmed
// because Windows adds it to the file, not to the command anyone types.
func commandName(argv []string) string {
	if len(argv) == 0 {
		return DefaultName
	}

	name := filepath.Base(argv[0])
	if extension := filepath.Ext(name); strings.EqualFold(extension, ".exe") {
		name = strings.TrimSuffix(name, extension)
	}

	// filepath.Base answers "." for an empty path and "/" for a bare
	// separator; neither is a name.
	if name == "" || name == "." || name == string(filepath.Separator) {
		return DefaultName
	}
	return name
}

// main prints every goroutine on a fatal panic, not only the panicking one.
// The overlay's watchdog panics from its own timer goroutine when the backend
// stalls (`ipnlocal: watchdog timeout`), and Go's default trace shows only that
// goroutine: the one holding the lock, which is the one worth reading, is left
// out. A GOTRACEBACK in the environment may raise this, not lower it.
func main() {
	debug.SetTraceback("all")
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run dispatches one invocation and returns the process exit code: 0 on
// success, 1 on a failed command, 2 on a usage error.
func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		usage(stderr)
		return 2
	}

	switch args[0] {
	case "version":
		fmt.Fprintln(stdout, version.String())
		return 0
	case "join":
		return join(args[1:], stdout, stderr)
	case "gateway":
		return runGateway(args[1:], stdout, stderr)
	case "egress":
		return runEgress(args[1:], stdout, stderr)
	case "proxy":
		return runProxy(args[1:], stdout, stderr)
	case "serve":
		return runServe(args[1:], stdout, stderr)
	case "status":
		return runStatus("status", args[1:], stdout, stderr)
	case "login":
		return login(args[1:], stdout, stderr)
	case "logout":
		return logout(args[1:], stdout, stderr)
	case "install":
		return install(args[1:], stdout, stderr)
	case "uninstall":
		return uninstall(args[1:], stdout, stderr)
	case "leave":
		return leave(args[1:], stdout, stderr)
	case "net":
		return netCommand(args[1:], stdout, stderr)
	case "cleanup":
		return cleanup(args[1:], stdout, stderr)
	case "tree":
		return treeCommand(args[1:], stdout, stderr)
	case "task":
		return task(args[1:], stdout, stderr)
	default:
		diagnosticf(stderr, "%s: unknown subcommand %q\n", invoked, args[0])
		usage(stderr)
		return 2
	}
}

// login authorizes this machine against an MCP host through the device flow.
// The wait is unbounded on purpose — it lasts as long as the device code the
// server minted, which is how long a person has to reach a browser.
//
// --server says this machine is being enrolled as an organization's server, so
// the approval has to name one and the token comes back with the join material
// `serve` needs. The flag is not sent anywhere: the browser is where a server
// is chosen, and this is only what the connector expects back.
//
// Without --server a login is a login: it stores the person's bearer and
// nothing else. Joining this machine to 6net is `gentility net join`, which
// fetches its join material at the moment it joins, because that material dies
// ten minutes after it is minted.
func login(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet(invoked+" login", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	host := flags.String("url", "", "the Gentility MCP host to sign in to")
	server := flags.Bool("server", false, "enrol this machine as a server for an organization (the approval must name one)")

	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return help(stdout, invoked+" login [--url <base>] [--server]", flags)
		}
		diagnosticf(stderr, "%s login: %v\n", invoked, err)
		return 2
	}
	if flags.NArg() > 0 {
		diagnosticf(stderr, "%s login: unexpected argument %q\n", invoked, flags.Arg(0))
		return 2
	}

	warnCurrentPath(stderr)
	store := state.New()
	origin, err := auth.ResolveOrigin(*host, store)
	if err != nil {
		diagnosticf(stderr, "%s login: %v\n", invoked, err)
		return 1
	}
	if err := auth.Login(context.Background(), origin, store, stdout, auth.Options{Server: *server}); err != nil {
		diagnosticf(stderr, "%s login: %v\n", invoked, err)
		return 1
	}
	if !*server {
		fmt.Fprintf(stdout, "To put this machine on 6net: %s net join\n", invoked)
	}
	return 0
}

// runServe holds this machine on its organization's overlay for as long as
// something lets it: the long-lived half of the connector, and what `install`
// puts under launchd or systemd.
//
// It is the one command that outlives the person who typed it, so everything it
// says it says through the exit code as well: a refusal exits the code that
// names it and the unit files list those as the ones not to restart into, while
// anything retryable exits Temporary and a supervisor is expected to come
// straight back. A clean stop — the SIGTERM that stops a unit — is a zero.
//
// The flags are the things an enrollment cannot carry, because none of them is
// about the grant: where this machine keeps the identity it earns, what the
// head should call it, which port it offers its relay on, and whether the
// overlay goes on a real network interface or stays inside this process.
func runServe(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet(invoked+" serve", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	stateDir := flags.String("state-dir", state.DefaultDir(), "the directory this node keeps its identity in")
	hostname := flags.String("hostname", defaultHostname(), "the name to join the head under, on a first start")
	egressPort := flags.Int("egress-port", serve.DefaultEgressPort, "the port to advertise this node's egress relay on")
	iface := flags.Bool("interface", false, "put the overlay on a real network interface, which needs root (install --interface sets it)")

	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return help(stdout, invoked+" serve [--state-dir <dir>] [--hostname <name>] [--egress-port <port>] [--interface]", flags)
		}
		diagnosticf(stderr, "%s serve: %v\n", invoked, err)
		return exitcodes.Usage
	}
	if flags.NArg() > 0 {
		diagnosticf(stderr, "%s serve: unexpected argument %q\n", invoked, flags.Arg(0))
		return exitcodes.Usage
	}

	say, complain := narrate("serve", stdout, stderr)
	say = stamped(say, time.Now)

	// SIGTERM and SIGINT are how a supervisor stops a node. Both end the two
	// loops and shut the relay down, so the tunnels in flight report their
	// bytes rather than losing them to exit.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	opts := serve.Options{
		StateDir: *stateDir, Hostname: *hostname, EgressPort: *egressPort, Interface: *iface,
	}

	if err := serve.Run(ctx, opts, serve.Default(say)); err != nil {
		return complain(err)
	}
	return exitcodes.OK
}

// runStatus answers the four questions a person standing at this machine has:
// is it enrolled, has it joined, is the daemon running, and what is it serving.
//
// **All four exit 0.** "Not enrolled" is the first step and not a failure, and a
// person who typed `status` to find out has done nothing wrong — a non-zero
// would make the obvious health check fire on every fresh machine. The only
// non-zero is a state directory this process cannot read, which is a real
// failure and not an answer.
//
// It reads the daemon's snapshot rather than asking it anything: this is a
// separate process, and internal/status says why that is the whole design.
//
// Which snapshot is statusDir's choice, and unless --state-dir made it, the
// first line says whose directory was read.
//
// command is how it was asked for, `status` or `net status`, so its usage and
// its errors name what the person typed.
func runStatus(command string, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet(invoked+" "+command, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	stateDir := flags.String("state-dir", "", "the directory this node keeps its identity in (default: the installed daemon's, else this user's)")
	asJSON := flags.Bool("json", false, "print the snapshot as JSON rather than as a block to read")

	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return help(stdout, invoked+" "+command+" [--state-dir <dir>] [--json]", flags)
		}
		diagnosticf(stderr, "%s %s: %v\n", invoked, command, err)
		return exitcodes.Usage
	}
	if flags.NArg() > 0 {
		diagnosticf(stderr, "%s %s: unexpected argument %q\n", invoked, command, flags.Arg(0))
		return exitcodes.Usage
	}

	installed := supervise.UnitInstalled(runtime.GOOS, "/", supervise.DefaultLabel)
	dir := statusDir(installed, *stateDir)

	snapshot, err := status.Load(&state.Store{Dir: dir}, time.Now())
	if err != nil {
		diagnosticf(stderr, "%s %s: %v\n", invoked, command, err)
		return 1
	}

	if *asJSON {
		encoded, err := json.MarshalIndent(snapshot, "", "  ")
		if err != nil {
			diagnosticf(stderr, "%s %s: %v\n", invoked, command, err)
			return 1
		}
		fmt.Fprintln(stdout, string(encoded))
		return exitcodes.OK
	}

	if *stateDir == "" {
		whose := "this user"
		if installed {
			whose = "daemon"
		}
		fmt.Fprintf(stdout, "state: %s (%s)\n", dir, whose)
	}
	fmt.Fprint(stdout, status.Render(snapshot))
	return exitcodes.OK
}

// statusDir is the directory status reads: the one it was told, else the
// installed daemon's — on a machine that installed one, that is where its
// identity lives, and the person's own directory holds only what login left —
// else this user's.
func statusDir(installed bool, explicit string) string {
	switch {
	case explicit != "":
		return explicit
	case installed:
		return supervise.DefaultStateDir
	default:
		return state.DefaultDir()
	}
}

// defaultHostname is what the head calls this machine unless somebody says
// otherwise. A machine that cannot say its own name leaves it empty rather than
// inventing one; enroll fills it in, and says so if it cannot either.
func defaultHostname() string {
	name, err := os.Hostname()
	if err != nil {
		return ""
	}
	return name
}

// narrate is how a long-running command talks to whoever started it: say for
// the ordinary line on stdout, complain for the one that ends the process.
//
// complain is the whole of the exit-code conversation, and it is here because
// there is only one of it to have. A refusal that named its own code keeps it —
// Usage means fix the recipe, EnrollRefused means enrol again — and everything
// else is Temporary, which is a supervisor's instruction to start the command
// again. serve, join, gateway, egress and proxy all say it the same way.
func narrate(verb string, stdout, stderr io.Writer) (func(string, ...any), func(error) int) {
	say := func(format string, args ...any) { fmt.Fprintf(stdout, format+"\n", args...) }

	complain := func(err error) int {
		diagnosticf(stderr, "%s %s: %v\n", invoked, verb, err)
		if code, fatal := exitcodes.Of(err); fatal {
			return code
		}
		return exitcodes.Temporary
	}

	return say, complain
}

// stamped puts the moment first, UTC, RFC 3339. serve is the one command whose
// output is a log file read later, under launchd or systemd, and a path change
// the overlay reports as "now using" means nothing without its time. The other
// commands speak to a person at a terminal and stay bare.
func stamped(say func(string, ...any), now func() time.Time) func(string, ...any) {
	return func(format string, args ...any) {
		say("%s "+format, append([]any{now().UTC().Format(time.RFC3339)}, args...)...)
	}
}

// libraryLogger is the --verbose overlay logger, or nil when quiet — the same
// choice join, gateway, egress and proxy all make, so it is made once.
//
// nil rather than a no-op writer because that is what tsnet reads as "say
// nothing"; a function that discarded would still have the library format
// every line it was about to throw away.
func libraryLogger(verbose bool, stderr io.Writer) func(string, ...any) {
	if !verbose {
		return nil
	}
	return func(format string, args ...any) { diagnosticf(stderr, format+"\n", args...) }
}

func logout(args []string, stdout, stderr io.Writer) int {
	// A flag set with no flags, so that --help is answered here the way it is
	// answered everywhere else rather than refused as a stray word.
	flags := flag.NewFlagSet(invoked+" logout", flag.ContinueOnError)
	flags.SetOutput(io.Discard)

	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return help(stdout, invoked+" logout", flags)
		}
		diagnosticf(stderr, "%s logout: %v\n", invoked, err)
		return 2
	}
	if flags.NArg() > 0 {
		diagnosticf(stderr, "%s logout: unexpected argument %q\n", invoked, flags.Arg(0))
		return 2
	}

	if err := auth.Logout(state.New(), stdout); err != nil {
		diagnosticf(stderr, "%s logout: %v\n", invoked, err)
		return 1
	}
	return 0
}

// install puts `connector serve` under this machine's supervisor — launchd on
// macOS, systemd on Linux — and starts it.
//
// It needs root, because a system daemon is written where only root writes. It
// also carries the enrollment `connector login --server` earned into the
// directory the daemon will run against: the login happens as a person and the
// daemon does not run as that person, and this is the only moment that can
// bridge the two.
//
// Which is why it takes --source-state-dir. sudo resets HOME, so the person's
// state directory has to be worked out from $SUDO_USER rather than read off
// the environment; the flag is there for the day that guess is wrong.
//
// The unattended install skips the person entirely: `net_machine_enroll` mints
// a machine's trio, the recipe exports it as the OVERLAY_* variables, and
// `sudo -E connector install` stages what the environment carries. --replace is
// what that install needs when the machine is coming back rather than arriving
// — it discards the identity in the state directory, which is otherwise the
// one thing an install refuses to decide on its own.
//
// --place-binary is Desk's: it runs install from inside its own bundle, which
// the person owns, so the daemon runs a root-owned copy placed at the path it
// names instead.
func install(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet(invoked+" install", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	stateDir := flags.String("state-dir", supervise.DefaultStateDir, "the directory the daemon keeps its identity in")
	logDir := flags.String("log-dir", supervise.DefaultLogDir, "the directory the daemon's output goes to")
	binary := flags.String("binary", "", "the connector binary to supervise (default: this one, with its symlinks resolved)")
	place := flags.String("place-binary", "", "copy this binary to PATH (root-owned) and run the daemon from there")
	label := flags.String("label", supervise.DefaultLabel, "the name the service is filed under")
	sourceDir := flags.String("source-state-dir", "", "the directory connector login --server left the enrollment in (default: the state directory of the user who ran sudo)")
	replace := flags.Bool("replace", false, "discard the identity this machine already holds and join on the enrollment it is being handed")
	iface := flags.Bool("interface", false, "run the daemon with a real network interface (needs root; for a person's machine)")

	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return help(stdout, invoked+" install [--state-dir <dir>] [--log-dir <dir>] [--binary <path> | --place-binary <path>] [--label <label>] [--source-state-dir <dir>] [--replace] [--interface]", flags)
		}
		diagnosticf(stderr, "%s install: %v\n", invoked, err)
		return exitcodes.Usage
	}
	if flags.NArg() > 0 {
		diagnosticf(stderr, "%s install: unexpected argument %q\n", invoked, flags.Arg(0))
		return exitcodes.Usage
	}

	opts := supervise.Options{
		BinaryPath:      *binary,
		PlaceBinary:     *place,
		StateDir:        *stateDir,
		LogDir:          *logDir,
		Label:           *label,
		SourceStateDir:  *sourceDir,
		ReplaceIdentity: *replace,
		Interface:       *iface,
		Out:             stdout,
	}
	if err := supervise.Install(context.Background(), opts, supervise.Exec{Stderr: stderr}); err != nil {
		diagnosticf(stderr, "%s install: %v\n", invoked, err)
		// A machine that has not been enrolled has not failed at anything; it
		// has been asked to do the second step first.
		if state.IsNoEnrollment(err) || errors.Is(err, supervise.ErrBinaryAndPlaceBinary) || errors.Is(err, supervise.ErrPlaceBinaryPath) || errors.Is(err, supervise.ErrUntrustedBinary) {
			return exitcodes.Usage
		}
		return 1
	}
	if *place != "" {
		warnPathShadow(stderr, *place, *place)
	}
	return 0
}

// uninstall stops supervising the connector and removes the unit. It says no
// more than that: the state directory, and with it this machine's identity on
// the overlay, stays where it is.
//
// It takes no --log-dir or --binary because neither is part of stopping: the
// unit file being removed already names them, and a flag that changes nothing
// is a flag someone will one day believe.
func uninstall(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet(invoked+" uninstall", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	stateDir := flags.String("state-dir", supervise.DefaultStateDir, "the directory the daemon keeps its identity in, which is left in place")
	label := flags.String("label", supervise.DefaultLabel, "the name the service is filed under")

	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return help(stdout, invoked+" uninstall [--state-dir <dir>] [--label <label>]", flags)
		}
		diagnosticf(stderr, "%s uninstall: %v\n", invoked, err)
		return exitcodes.Usage
	}
	if flags.NArg() > 0 {
		diagnosticf(stderr, "%s uninstall: unexpected argument %q\n", invoked, flags.Arg(0))
		return exitcodes.Usage
	}

	opts := supervise.Options{StateDir: *stateDir, Label: *label, Out: stdout}
	return uninstallWithRunner(context.Background(), opts, supervise.Exec{Stderr: stderr}, stderr)
}

func uninstallWithRunner(ctx context.Context, opts supervise.Options, runner supervise.Runner, stderr io.Writer) int {
	store := &state.Store{Dir: opts.StateDir}
	if err := auth.Revoke(ctx, store); err != nil {
		if errors.Is(err, auth.ErrReadLogin) {
			fmt.Fprintln(stderr, "Warning: could not read the stored login. Server sign-out was not attempted; local state was kept.")
		} else {
			fmt.Fprintln(stderr, "Warning: server sign-out could not be confirmed. The server session may remain live until it expires. Local identity and login were kept.")
		}
	} else if err := store.ClearAuth(); err != nil {
		fmt.Fprintln(stderr, "Warning: server sign-out was confirmed, but the stored login could not be removed.")
	}
	if err := supervise.Uninstall(ctx, opts, runner); err != nil {
		// A refused stop is the note Uninstall has already printed, and, as
		// it always has been for uninstall, not a failure: a second uninstall
		// is a supervisor asked to stop what it is not running.
		var stop *supervise.StopError
		if errors.As(err, &stop) {
			return 0
		}
		diagnosticf(stderr, "%s uninstall: %v\n", invoked, err)
		return 1
	}
	return 0
}

// task dispatches the four task verbs. Each one is a single tools/call
// against the MCP host this laptop is signed in to, so the work is all in
// mapping words onto arguments (internal/tasks) and printing what came back.
func task(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		taskUsage(stderr)
		return 2
	}

	switch args[0] {
	case "add":
		return taskAdd(args[1:], stdout, stderr)
	case "list":
		return taskList(args[1:], stdout, stderr)
	case "check":
		return taskCheck(args[1:], stdout, stderr)
	case "close":
		return taskClose(args[1:], stdout, stderr)
	default:
		diagnosticf(stderr, "%s task: unknown subcommand %q\n", invoked, args[0])
		taskUsage(stderr)
		return 2
	}
}

// taskAdd creates one task. The arguments are decided before anything is
// opened: a missing kind or a missing organization is a sentence here, not a
// round trip that comes back refused.
func taskAdd(argv []string, stdout, stderr io.Writer) int {
	store := state.New()

	args, err := tasks.AddArgs(argv, store.RecentOrg())
	if err != nil {
		return refuse("task add", err, stdout, stderr)
	}

	payload, code := call(store, "task add", "task_create", args, stderr)
	if code != 0 {
		return code
	}

	created, err := tasks.DecodeTask(payload)
	if err != nil {
		diagnosticf(stderr, "%s task add: %v\n", invoked, err)
		return 1
	}

	// dedup_key means a retry returns the task it returned last time, and
	// saying "created" about a task that already existed is the connector
	// claiming something it did not do.
	verb := "created"
	if deduplicated, _ := payload["deduplicated"].(bool); deduplicated {
		verb = "already there"
	}
	fmt.Fprintf(stdout, "%s %s  %s\n", verb, terminal.Text(created.ID), terminal.Text(created.Title))
	return 0
}

// taskList prints the organization's tasks. With --json it prints the tool's
// payload instead, for the person piping it into jq — the rendered table is
// for reading, and reformatting it in a script is nobody's idea of an API.
func taskList(argv []string, stdout, stderr io.Writer) int {
	store := state.New()

	args, err := tasks.ListArgs(argv, store.RecentOrg())
	if err != nil {
		return refuse("task list", err, stdout, stderr)
	}

	payload, code := call(store, "task list", "task_list", args, stderr)
	if code != 0 {
		return code
	}

	if tasks.ListWantsJSON(argv) {
		return printJSON(payload, stdout, stderr, "task list")
	}

	listed, err := tasks.DecodeTasks(payload)
	if err != nil {
		diagnosticf(stderr, "%s task list: %v\n", invoked, err)
		return 1
	}
	fmt.Fprint(stdout, tasks.RenderTasks(listed))
	return 0
}

// taskCheck marks one checklist item done and says which one — a position
// alone is a number nobody can check against what they meant — then prints
// where that leaves the task.
//
// The task is printed as the row `task list` prints, unprefixed and with the
// id at column 0, so what changed is read the same way it was read a moment
// ago. What happened is its own line above it.
func taskCheck(argv []string, stdout, stderr io.Writer) int {
	store := state.New()

	args, err := tasks.CheckArgs(argv, store.RecentOrg())
	if err != nil {
		return refuse("task check", err, stdout, stderr)
	}

	checked, code := taskCall(store, "task check", "task_check", args, stderr)
	if code != 0 {
		return code
	}
	fmt.Fprintln(stdout, tasks.CheckedLine(checked, args))
	fmt.Fprint(stdout, tasks.RenderTasks([]tasks.Task{checked}))
	return 0
}

// taskClose closes a task with a disposition and a cause, and prints the state
// it left behind in the same shape `task check` does.
func taskClose(argv []string, stdout, stderr io.Writer) int {
	store := state.New()

	args, err := tasks.CloseArgs(argv, store.RecentOrg())
	if err != nil {
		return refuse("task close", err, stdout, stderr)
	}

	closed, code := taskCall(store, "task close", "task_close", args, stderr)
	if code != 0 {
		return code
	}
	fmt.Fprintln(stdout, "closed")
	fmt.Fprint(stdout, tasks.RenderTasks([]tasks.Task{closed}))
	return 0
}

// taskCall runs one tool that answers with a single task, and hands it back
// for the caller to say what happened to it.
func taskCall(store *state.Store, verb, tool string, args map[string]any, stderr io.Writer) (tasks.Task, int) {
	payload, code := call(store, verb, tool, args, stderr)
	if code != 0 {
		return tasks.Task{}, code
	}

	task, err := tasks.DecodeTask(payload)
	if err != nil {
		diagnosticf(stderr, "%s %s: %v\n", invoked, verb, err)
		return tasks.Task{}, 1
	}
	return task, 0
}

// refuse turns a mapper's refusal into an exit code. `--help` is the one
// "error" that is not one: a person asking what a command takes has not
// failed, so it is answered on stdout and worth a 0.
func refuse(verb string, err error, stdout, stderr io.Writer) int {
	var asked *tasks.HelpError
	if errors.As(err, &asked) {
		return help(stdout, invoked+" "+asked.Synopsis, asked.Flags)
	}
	diagnosticf(stderr, "%s %s: %v\n", invoked, verb, err)
	return 2
}

// help answers --help: one line saying what the command takes, then what each
// of its flags means, printed from the very flag set the parse used so a flag
// cannot go undocumented. It returns the exit code, which is 0.
func help(w io.Writer, synopsis string, flags *flag.FlagSet) int {
	fmt.Fprintf(w, "usage: %s\n", synopsis)

	described := 0
	flags.VisitAll(func(*flag.Flag) { described++ })
	if described > 0 {
		fmt.Fprintln(w)
		flags.SetOutput(w)
		flags.PrintDefaults()
	}
	return 0
}

// printJSON writes the tool's payload back out, indented. It is the payload
// the MCP client decoded rather than the bytes that arrived, because those
// bytes are JSON inside a JSON string inside an MCP envelope, and unwrapping
// them is the whole reason this connector exists.
func printJSON(payload map[string]any, stdout, stderr io.Writer, verb string) int {
	encoded, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		diagnosticf(stderr, "%s %s: %v\n", invoked, verb, err)
		return 1
	}
	// JSON escapes C0 itself, but DEL and C1 are valid literal JSON runes
	// with terminal effects. Escape them without changing the decoded data.
	var safe strings.Builder
	for _, r := range string(encoded) {
		if r >= 127 && r <= 159 {
			fmt.Fprintf(&safe, "\\u%04x", r)
		} else {
			safe.WriteRune(r)
		}
	}
	fmt.Fprintln(stdout, safe.String())
	return 0
}

// call runs one tool against the host this laptop is signed in to, says why
// when it fails, and remembers the organization it named when it succeeds.
func call(store *state.Store, verb, tool string, args map[string]any, stderr io.Writer) (map[string]any, int) {
	payload, err := callTool(store, tool, args)
	if err != nil {
		diagnosticf(stderr, "%s %s: %v\n", invoked, verb, err)
		return nil, 1
	}

	// The organization this call named becomes the default for the next one.
	// It is a local memory of the argument, never a server-side default — see
	// internal/tasks' package doc.
	// A note, not the "<name> <verb>:" prefix a failure carries: the call
	// succeeded, and only the convenience of not retyping --org was lost.
	if org, ok := args["organization"].(string); ok {
		if err := store.SetRecentOrg(org); err != nil {
			diagnosticf(stderr, "note: could not remember %s as the default organization: %v\n", org, err)
		}
	}
	return payload, 0
}

// callTool runs one tool and turns each way it can fail into the sentence that
// says what to do about it. A tool's own refusal comes back as the
// *mcp.ToolRefusedError it is, for a caller that reads one refusal as success.
//
// The origin is decided once and the bearer is loaded for that origin, never
// for "wherever we are pointed": a bearer is minted by one host and means
// nothing anywhere else, so a production token posted at whatever
// $GENTILITY_MCP_URL resolves to is the failure this pairing prevents.
func callTool(store *state.Store, tool string, args map[string]any) (map[string]any, error) {
	origin, err := auth.ResolveOrigin("", store)
	if err != nil {
		return nil, err
	}

	stored, err := store.LoadAuthFor(origin.URL)
	switch {
	case state.IsNotLoggedIn(err):
		return nil, fmt.Errorf("not signed in to %s — run `%s login`", origin.URL, invoked)
	case err != nil:
		return nil, err
	}

	payload, err := mcp.New(origin.URL, stored.AccessToken).CallTool(context.Background(), tool, args)
	if errors.Is(err, mcp.ErrUnauthorized) {
		return nil, fmt.Errorf("your session expired — run `%s login`", invoked)
	}
	// *mcp.ToolRefusedError and *mcp.ToolError both render as the reason the
	// tool or its boundary gave; neither is a status code.
	return payload, err
}

func taskUsage(w io.Writer) { taskUsageAs(invoked, w) }

func taskUsageAs(name string, w io.Writer) {
	fmt.Fprintf(w, `usage: %[1]s task <command> [arguments]

commands:
  add    create a task: %[1]s task add "Rotate the WorkOS key" --kind ops
  list   list tasks: %[1]s task list --mine [--status open] [--limit 20] [--json]
  check  check one item: %[1]s task check <task-id> <item-id-or-position> [--cause "…"]
  close  close a task: %[1]s task close <task-id> --cause "shipped" [--dismiss]

every command takes --org <slug>, defaulting to the last organization used,
and answers --help with its own flags.
`, name)
}

func usage(w io.Writer) { usageAs(invoked, w) }

// usageAs writes the top-level usage under one name. Every line a person could
// retype is built from it, so the help a `gentility` prints is a transcript of
// commands that work when typed back.
func usageAs(name string, w io.Writer) {
	fmt.Fprintf(w, `usage: %[1]s <command> [arguments]

commands:
  join       bring this node up on the org overlay and hold it there
  gateway    run Gentility's own node: terminate TLS on the overlay and forward
  egress     run this node's approved egress CONNECT relay(s) on the overlay
  proxy      run the case-side preflight proxy and print its HTTPS_PROXY address
  serve      hold this machine on its organization's overlay and serve what it
             has been approved for [--state-dir <dir>] [--hostname <name>]
  status     say whether this machine is enrolled, joined and serving, and what
             it holds — the installed daemon's, else this user's
             [--state-dir <dir>] [--json]
  login      authorize this machine against a Gentility MCP host, or enrol it
             as a server for an organization [--url <base>] [--server]
  logout     revoke the server session and forget the stored bearer
  install    supervise this node under launchd or systemd, starting it at boot;
             stages the enrollment the OVERLAY_* environment carries, or the one
             login --server earned [--replace]
  uninstall  stop supervising it; the machine stays enrolled
  leave      remove the daemon and forget this machine's identity; the
             organisation lists the node until an admin revokes it
  net        put this machine on 6net as you (join, asking sudo once), take
             it off (leave), or say how it is (status); open its ssh to your
             other machines (serve ssh) and close it again (unserve ssh)
  cleanup    restore the resolver configuration a stopped interface-mode
             daemon may have left behind [--interface]
  tree       keep a local shared tree (run), install or uninstall its user service
  task       add, list, check and close organization tasks
  version    print the connector, tailscale and genscale versions

examples:
  %[1]s login
  %[1]s net join
  %[1]s net status
  %[1]s net serve ssh
  %[1]s task add "Rotate the WorkOS key" --kind ops --org acme
  %[1]s task list --mine
`, name)
}
