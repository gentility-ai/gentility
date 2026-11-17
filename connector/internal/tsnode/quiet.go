package tsnode

import (
	"tailscale.com/envknob"
	"tailscale.com/logtail"
)

// quietLibrary keeps the library's own logs on this machine.
//
// In userspace mode tsnet starts a logtail uploader to log.tailscale.com
// (tsnet.Server.startLogger) unless TS_NO_LOGS_NO_SUPPORT is set, and those
// lines name peers, endpoints and addresses; nothing a 6net node says should
// leave for a third party. The knob makes logpolicy's transport a no-op, and
// Disable is the process-wide kill switch for any path that reaches logtail
// without reading it. Both are idempotent. The node's own Logf still gets
// every line.
func quietLibrary() {
	envknob.SetNoLogsNoSupport()
	logtail.Disable()
}
