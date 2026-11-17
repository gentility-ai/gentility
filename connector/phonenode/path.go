package phonenode

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"time"
)

// PeerPath answers the library's view of its path to one peer address at this
// moment: {"path":"direct","via":"ip:port"}, {"path":"derp","relay":"<region>"},
// {"path":"peer_relay"}, {"path":"idle"}, or {"path":"unknown"} for an address
// the netmap does not hold. The app logs it when a door opens and again when
// the door closes if it changed (the spec's B1): it is the one fact that says
// whether a LAN peer was reached over Wi-Fi, a VM bridge or a router's
// hairpin. The address is this phone's own network, and fine to log.
//
// It reads the same report Status does, bounded by the same five seconds, and
// answers status_unknown when the node cannot say.
func PeerPath(address string) string {
	addr, err := netip.ParseAddr(address)
	if err != nil || !addr.Is6() || addr.Is4In6() || addr.Zone() != "" {
		return failure(codeMalformed, fmt.Errorf("%q is not a 6net address", address))
	}
	mu.Lock()
	s, isJoining := current, joining != nil
	mu.Unlock()
	switch {
	case s != nil:
	case isJoining:
		return failure(codeJoining, errors.New("the node is still joining: ask once Status says up"))
	default:
		return failure(codeNodeDown, errors.New("no node is up"))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	seen, err := s.report(ctx)
	if err != nil {
		return failure(codeStatusUnknown, err)
	}
	want := addr.String()
	for _, p := range seen.PeerPaths {
		if p.Address != want {
			continue
		}
		out := map[string]any{"path": p.Path}
		if p.Via != "" {
			out["via"] = p.Via
		}
		if p.Relay != "" {
			out["relay"] = p.Relay
		}
		return answer(out)
	}
	return answer(map[string]any{"path": "unknown"})
}
