# The Connector

One Go binary, `connector`, built from `cmd/connector`. On a laptop it is how a
person adds and closes organization tasks without a browser; on a server it is
the node that joins the org overlay.

Every task command is a single MCP `tools/call` against the same four tools the
web UI uses (`task_create`, `task_list`, `task_check`, `task_close`), so a task
created here is indistinguishable from one created in the app.

For a remote machine that will also run GenDesk coding sessions, provision
the person's Git identity and commit-signing key. Overlay enrollment alone does
not configure Git signing.

## Install the `gentility` alias

`connector` is what a server installs; `gentility` is what a person types. They
are the same file — the binary reads the name it was invoked by — so the alias
is a symlink, not a second build:

```
sudo ln -sf /usr/local/bin/connector /usr/local/bin/gentility
```

A person's machine needs none of this: `gentility net join` copies the binary
it was run from to `/usr/local/bin/gentility`, root-owned, so after the first
join `gentility` is on the PATH (see below).

If `gentility login` says `Unknown command: login`, another program named
`gentility` may come first on your PATH. Use `/usr/local/bin/gentility login`
(or the full path to your connector) now, then put the connector's directory
earlier on PATH or remove the other program. The connector warns with both
resolved paths on each `login`, `net join` and `tree install` while shadowed.
The installed connector at `/usr/local/bin/gentility` is recognized even when
running a downloaded or built copy. Checks skip elevated processes, including
`install --place-binary` under sudo or Desk, because the person's original PATH
is unavailable. CLI `net join` checks the person's PATH after placement.

You can make the same link beside a locally built binary. Everything the command
prints about itself uses the name it was called by; the examples inside error
messages name `connector`, which is the file both names point at.

## Sign in

```
gentility login                                  # https://mcp.gentility.ai
gentility login --url "$CONNECTOR_TEST_URL"      # an explicitly configured test server
```

`login` completes the device flow: it prints a URL and a code, you approve it in
a browser, and the bearer it mints lands in the state dir. Tokens last 30 days.
`gentility logout` forgets the token here — there is no revoke endpoint, so it
stays valid on that host until it expires.

The host is resolved in this order: `--url`, then `$GENTILITY_MCP_URL`, then the
host you last signed in to, then `https://mcp.gentility.ai`. `login` says which
one it used. A bearer belongs to the origin that minted it, so switching hosts
is a fresh `gentility login --url <host>`; a token stored for one host is never
sent to another.

## Tasks

```
gentility task add "Rotate the WorkOS key" --kind ops --org acme
gentility task list --mine --status open --limit 20
gentility task list --json | jq '.tasks[0]'
gentility task check <task-id> 2 --cause "ran it"
gentility task close <task-id> --cause "shipped"
gentility task close <task-id> --dismiss --cause "not ours"
```

Every call names its organization. `--org` is the argument the server requires
on every one of these tools — it is never a server-side default, because a
default is an authorization snapshot and a removed member should lose access at
once. The connector remembers the last organization you used and sends it as
that argument next time; `--org` overrides it, and a fresh state dir refuses
until you pass one.

Each command answers `--help` with its own flags.

## State

`$GENTILITY_STATE_DIR`, else the per-OS user config directory
(`~/Library/Application Support/gentility` on macOS, `$XDG_CONFIG_HOME` or
`~/.config/gentility` on Linux, `%AppData%\gentility` on Windows). The directory
is `0711` (traversable, not listable) and `auth.json` is `0600`; `prefs.json`
beside it holds the remembered organization. Deleting the directory is a full
sign-out. One rule covers every state directory, the person's and the
daemon's: `status.json` is `0644`, because it holds no secret, and every other
file is `0600`. On a shared host that means any local account can read a
daemon's org slugs, overlay addresses and public endpoint; that is deliberate.

## When status says no heartbeat is taken

```
presence   no heartbeat taken for 5h31m0s; other machines may not reach this one
```

The daemon sends the mothership a heartbeat every thirty seconds, over 6net.
This line appears once none has been taken for ninety seconds, and says for
how long. The daemon is running and still receiving, which is why the rest of
the block looks well; what it sends is not arriving, so its peers may not
reach it either. `--json` carries the same as `unheard_seconds`, and
`status.json` as `unheard_since`.

## Check the daemon's network observations

```
gentility net check
gentility net check --json
sudo gentility net check --endpoints
```

The check reads the running daemon's saved observations; it does not open a
second network socket or trigger a new probe. It shows when the daemon last
completed its UDP probe and the machine's own address observed by STUN. A
successful round trip proves UDP worked at that time. No round trip alone does
not prove a firewall blocked UDP. An idle daemon can retain an old probe.

