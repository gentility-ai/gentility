package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPathShadowWarning(t *testing.T) {
	asUID(t, 501)
	root := t.TempDir()
	connectorDir := filepath.Join(root, "connector's directory")
	otherDir := filepath.Join(root, "other")
	linkDir := filepath.Join(root, "link")
	for _, dir := range []string{connectorDir, otherDir, linkDir} {
		if err := os.Mkdir(dir, 0755); err != nil {
			t.Fatal(err)
		}
	}
	self := filepath.Join(connectorDir, "gentility")
	other := filepath.Join(otherDir, "gentility")
	marker := filepath.Join(root, "executed")
	for _, path := range []string{self, other} {
		if err := os.WriteFile(path, []byte("#!/bin/sh\ntouch '"+marker+"'\n"), 0755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(self, filepath.Join(linkDir, "gentility")); err != nil {
		t.Fatal(err)
	}
	// macOS temp paths themselves may contain symlinks.
	resolvedSelf, err := filepath.EvalSymlinks(self)
	if err != nil {
		t.Fatal(err)
	}
	resolvedOther, err := filepath.EvalSymlinks(other)
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(root)
	for _, tc := range []struct {
		name, path string
		warn       bool
	}{
		{"shadowed", otherDir + ":" + connectorDir, true},
		{"connector first", connectorDir + ":" + otherDir, false},
		{"missing", root, false},
		{"empty PATH", "", false},
		{"symlink to connector", linkDir + ":" + otherDir, false},
		{"relative shadow", "other:" + connectorDir, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("PATH", tc.path)
			var out bytes.Buffer
			warnPathShadow(&out, self, filepath.Join(root, "installed", "gentility"))
			if !tc.warn {
				if out.Len() != 0 {
					t.Fatalf("unexpected warning: %s", out.String())
				}
				return
			}
			for _, want := range []string{resolvedOther, resolvedSelf, "Use '" + strings.ReplaceAll(resolvedSelf, "'", "'\"'\"'") + "' now", "first on PATH", "remove the other program"} {
				if !strings.Contains(out.String(), want) {
					t.Errorf("warning %q missing %q", out.String(), want)
				}
			}
			if strings.Count(out.String(), "\n") != 2 {
				t.Fatalf("expected two lines: %q", out.String())
			}
		})
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("PATH candidate was executed: %v", err)
	}
}

func TestPathWarningSkipsElevatedPath(t *testing.T) {
	asUID(t, 0)
	for _, sudoUser := range []string{"person", ""} {
		t.Run("SUDO_USER="+sudoUser, func(t *testing.T) {
			t.Setenv("SUDO_USER", sudoUser)
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "gentility"), []byte("other program"), 0755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", dir)
			var out bytes.Buffer
			warnCurrentPath(&out)
			if out.Len() != 0 {
				t.Fatalf("unexpected elevated warning: %s", out.String())
			}
		})
	}
}

func TestPathWarningRecognizesPlacedCopy(t *testing.T) {
	asUID(t, 501)
	dir := t.TempDir()
	placed := filepath.Join(dir, "gentility")
	if err := os.WriteFile(placed, []byte("installed connector"), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	self, err := thisBinary()
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	warnPathShadow(&out, self, placed)
	if out.Len() != 0 {
		t.Fatalf("recommended removing the installed connector: %s", out.String())
	}
}

func TestPersonCommandsWarnWithoutChangingFailure(t *testing.T) {
	asUID(t, 501)
	t.Setenv("SUDO_USER", "")
	swap(t, &unitInstalled, func() bool { return true })
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "gentility"), []byte("other program"), 0755); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"login", "--url", "://invalid"},
		{"net", "join"},
		{"tree", "install", "--discovery-dir", filepath.Join(dir, "missing"), "--state-dir", filepath.Join(dir, "state")},
	} {
		t.Run(strings.Join(args[:2], " "), func(t *testing.T) {
			var baseline, stdout, stderr bytes.Buffer
			t.Setenv("PATH", "")
			wantCode := run(args, &stdout, &baseline)
			t.Setenv("PATH", dir)
			code := run(args, &stdout, &stderr)
			if code != wantCode || code == 0 {
				t.Fatalf("exit = %d, baseline = %d; expected same failure", code, wantCode)
			}
			if strings.Count(stderr.String(), "Warning: gentility on PATH") != 1 {
				t.Fatalf("expected one warning: %s", stderr.String())
			}
		})
	}
}
