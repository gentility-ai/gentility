package main

import (
	"fmt"
	"io"
	"strings"

	"github.com/gentility-ai/gentility/connector/internal/terminal"
)

// Diagnostics are one terminal line. Sanitize the completed message so even
// errors returned by a library cannot insert terminal commands or new lines.
func diagnosticf(w io.Writer, format string, args ...any) {
	message := strings.TrimSuffix(fmt.Sprintf(format, args...), "\n")
	fmt.Fprintln(w, terminal.Text(message))
}

func diagnosticln(w io.Writer, args ...any) {
	diagnosticf(w, "%s", fmt.Sprintln(args...))
}