The self summary is public in `status.json`. Peer endpoints are private in
`net-check.json` (0600), and appear only with `--endpoints`. This first version
requires root or sudo for endpoint detail on both Linux and macOS; the Mac
installer does not record a trusted owner UID for password-free access.
Advertised endpoints are candidates from the daemon's current network map,
not proof of the current path or the full set learned through peer discovery.
A missing map or peer says unavailable; a present peer with no candidates says
none known. The current path is shown separately, with its relay region when
applicable.

Each snapshot and probe has its own timestamp. A daemon snapshot at least 90
seconds old, or with an invalid timestamp, exits with status 1: check that the
daemon is running and the machine's clock is correct. A missing probe is
unavailable, including with an older daemon, and also exits 1. An invalid probe
timestamp exits 1. An old probe alone does not fail the command: probes pause
while idle or without peers. The output preserves the last probe's age and
explains that the daemon probes again when it has traffic. Send traffic to a
peer and rerun the command for a newer observation; this command does not
refresh the probe itself. A negative UDP observation also exits successfully
because the observation itself is valid. The --state-dir flag selects a
particular daemon's state directory.

## A person's machine on the overlay

Interface mode puts the overlay on a real network interface (a `utun` on
macOS, `gentility0` on Linux), so every program on the machine reaches the
overlay and resolves overlay names. It needs root, and it runs as the
installed daemon rather than in a terminal. On Linux the whole flow is three
lines:

```
curl -fsSL -o gentility https://core.gentility.ai/downloads/connector/linux/amd64 && chmod +x gentility
./gentility login        # approve "This is my device"; the login lasts 30 days
./gentility net join     # asks sudo once; prints this machine's 6net name and address
```

`login` only signs you in. `net join` runs as you, not under sudo
(`sudo gentility net join` is refused and told the right command): it fetches
this machine's join material from the server with your login, at the moment
it joins, because the single-use key in it dies ten minutes after it is
minted. It writes the material into a fresh `0700` directory under your state
directory and runs `sudo -- <this binary> install --interface
--source-state-dir <that directory> --place-binary /usr/local/bin/gentility`,
the one privileged step. Your login never leaves your process: root reads the
material and nothing else, and the install removes the material from that
directory once it has staged it; `net join` removes the directory on every
exit. The machine joins as `<label>.<you>`, the label being the hostname's
first label unless `--label` names another. It then waits up to a minute for
the daemon to say it is live, and prints its name, its address in each
organisation and the interface:

```
on 6net as cedar.morgan.northwind.6net.io  fd06:6e7:0:2::5 (northwind)  interface gentility0
```

What it installs, and where:

- `/usr/local/bin/gentility`, a root-owned copy of the binary, which the daemon
  runs and which puts `gentility` on the PATH;
- `/etc/systemd/system/gentility-connector.service` (a LaunchDaemon on macOS),
  the daemon's unit;
- `/var/lib/gentility`, the daemon's state: its identity, `0600`, and
  `status.json`;
- `/var/log/gentility`, its logs.

`gentility net status` says whether the machine is on 6net and as what;
`gentility net leave` takes it off (it asks sudo). `net join` refuses over an
installed daemon, so a second join is `net leave` first. GenDesk's **Join
6net** button on a Mac runs the same install, and the two share the one
daemon.

