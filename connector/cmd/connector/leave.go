package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"

	"github.com/gentility-ai/gentility/connector/internal/auth"
	"github.com/gentility-ai/gentility/connector/internal/exitcodes"
	"github.com/gentility-ai/gentility/connector/internal/state"
	"github.com/gentility-ai/gentility/connector/internal/supervise"
	"github.com/gentility-ai/gentility/connector/internal/tsnode"
)

// geteuid is os.Geteuid, held in a variable so a test can be a person
// without sudo.
var geteuid = os.Geteuid

// cleanUpInterface is tsnode.CleanUp, held in a variable so a test can watch
// leave run it without touching this machine's resolver.
var cleanUpInterface = func() error { return tsnode.CleanUp(nil) }

// cleanup restores what an interface-mode daemon may have left behind when it
// stopped without closing: the system resolver configuration, and no
// firewall (tsnode.CleanUp says why). The engine does the same at every
// start; this is the same cleanup after a stop, which systemd runs from
// the interface unit's ExecStopPost and leave runs itself, since launchd has
// no such hook.
//
// --interface is required, so the command says what it touches. It needs
// root, because what it restores is root's.
func cleanup(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet(invoked+" cleanup", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	iface := flags.Bool("interface", false, "restore the resolver configuration an interface-mode daemon may have left behind (needs root)")

	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return help(stdout, invoked+" cleanup --interface", flags)
		}
		diagnosticf(stderr, "%s cleanup: %v\n", invoked, err)
		return exitcodes.Usage
	}
	if flags.NArg() > 0 {
		diagnosticf(stderr, "%s cleanup: unexpected argument %q\n", invoked, flags.Arg(0))
		return exitcodes.Usage
	}
	if !*iface {
		diagnosticf(stderr, "%s cleanup: say what to clean up; run: sudo %s cleanup --interface\n", invoked, invoked)
		return exitcodes.Usage
	}
	if geteuid() != 0 {
		diagnosticf(stderr, "%s cleanup: needs root, because the resolver configuration is root's; run: sudo %s cleanup --interface\n", invoked, invoked)
		return exitcodes.Usage
	}
	if err := cleanUpInterface(); err != nil {
		diagnosticf(stderr, "%s cleanup: %v\n", invoked, err)
		return 1
	}
	fmt.Fprintln(stdout, "restored the resolver configuration interface mode changes")
	return exitcodes.OK
}

// leave takes this machine off 6net: it stops and removes the daemon, then
// forgets the identity the daemon joined with. Where uninstall is "stop running
// this here", leave is "this machine is done here" — which is a person's call
// to make about their own machine, and why it is a separate command.
//
// It needs root, because the unit and the daemon's state directory are root's.
func leave(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet(invoked+" leave", flag.ContinueOnError)
	flags.SetOutput(io.Discard)

	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return help(stdout, invoked+" leave", flags)
		}
		diagnosticf(stderr, "%s leave: %v\n", invoked, err)
		return exitcodes.Usage
	}
	if flags.NArg() > 0 {
		diagnosticf(stderr, "%s leave: unexpected argument %q\n", invoked, flags.Arg(0))
		return exitcodes.Usage
	}
	if geteuid() != 0 {
		diagnosticf(stderr, "%s leave: needs root, because the daemon and its identity are root's — run: sudo %s leave\n", invoked, invoked)
		return exitcodes.Usage
	}

	if err := leaveWith(context.Background(), stdout, stderr); err != nil {
		diagnosticf(stderr, "%s leave: %v\n", invoked, err)
		return 1
	}
	return exitcodes.OK
}

// leaveWith is forget against this machine: its supervisor, its root and the
// daemon's state directory. A variable so a test can run `leave` end to end
// against a temporary root and a recorder instead.
var leaveWith = func(ctx context.Context, stdout, stderr io.Writer) error {
	opts := supervise.Options{
		GOOS:     runtime.GOOS,
		Root:     "/",
		Label:    supervise.DefaultLabel,
		StateDir: supervise.DefaultStateDir,
		Out:      stdout,
	}
	return forget(ctx, opts, supervise.Exec{Stderr: stderr})
}

// forget is leave past its flags and its root check: uninstall the daemon,
// then clear the identity in its state directory and any enrollment staged
// there to join on. It goes by what is on disk rather than by the unit alone,
// so a leave that removed the unit and then failed is finished by the next
// one. Only a machine with neither a unit nor any identity has nothing of
// leave's to undo; it is told so and nothing changes, which is also what makes
// a second leave a no-op.
//
// The order matters. The daemon is stopped before its identity goes, so no
// running process is left holding a node whose state was removed under it.
//
// A stop the supervisor refused keeps the identity: a daemon that may still be
// running would otherwise hold a node whose state was removed under it. The
// unit is gone by then (Uninstall finishes regardless), so once the daemon is
// stopped a second leave forgets the identity.
//
// An interface-mode daemon holds resolver configuration (on macOS the files
// under /etc/resolver). Its own Close removes it on a clean stop; a daemon
// killed uncleanly would leave it behind. So after a stop of an interface unit
// leave runs the same cleanup the engine runs at start, on both platforms:
// systemd has already run it from ExecStopPost, launchd has no such hook.
func forget(ctx context.Context, opts supervise.Options, runner supervise.Runner) error {
	daemon := &state.Store{Dir: opts.StateDir}
	if err := auth.RevokeNode(ctx, daemon); err != nil {
		return err
	}
	installed := supervise.UnitInstalled(opts.GOOS, opts.Root, opts.Label)
	if !installed {
		if err := os.Remove(filepath.Join(opts.StateDir, "discovery.json")); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	if !installed && !daemon.HoldsMachine() {
		fmt.Fprintln(opts.Out, "no daemon is installed on this machine")
		return nil
	}

	if installed {
		// Uninstall's own summary ends by saying the identity is left in
		// place, which is true of uninstall and the opposite of what leave is
		// about to do; the one line leave prints below is the whole of what
		// it did.
		iface := supervise.UnitIsInterface(opts.GOOS, opts.Root, opts.Label)
		quiet := opts
		quiet.Out = io.Discard
		if err := supervise.Uninstall(ctx, quiet, runner); err != nil {
			var stop *supervise.StopError
			if errors.As(err, &stop) {
				return fmt.Errorf("%w; this machine's identity is kept: once the daemon is stopped, run leave again to forget it", err)
			}
			return err
		}
		if iface {
			if err := cleanUpInterface(); err != nil {
				fmt.Fprintf(opts.Out, "note: restoring the resolver configuration: %v\n", err)
			}
		}
	}
	// The staged enrollment goes too: left behind, it is material the next
	// install would quietly join on, and a machine that left would be back.
	if err := daemon.ClearIdentity(); err != nil {
		return fmt.Errorf("forgetting the identity in %s: %w", opts.StateDir, err)
	}
	if err := daemon.ClearEnrollment(); err != nil {
		return fmt.Errorf("forgetting the enrollment in %s: %w", opts.StateDir, err)
	}

	fmt.Fprintln(opts.Out, "left 6net: the daemon is removed and this machine's identity is forgotten.")
	return nil
}
