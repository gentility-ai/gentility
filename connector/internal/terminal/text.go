// Package terminal makes untrusted text safe to include in terminal output.
package terminal

import "strings"

// Text replaces terminal controls, including embedded line breaks. Callers
// supply their own layout after sanitizing fields; decoded data stays intact.
func Text(value string) string {
	return strings.Map(func(r rune) rune {
		if r < 32 || r >= 127 && r <= 159 {
			return '\uFFFD'
		}
		return r
	}, value)
}