A connection 6net refuses is reported: the refusing machine's daemon posts it to the mothership once a minute, and the person reviews it on the web’s Network page. A refused connection may still time out on the sender; its daemon logs `refused by <peer>'s rules: <address>:<port>`.

A server joins as a machine, not as a person, and keeps its own flow:
`connector login --server` stages a machine enrollment in the state
directory, and `sudo connector install` carries it across and starts the
daemon in userspace, with no interface. An unattended install stages what the
environment carries instead (`sudo -E connector install`).

On Linux the interface daemon's unit has no `User=` and `ProtectSystem=true`,
so `/etc/resolv.conf` stays writable. An install
that changes the unit applies it at once: a restart on Linux, a bootout then a
bootstrap on macOS. An unchanged reinstall drops nothing. Interface mode
runs the engine's own DNS cleanup before it starts, and
`gentility cleanup --interface` runs the same cleanup after it stops: systemd
runs it from the unit's `ExecStopPost`, and `leave` runs it on both
platforms. Neither touches the firewall: interface mode adds no netfilter
rules. A daemon that crashed on macOS keeps its resolver files until the
daemon starts again or `sudo gentility cleanup --interface` is run.
Tailscale's log directory is kept inside the state directory and no logs are
uploaded.

Interface mode runs beside an installed Tailscale. 6net has its own range,
`fd06:6e7::/48`, apart from the one Tailscale holds, and the library is our
fork, genscale, whose `GENSCALE.md` lists every name it changes: on Linux
policy-routing table 66 behind rules 5110-5170 (Tailscale's are table 52 and
5210-5270), and resolver files and markers of its own on both platforms.
Consult that fork's `GENSCALE.md` when checking coexistence with Tailscale.

`gentility status` reads `--state-dir` when given; otherwise the daemon's
directory when the daemon is installed, else this user's, and its first line
says which (`daemon` or `this user`). Without sudo it cannot read the
daemon's `0600` identity files, so it reports from `status.json` alone and
says "identity details need sudo"; with no `status.json` it can read, it says
"the daemon's status could not be read" rather than claiming the node joined.

The path summary counts observed peer addresses, followed by each address's
path: direct, relay (with its region), peer relay, idle, or unknown. A machine
can have a different path to each peer; its home relay alone says nothing about
those paths. Older daemons without peer observations say "peer paths unavailable"
until updated. Stale and incomplete observations are marked. The public status
file includes these paths and relay regions, never peer endpoint addresses.

`gentility net leave` (`sudo gentility leave`, which it runs) stops the
daemon and removes its unit, cleans up after
an interface daemon, then forgets the identity and any staged enrollment. If
the supervisor refuses the stop, leave keeps the identity, prints what the
supervisor said and exits 1; once the daemon is stopped, a second leave
finishes. A second run finishes a leave that
stopped halfway, and on a machine with nothing to leave it says so and
changes nothing. The organisation still lists the node until an admin revokes
it.

### Reaching your own machines

`gentility net serve ssh` opens this machine's ssh to your other machines on
6net, in every organisation it has joined, and to nobody else:

```
cedar$ ./gentility net serve ssh      # [--port 22]
ssh on cedar.morgan.northwind.6net.io:22 — approved; your other machines can reach it
mac$   ssh cedar.morgan.northwind.6net.io
```

It runs as you, not under sudo, and changes nothing on the machine: it tells
the server, with your login, that this machine serves ssh, and the server
opens a path to it from each of your other machines in that organisation.
sshd already listens on the overlay interface, so nothing restarts. An admin's
declaration is approved at once; a member's waits for an admin, and the line
says so and names the service to approve. `gentility net unserve ssh`
withdraws it, and every path to it with it; it withdraws ssh and nothing else
that port carries.

The path runs one way. Your other machines reach this one's ssh, can ping it
and resolve its name, and this one resolves theirs, but it cannot ping or
reach them unless they serve something too. A port this machine has not
declared stays closed: a connection to it times out rather than being
refused, because the filter drops it and IPv6 gets no reset. A machine you
have left with `gentility leave`, or lost, keeps reaching your ssh until an
admin revokes it, so have a lost machine revoked.

## A host that forwards one service

```
connector serve --forward vmrunner=/run/vmrunner/door.sock
```

A host that runs loop computers serves one thing on the overlay: its runner,
which listens on a unix socket and nowhere else. `--forward` carries that
service to the socket. It forwards a kind only if this binary lists it
(`vmrunner`, today) and the server has declared it for this node and marked it
`forwardable` in the config document, which no heartbeat can do. The forward is
bound only while the current document carries exactly one approved, marked
entry. Otherwise nothing is bound, every forwarded connection is closed, and
the node beats offering nothing and waits; its log says why. So a host's unit
may start before the server has declared the service. Below the server's capver
floor, an unchanged binding is kept and nothing new is bound.

With `--forward` the heartbeat offers nothing and no egress relay runs, and
`--interface` is refused: the node stays in userspace. It listens on its own
address in the service's organization, lets in only the platform gateway the
current document names, and copies bytes both ways without reading them. If
the socket does not answer, the connection is closed, with no retry.

## Building and testing

Contract readers expect the single published `contract/` directory beside
this Go module. Run `../contract/tools/verify` from the module before testing.
For a local experiment, set `GENTILITY_CONTRACT_DIR` to a separately built
artifact. CI refuses that override and reads the sibling copy. No reader
falls back to the server's source files.

```
cd node && gofmt -l . && go vet ./... && go test ./...
go build -o connector ./cmd/connector
```

The tests in `internal/tasks` run against `contract/connector/tool_schemas.json`,
the published schemas the four tools advertise, so they need no server.
The server's schema-drift test fails when a tool's schema changes without that
file being regenerated. To check the fixture against a
running host:

```
cd node && go test -tags livemcp ./internal/tasks/ \
    -url "$CONNECTOR_TEST_URL" \
    -token "$(jq -r .access_token "${GENTILITY_STATE_DIR:-$HOME/.config/gentility}/auth.json")"
