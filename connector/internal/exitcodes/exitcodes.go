// Package exitcodes is the connector's exit vocabulary: what a supervisor
// reads when the process is gone.
//
// A node is started by something that cannot read a log line — launchd, systemd
// or the Builder — so the exit code is the whole of the message. The split that
// matters is retryable against not: Temporary says "start me again", and
// everything between EnrollRefused and CapverTooOld says "do not, and here is
// why", because a supervisor restarting into a refusal is a loop nobody watches.
package exitcodes

import "errors"

const (
	// OK is a command that did what it was asked.
	OK = 0

	// Failure is anything unclassified. A supervisor may retry it; nothing
	// promises that helps.
	Failure = 1

	// Usage is a missing or malformed environment variable or flag. It is the
	// operator's, and it will still be wrong on the next start.
	Usage = 2

	// EnrollRefused is the mothership refusing the join: the nonce was wrong,
	// the enrollment was never bound, the node was revoked, or another node
	// presented this material first. Do NOT retry — enrol again.
	EnrollRefused = 3

	// MaterialExpired is an exchange retried past the one-hour window the
	// enrollment material lives for. The material is gone
	// and there is nothing left to be idempotent with; enrol again.
	MaterialExpired = 4

	// CapverTooOld is this binary speaking a node_capver below the
	// mothership's floor. Only a new binary fixes it.
	CapverTooOld = 5

	// Temporary is retryable: the supervisor should restart us. 75 is
	// sysexits' EX_TEMPFAIL, which is what systemd's and launchd's own
	// documentation reaches for.
	Temporary = 75

	// Restart means "I updated myself, start the new binary".
	// It is named here so nothing else takes 70.
	Restart = 70
)

// Fatal marks an error the caller must not retry, and names the exit code it
// calls for.
//
// It lives here rather than in whichever package raised it because the
// question it answers — "does this process stop, and with what code?" — is one
// question with one answer, and every layer that could invent a second
// vocabulary for it is a layer where a refusal turns into a restart loop.
type Fatal struct {
	Code int
	Err  error
}

func (e *Fatal) Error() string { return e.Err.Error() }
func (e *Fatal) Unwrap() error { return e.Err }

// Wrap marks err fatal with the exit code it calls for.
func Wrap(code int, err error) error { return &Fatal{Code: code, Err: err} }

// Of reports the exit code err calls for, and whether it is fatal at all.
//
// A retryable error has no exit code of its own: the caller either keeps
// trying or eventually gives up with Temporary, and which of those it does is
// the caller's decision and not the error's.
func Of(err error) (int, bool) {
	var fatal *Fatal
	if errors.As(err, &fatal) {
		return fatal.Code, true
	}
	return 0, false
}
