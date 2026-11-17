package tree

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"

	"github.com/gentility-ai/gentility/connector/internal/supervise"
)

func RefuseRoot(uid int, sudoUser string) error {
	if uid == 0 || sudoUser != "" {
		return errors.New("tree: run user tree commands as yourself, without sudo; system use is handled by connector serve daemon mode")
	}
	return nil
}

type InstallOptions struct {
	Unit     supervise.TreeUnit
	GOOS     string
	UID      int
	SudoUser string
	Runner   supervise.Runner
	Trust    Trust
	Out      io.Writer
}

func (o InstallOptions) location() (string, string, error) {
	switch o.GOOS {
	case "darwin":
		return filepath.Join(o.Unit.Home, "Library", "LaunchAgents"), supervise.TreeLabel + ".plist", nil
	case "linux":
		return filepath.Join(o.Unit.Home, ".config", "systemd", "user"), supervise.TreeService, nil
	default:
		return "", "", fmt.Errorf("tree: unsupported OS %s", o.GOOS)
	}
}
func Install(ctx context.Context, o InstallOptions) error {
	if err := RefuseRoot(o.UID, o.SudoUser); err != nil {
		return err
	}
	discovery, err := ReadDiscovery(o.Unit.DiscoveryDir, o.Trust)
	if err != nil {
		return err
	}
	if discovery.Interface == "" {
		return errors.New("tree: connector net join must provide a 6net interface before installing")
	}
	content, err := supervise.RenderTree(o.GOOS, o.Unit)
	if err != nil {
		return err
	}
	dir, name, err := o.location()
	if err != nil {
		return err
	}
	directory, err := openDirectory(dir)
	if err != nil {
		return err
	}
	defer directory.close()
	state, err := openDirectory(o.Unit.StateDir)
	if err != nil {
		return err
	}
	state.close()
	old, err := directory.read(name, 1<<20)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	changed := string(old) != content
	if changed {
		if err := directory.replace(name, []byte(content), 0600); err != nil {
			return err
		}
	}
	return supervise.ActivateTree(ctx, o.Runner, o.GOOS, o.UID, filepath.Join(dir, name), changed, o.Out)
}
func Uninstall(ctx context.Context, o InstallOptions) error {
	if err := RefuseRoot(o.UID, o.SudoUser); err != nil {
		return err
	}
	dir, name, err := o.location()
	if err != nil {
		return err
	}
	directory, err := openExistingDirectory(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer directory.close()
	if _, err := directory.read(name, 1<<20); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	if o.GOOS == "linux" {
		if err := o.Runner.Run(ctx, "systemctl", "--user", "disable", "--now", name); err != nil {
			return err
		}
	} else {
		domain := "gui/" + strconv.Itoa(o.UID) + "/" + supervise.TreeLabel
		if o.Runner.Run(ctx, "launchctl", "print", domain) == nil {
			if err := o.Runner.Run(ctx, "launchctl", "bootout", domain); err != nil {
				return err
			}
		}
	}
	if err := directory.remove(name); err != nil {
		return err
	}
	if o.GOOS == "linux" {
		return o.Runner.Run(ctx, "systemctl", "--user", "daemon-reload")
	}
	return nil
}