```

Both flags empty skips those tests rather than failing them.

The client-owned files are generated by `go run ./cmd/clientcontract --out DIR`
into a new directory. They have their own manifest, separate from the server's
manifest. The contract verifier accepts both manifests together and refuses
overlapping ownership or modified files.

## Multi-org transport regression

`go test ./internal/tsnode -run TestMultiOrgConnections -count=1` starts three
real userspace nodes, a local test control server, and a local DERP server.
One node holds two org addresses simultaneously; the other two hold one each.
The test checks connections in both directions, the source address observed
by each receiver, and packet-filter rejection when the dual-org node uses the
wrong org's source address. It runs in the ordinary `go test ./...` CI suite,
without credentials, root, or a deployed server.

The control plane supplies fixed addresses and filters; this covers the
connector's real netstack and transport, not Gentility enrollment, the head's
renderer, kernel interface mode, or NAT traversal between physical hosts.

## Windows

Best effort: the binary builds and the commands work, the state dir follows
`%AppData%`, and `connector.exe` reads its own name so a copy named
`gentility.exe` behaves as the alias does elsewhere. It is not part of the
supported release matrix and is not tested on CI.

### Shared tree

After `connector login` and `connector net join`, run `connector tree install`
as yourself to keep `~/.gentility/tree` synchronized. macOS uses a LaunchAgent;
Linux uses a user systemd unit. `connector tree run` runs it in the foreground,
and `connector tree uninstall` stops and removes the unit while retaining the
cache and sync state. User commands refuse sudo and direct root login.

The system connector publishes public `discovery.json` next to its status file.
The user service checks its ownership and permissions, pins TLS to its CA, and
binds connections to its 6net interface. No gateway address or certificate needs
to be supplied by hand. An unsafe discovery file is refused; reinstall the
system connector to restore trusted ownership and permissions.

A machine-owned connector also keeps the organization's read-only tree under its
system state directory's `tree/`. It uses its existing overlay connection and
node credential; personal paths are excluded. A user-owned node's daemon tree
request is forbidden by the server, while its person's user service can run.

Sync checks every 12–18 seconds, uses ETags, verifies every downloaded SHA-256,
and replaces files atomically. The user service watches writable directories and
coalesces saves for 500 ms before uploading. Only explicit manifest capabilities
permit creates or edits; older servers advertising a bare `true` remain read-only.
Editable files have mode 0600; other user files stay 0400.

Create or edit notes and skills in directories that permit it. To create a board
post, save a Markdown file with a `# Title`, an optional `refs: {JSON}` header
block separated by blank lines, and the body. The accepted post moves to the
server's canonical filename. Existing posts, tasks and INDEX files cannot be
edited here. Deletes and renames are restored; archive and move through the tools.

A refused edit is preserved beside the server copy as `*.rejected.md`, with a
short reason header. Unmanaged collisions and ambiguous rename candidates are
retained as `*.conflict.md`. These recovery copies and `.sync-status` are never
uploaded or removed by reconciliation. Root `.sync-status` records the principal,
last success, last error, separate ledger cursors and recovery paths.

Pending writes live durably in the private `tree-sync/writes/` journal. Transient
failures back off and retry. Post creates carry a persistent `refs.tree_write`
key: an ambiguous response waits two minutes before checking recent board posts
and retrying with the same key. An incomplete recent-post window or ambiguity
lasting 24 hours produces a rejected copy instead. This is at-least-once delivery
with deduplication: a server commit arriving after the grace period can still
produce a duplicate. Keep the private journal when recovering or upgrading.

HTTP 401/403 stops synchronization and preserves pending edits as rejected copies;
after correcting login or access, rerun `tree install`. User-service errors also
go to its launchd log or systemd journal.

Each cache binds to login origin, organization slug and OS uid. A bearer refresh
continues normally. To deliberately change origin or organization, uninstall,
move aside both `~/.gentility/tree` and the login state directory's `tree-sync/`,
and install again. A different person logging in under the same OS account
reconciles the existing managed inventory against their own authorized manifest.

Untrusted paths, symlinks, corrupt state, and hash mismatches stop the syncer
without accepting the offending content. Downloads are bounded to 16 MiB per
blob and manifest. The daemon buffers at most 256 MiB of distinct verified
content per reconciliation; user mode materializes one verified blob at a time.
Keep these limits in mind for unusually large organizations.
