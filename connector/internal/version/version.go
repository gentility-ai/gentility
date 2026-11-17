// Package version names the versions this binary speaks.
//
// The Connector's own release string is set at build time with the linker's -X;
// the library tags are constants because they are source-level facts —
// Tailscale is the upstream tag our fork is cut from, which must match the
// version the head fork pins, and Genscale is the fork's own tag, which must
// match the `replace` directive in go.mod. NodeCapver is not Tailscale's at
// all: it is the protocol between this binary and the mothership.
package version

// Connector is the node binary's release string, overridden at link time with
// -X. Unstamped builds use release trust rules. Development tooling must
// explicitly stamp "dev" to enable local development endpoints.
var Connector = "unreleased"

// Commit is the server build's full source SHA; local and Desk builds omit it.
var Commit string

// Tailscale is the upstream library tag this binary's library is cut from.
// It pairs with go.mod's require line and with the tag headscale pins:
// changing one without the others is a lie; changing all is a deliberate move
// of the pinned pair. The replace line is Genscale's.
const Tailscale = "v1.101.0-pre"

// Genscale is the tag of our fork of the library, genscale, that go.mod's
// `replace` compiles in instead of Tailscale's own: Tailscale's tree with
// 6net's range and names (genscale's GENSCALE.md lists every divergence).
// It must equal the replace line's version; the test reads go.mod to hold it
// there.
const Genscale = "v1.101.0-pre.6net.7"

// NodeCapver is the node_capver this binary speaks, sent as X-Node-Capver on
// EVERY node → Gentility request — the exchange included,
// which carries no bearer.
//
// It is the version of everything the two ends say to each other that is not
// Tailscale: enrollment and exchange, the config document, presence. One
// monotonic integer, additive-only within a version; the register of what each
// one means is contract/connector/capvers.md, and the server's capability table is the
// mothership's half of this same number.
const NodeCapver = 1

// String is what `connector version` prints.
func String() string {
	release := Connector
	if Commit != "" {
		release += " (commit " + Commit + ")"
	}
	return "connector " + release + " (tailscale " + Tailscale + ", genscale " + Genscale + ")"
}
