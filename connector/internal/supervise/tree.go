package supervise

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"strconv"
	"strings"
)

const TreeLabel = "ai.gentility.tree"
const TreeService = "gentility-tree.service"

type TreeUnit struct {
	Binary, Home, StateDir, DiscoveryDir string
}

func (u TreeUnit) args() []string {
	return []string{u.Binary, "tree", "run", "--state-dir", u.StateDir, "--discovery-dir", u.DiscoveryDir}
}
func (u TreeUnit) validate() error {
	for _, value := range []string{u.Binary, u.Home, u.StateDir, u.DiscoveryDir} {
		if !filepath.IsAbs(value) || strings.ContainsAny(value, "\x00\r\n") {
			return fmt.Errorf("tree unit: paths must be absolute and contain no line breaks")
		}
	}
	return nil
}
func RenderTree(goos string, u TreeUnit) (string, error) {
	if err := u.validate(); err != nil {
		return "", err
	}
	switch goos {
	case "darwin":
		var b strings.Builder
		b.WriteString("<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n<plist version=\"1.0\"><dict>\n<key>Label</key><string>" + TreeLabel + "</string>\n<key>ProgramArguments</key><array>\n")
		for _, arg := range u.args() {
			b.WriteString("<string>" + escapeXML(arg) + "</string>\n")
		}
		b.WriteString("</array>\n<key>EnvironmentVariables</key><dict><key>HOME</key><string>" + escapeXML(u.Home) + "</string></dict>\n<key>RunAtLoad</key><true/>\n<key>KeepAlive</key><dict><key>SuccessfulExit</key><false/></dict>\n<key>ThrottleInterval</key><integer>30</integer>\n<key>StandardErrorPath</key><string>" + escapeXML(filepath.Join(u.StateDir, "tree.log")) + "</string>\n</dict></plist>\n")
		return b.String(), nil
	case "linux":
		args := u.args()
		for i, arg := range args {
			args[i] = strconv.Quote(strings.NewReplacer("%", "%%", "$", "$$").Replace(arg))
		}
		return "[Unit]\nDescription=Gentility shared tree\nAfter=network-online.target\n\n[Service]\nType=simple\nExecStart=" + strings.Join(args, " ") + "\nRestart=on-failure\nRestartSec=5\nNoNewPrivileges=true\n\n[Install]\nWantedBy=default.target\n", nil
	default:
		return "", fmt.Errorf("tree unit: unsupported OS %s", goos)
	}
}

// ActivateTree uses the daemon installer's activation state machine with the
// user's service-manager domain and commands.
func ActivateTree(ctx context.Context, runner Runner, goos string, uid int, path string, changed bool, out io.Writer) error {
	if out == nil {
		out = io.Discard
	}
	var p platform
	if goos == "darwin" {
		domain := "gui/" + strconv.Itoa(uid)
		label := domain + "/" + TreeLabel
		p = platform{loaded: []string{"launchctl", "print", label},
			unload:   [][]string{{"launchctl", "bootout", label}},
			activate: [][]string{{"launchctl", "bootstrap", domain, path}},
			restart:  [][]string{{"launchctl", "kickstart", "-k", label}}}
	} else {
		p = platform{activate: [][]string{{"systemctl", "--user", "daemon-reload"}, {"systemctl", "--user", "enable", "--now", TreeService}},
			restart: [][]string{{"systemctl", "--user", "restart", TreeService}}}
	}
	_, _, err := activateUnit(ctx, runner, p, TreeLabel, changed, true, out)
	return err
}
