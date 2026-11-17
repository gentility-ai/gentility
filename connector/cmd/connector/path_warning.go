package main

import (
	"errors"
	"io"
	"os/exec"
	"path/filepath"
	"strings"
)

func warnCurrentPath(stderr io.Writer) {
	self, err := thisBinary()
	if err == nil {
		warnPathShadow(stderr, self, placedBinary)
	}
}

// This is advisory only: never execute a PATH candidate, or guess the person's
// PATH from an elevated process (sudo and Desk both replace it). Even a root
// shell has no reliable evidence of a person's PATH, so root stays quiet.
func warnPathShadow(stderr io.Writer, connector, installedConnector string) {
	if geteuid() == 0 {
		return
	}
	candidate, err := exec.LookPath("gentility")
	// A relative PATH entry still shadows the connector in the person's shell.
	if err != nil && !errors.Is(err, exec.ErrDot) {
		return
	}
	first, err := filepath.Abs(candidate)
	if err != nil {
		return
	}
	first, err = filepath.EvalSymlinks(first)
	if err != nil {
		return
	}
	self, err := filepath.Abs(connector)
	if err != nil {
		return
	}
	self, err = filepath.EvalSymlinks(self)
	if err != nil || first == self {
		return
	}
	// Placement makes a copy, not a symlink to the downloaded/build binary.
	// Never recommend removing the connector the daemon was installed to run.
	installed, err := filepath.EvalSymlinks(installedConnector)
	if err == nil && first == installed {
		return
	}
	diagnosticf(stderr, "Warning: gentility on PATH resolves to %q; this connector is %q.\n", first, self)
	// Single quotes keep spaces and shell metacharacters literal when copied.
	diagnosticf(stderr, "Use '%s' now; put its directory first on PATH or remove the other program.\n", strings.ReplaceAll(self, "'", "'\"'\"'"))
}
