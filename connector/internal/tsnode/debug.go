package tsnode

// NetDebugEnv is the connector's knob for magicsock's [v1] lines: disco
// pings and pongs, endpoint choices, the reasons behind a path change. Off by
// default; the unconditional "now using" lines are enough on an ordinary day.
//
// The knob sets the flag on the live magicsock, so it lasts the process and
// no longer. The library's SetComponentDebugLogging would do the same but
// also writes a deadline into the node's state store, which every later start
// reads back: one run with the knob would leave debug on for good.
const NetDebugEnv = "GENTILITY_NET_DEBUG"

// netDebugWanted reads the knob: exactly "1", nothing looser, so a stray
// GENTILITY_NET_DEBUG=true in a unit file is not a surprise flood.
func netDebugWanted(lookup func(string) (string, bool)) bool {
	value, ok := lookup(NetDebugEnv)
	return ok && value == "1"
}
