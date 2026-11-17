package tsnode

import (
	"slices"
	"time"

	"tailscale.com/net/netcheck"
	"tailscale.com/types/netmap"
)

// UDPProbe describes the daemon's own most recent STUN check. ObservedAt is
// the probe's time, not the time a reader happened to ask for it. No peer
// endpoints belong here: this summary may be written to public status.
type UDPProbe struct {
	ObservedAt time.Time `json:"observed_at"`
	UDP        bool      `json:"udp"`
	IPv4       bool      `json:"ipv4"`
	IPv6       bool      `json:"ipv6"`
	GlobalV4   string    `json:"global_v4,omitempty"`
	GlobalV6   string    `json:"global_v6,omitempty"`
}

func udpProbe(report *netcheck.Report) *UDPProbe {
	if report == nil {
		return nil
	}
	probe := &UDPProbe{ObservedAt: report.Now, UDP: report.UDP, IPv4: report.IPv4, IPv6: report.IPv6}
	if report.GlobalV4.IsValid() {
		probe.GlobalV4 = report.GlobalV4.String()
	}
	if report.GlobalV6.IsValid() {
		probe.GlobalV6 = report.GlobalV6.String()
	}
	return probe
}

// PeerDiagnostic is private: both the current path and the candidate endpoints
// are observations of another machine's network, not public status fields.
type PeerDiagnostic struct {
	PeerPath
	Endpoints            []string `json:"endpoints"`
	EndpointsUnavailable bool     `json:"endpoints_unavailable,omitempty"`
	EndpointsTruncated   bool     `json:"endpoints_truncated,omitempty"`
}

func peerDiagnostics(nm *netmap.NetworkMap, paths []PeerPath) []PeerDiagnostic {
	// These are candidates advertised through the head, not the additional
	// endpoints learned by magicsock discovery. PeerStatus.Addrs is self-only.
	byAddress := make(map[string][]string)
	if nm != nil {
		for _, peer := range nm.Peers {
			endpoints := make([]string, 0, peer.Endpoints().Len())
			for _, endpoint := range peer.Endpoints().All() {
				endpoints = append(endpoints, endpoint.String())
			}
			for _, prefix := range peer.Addresses().All() {
				if prefix.IsSingleIP() {
					byAddress[prefix.Addr().String()] = endpoints
				}
			}
		}
	}
	result := make([]PeerDiagnostic, 0, len(paths))
	for _, path := range paths {
		candidates, available := byAddress[path.Address]
		endpoints := slices.Clone(candidates)
		slices.Sort(endpoints)
		endpoints = slices.Compact(endpoints)
		truncated := len(endpoints) > 32
		if truncated {
			endpoints = endpoints[:32]
		}
		result = append(result, PeerDiagnostic{PeerPath: path, Endpoints: endpoints,
			EndpointsTruncated: truncated, EndpointsUnavailable: !available})
	}
	return result
}
