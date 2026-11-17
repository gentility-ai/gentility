package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"syscall"

	"github.com/gentility-ai/gentility/connector/internal/state"
	"github.com/gentility-ai/gentility/connector/internal/supervise"
	"github.com/gentility-ai/gentility/connector/internal/tree"
)

func treeCommand(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		diagnosticln(stderr, invoked+" tree: expected run, install or uninstall")
		return 2
	}
	if err := tree.RefuseRoot(geteuid(), os.Getenv("SUDO_USER")); err != nil {
		diagnosticln(stderr, err)
		return 2
	}
	if args[0] != "run" && args[0] != "install" && args[0] != "uninstall" {
		diagnosticln(stderr, invoked+" tree: expected run, install or uninstall")
		return 2
	}
	flags := flag.NewFlagSet(invoked+" tree "+args[0], flag.ContinueOnError)
	flags.SetOutput(stderr)
	stateDir := flags.String("state-dir", state.DefaultDir(), "your connector login state directory")
	discoveryDir := flags.String("discovery-dir", supervise.DefaultStateDir, "trusted system connector state directory")
	if err := flags.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if flags.NArg() != 0 {
		diagnosticln(stderr, "tree: unexpected arguments")
		return 2
	}
	if args[0] == "install" {
		warnCurrentPath(stderr)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		diagnosticln(stderr, err)
		return 1
	}
	*discoveryDir = tree.DiscoveryDirectory(*discoveryDir)
	store := &state.Store{Dir: *stateDir}
	var cachedDiscovery tree.Discovery
	var cachedBase string
	var cachedClient *http.Client
	defer func() {
		if cachedClient != nil {
			cachedClient.CloseIdleConnections()
		}
	}()
	source := func(context.Context) (tree.Source, error) {
		discovery, err := tree.ReadDiscovery(*discoveryDir, nil)
		if err != nil {
			return tree.Source{}, err
		}
		if cachedClient == nil || discovery != cachedDiscovery {
			base, client, err := discovery.Client()
			if err != nil {
				return tree.Source{}, err
			}
			if cachedClient != nil {
				cachedClient.CloseIdleConnections()
			}
			cachedDiscovery, cachedBase, cachedClient = discovery, base, client
		}
		login, err := treeLogin(store, discovery)
		if err != nil {
			return tree.Source{}, err
		}
		// The OS account owns this cache. A rotated bearer reconciles against the
		// new authorized manifest without changing the root's identity.
		identity := discovery.Origin + ":uid:" + strconv.Itoa(os.Geteuid())
		return tree.Source{
			BaseURL: cachedBase, Bearer: login.AccessToken, Identity: identity,
			Organization: discovery.Organization, OrganizationID: discovery.OrganizationID, HTTP: cachedClient,
		}, nil
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if args[0] == "run" {
		syncer, err := tree.Open(tree.Options{
			Root:     filepath.Join(home, ".gentility", "tree"),
			StateDir: filepath.Join(*stateDir, "tree-sync"), Principal: "person",
			Source: source,
			Log:    func(format string, args ...any) { diagnosticf(stderr, format+"\n", args...) },
		})
		if err != nil {
			diagnosticln(stderr, err)
			return treeExitCode(err)
		}
		defer syncer.Close()
		if err := syncer.Run(ctx); err != nil {
			diagnosticln(stderr, err)
			return treeExitCode(err)
		}
		return 0
	}
	binary, err := os.Executable()
	if err != nil {
		diagnosticln(stderr, err)
		return 1
	}
	options := tree.InstallOptions{
		GOOS: runtime.GOOS, UID: os.Geteuid(), SudoUser: os.Getenv("SUDO_USER"),
		Runner: supervise.Exec{Stderr: stderr}, Out: stderr,
		Unit: supervise.TreeUnit{
			Binary: binary, Home: home, StateDir: *stateDir, DiscoveryDir: *discoveryDir,
		},
	}
	if args[0] == "install" {
		if _, err := source(ctx); err != nil {
			diagnosticln(stderr, err)
			return 1
		}
		err = tree.Install(ctx, options)
	} else {
		err = tree.Uninstall(ctx, options)
	}
	if err != nil {
		diagnosticln(stderr, err)
		return 1
	}
	fmt.Fprintln(stdout, "tree "+args[0]+" complete")
	return 0
}

func treeLogin(store *state.Store, discovery tree.Discovery) (state.Auth, error) {
	login, err := store.LoadAuthFor(discovery.Origin)
	if err != nil {
		return state.Auth{}, fmt.Errorf("tree: log in with connector login --url %s: %w", discovery.Origin, err)
	}
	return login, nil
}

// A deliberate security stop stays stopped under both generated restart
// policies. Unexpected startup failures return nonzero so the unit restarts.
func treeExitCode(err error) int {
	if err == nil || tree.Terminal(err) {
		return 0
	}
	return 1
}
