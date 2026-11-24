module github.com/gentility-ai/gentility/connector

// Bump Dockerfile's GO_VERSION ARG alongside this line — the node stage builds with GOTOOLCHAIN=local.
go 1.26.4

require (
	github.com/coder/websocket v1.8.12
	// coder/websocket is tailscale's own (DERP over WebSocket) and moves with
	// its pin; phonenode's door dials over it.
	github.com/fsnotify/fsnotify v1.10.1
	golang.org/x/sys v0.49.0
	// gvisor is a DIRECT require because internal/tsnode reaches netstack.Impl's
	// gonet types through it, and it must move with the tailscale pin as one
	// pair — never `go get -u`'d on its own.
	gvisor.dev/gvisor v0.0.0-20260224225140-573d5e7127a8
	tailscale.com v1.101.0-pre
)

require (
	filippo.io/edwards25519 v1.2.0 // indirect
	github.com/akutz/memconn v0.1.0 // indirect
	github.com/alexbrainman/sspi v0.0.0-20231016080023-1a75b4708caa // indirect
	github.com/axiomhq/hyperloglog v0.0.0-20240319100328-84253e514e02 // indirect
	github.com/coreos/go-iptables v0.7.1-0.20240112124308-65c67c9f46e6 // indirect
	github.com/creachadair/msync v0.7.1 // indirect
	github.com/dblohm7/wingoes v0.0.0-20240119213807-a09d6be7affa // indirect
	github.com/dgryski/go-metro v0.0.0-20180109044635-280f6062b5bc // indirect
	github.com/fxamacker/cbor/v2 v2.9.0 // indirect
	github.com/gaissmai/bart v0.26.1 // indirect
	github.com/go-json-experiment/json v0.0.0-20260214004413-d219187c3433 // indirect
	github.com/go-ole/go-ole v1.3.0 // indirect
	github.com/go4org/hashtriemap v0.0.0-20251130024219-545ba229f689 // indirect
	github.com/godbus/dbus/v5 v5.1.1-0.20230522191255-76236955d466 // indirect
	github.com/golang/groupcache v0.0.0-20241129210726-2c02b8208cf8 // indirect
	github.com/google/btree v1.1.3 // indirect
	github.com/google/go-cmp v0.7.0 // indirect
	github.com/google/nftables v0.2.1-0.20240414091927-5e242ec57806 // indirect
	github.com/hdevalence/ed25519consensus v0.2.0 // indirect
	github.com/huin/goupnp v1.3.0 // indirect
	github.com/jsimonetti/rtnetlink v1.4.0 // indirect
	github.com/klauspost/compress v1.18.5 // indirect
	github.com/mdlayher/netlink v1.7.3-0.20250113171957-fbb4dce95f42 // indirect
	github.com/mdlayher/socket v0.5.0 // indirect
	github.com/mitchellh/go-ps v1.0.0 // indirect
	github.com/pires/go-proxyproto v0.8.1 // indirect
	github.com/safchain/ethtool v0.3.0 // indirect
	github.com/tailscale/certstore v0.1.1-0.20260409135935-3638fb84b77d // indirect
	github.com/tailscale/go-winio v0.0.0-20231025203758-c4f33415bf55 // indirect
	github.com/tailscale/hujson v0.0.0-20260302212456-ecc657c15afd // indirect
	github.com/tailscale/netlink v1.1.1-0.20240822203006-4d49adab4de7 // indirect
	github.com/tailscale/peercred v0.0.0-20250107143737-35a0c7bd7edc // indirect
	github.com/tailscale/web-client-prebuilt v0.0.0-20250124233751-d4cd19a26976 // indirect
	github.com/tailscale/wireguard-go v0.0.0-20260527010701-b48af7099cad // indirect
	github.com/vishvananda/netns v0.0.5 // indirect
	github.com/x448/float16 v0.8.4 // indirect
	go4.org/mem v0.0.0-20240501181205-ae6ca9944745 // indirect
	go4.org/netipx v0.0.0-20231129151722-fdeea329fbba // indirect
	golang.org/x/crypto v0.58.0 // indirect
	golang.org/x/exp v0.0.0-20250620022241-b7579e27df2b // indirect
	golang.org/x/mobile v0.0.0-20261011034528-197770444867 // indirect
	golang.org/x/mod v0.42.0 // indirect
	golang.org/x/net v0.61.0 // indirect
	golang.org/x/oauth2 v0.36.0 // indirect
	golang.org/x/sync v0.24.0 // indirect
	golang.org/x/term v0.47.0 // indirect
	golang.org/x/text v0.43.0 // indirect
	golang.org/x/time v0.12.0 // indirect
	golang.org/x/tools v0.52.0 // indirect
	golang.zx2c4.com/wintun v0.0.0-20230126152724-0fa3db229ce2 // indirect
	golang.zx2c4.com/wireguard/windows v0.5.3 // indirect
)

// The pinned pair. genscale carries 6net's range and names (its
// GENSCALE.md lists every divergence); version.Genscale and the parity fixture
// must name this tag.
replace tailscale.com => github.com/gentility-ai/genscale v1.101.0-pre.6net.8

// gomobile binds ./phonenode into the iOS app's xcframework
// (phone/build-phonenode.sh), and refuses a module that does not hold
// golang.org/x/mobile; the tool directives keep `go mod tidy` from pruning it.
tool (
	golang.org/x/mobile/cmd/gobind
	golang.org/x/mobile/cmd/gomobile
)
