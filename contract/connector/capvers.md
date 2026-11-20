# Capability Versions

Two independent monotonically increasing capability versions ("capvers") live
in this repository, both modelled on Tailscale's `CapabilityVersion`. They are
different numbers for different pairs of programs and never move together. A
third register, Desk's protocol, is a `{major, minor}` pair rather than a
capver and is listed here so the three are found in one place:

| Register | Between | Source of truth | Current | Floor |
|---|---|---|---|---|
| **agent capver** | the Crystal agent and the Phoenix app, over `wss /agent` | `Gentility.Connector.Protocol` (`lib/gentility/connector/protocol.ex`) | 5 | 2 |
| **`node_capver`** | the Go Connector and the mothership, over the overlay | `Gentility.Overlay.Capver` (`lib/gentility/overlay/capver.ex`) | 1 | 1 |
| **Desk protocol** | the Desk window, `desk call` and `desk mcp`, and the Desk core, over the control socket and a pairing | `desk_core::api::PROTOCOL` (`desk/core/src/api.rs`), snapshot `desk/core/protocol/released.json` | 1.1 | 1 |

Desk's rules are its own (`desk/README.md`, "What the app adds to the core"):
within a major every change is additive and a client reads `accepted` for the
words it asked for; a major mismatch is refused by the client before anything
is published; a snapshot test fails the build when a declaration disappears
without the major going up.

When you add or change either capver, update this file in the same change.

---

# Agent protocol capability versions

The agent protocol uses a monotonically increasing capability version
("capver"), inspired by Tailscale's `CapabilityVersion`. The agent declares
its capver in connect params; the server uses it to gate behaviour and to
decide which wire-format strings to emit for capver-aware events.

This document is the **shared reference** for the server (Elixir) and the
agent (Crystal). When you add or change a capver, update this file in the
same change.

> **Current capver:** **5**
> **Server source of truth:** `Gentility.Connector.Protocol` (`lib/gentility/connector/protocol.ex`)
> **Historical AsyncAPI source:** `agent-protocol.asyncapi.yaml` in the server
> repository; not bundled in this contract. The history below describes the
> legacy Crystal protocol, not an executable Go Connector reader contract.

---

## How capver works

- The agent connects with `?version=N&agent_version=...` in its socket
  params. `version` is the capver integer; `agent_version` is the agent's
  software release string (independent of capver).
- The server stores the declared capver on the connection state and uses it
  to look up wire-format strings via `Protocol.event_name/2` and to gate
  any capver-conditional behaviour.
- **The server does not reject unknown (higher) capvers.** A future agent
  declaring a capver above the server's `@current` connects fine; any
  fields the server doesn't understand are simply ignored. This means
  *agent rollouts and server rollouts can happen independently*.
- Lower capvers are supported as long as their clauses remain in
  `event_name/2` / `parse_type/1`. Capver 1 has been removed (see
  history below); the floor is currently capver 2.

## How to add a new capver

1. Bump `@current` in `lib/gentility/connector/protocol.ex`.
2. Add gated clauses to `event_name/2` and/or `parse_type/1` for any new
   wire-format strings.
3. Update the server repository's `agent-protocol.asyncapi.yaml`
   with the new message schemas, and/or write a per-version design doc in
   this directory (see `2026-04-12-agent-egress-protocol-v3-design.md` for
   the format).
4. **Add a new section to this file** describing what the capver adds.
5. Crystal agent ships matching support and starts sending the new capver
   in its connect params once it's ready.

New features that only exist in the latest capver need only one clause in
`event_name/2` (no fallback). Old agents continue to work because their
declared capver doesn't match the new clause.

---

## Capver history

### Capver 1 — removed

Flat, ad-hoc event names: `welcome`, `ping`, `pong`. Removed in
[55eefb80 / 5e9546b1] (the `refactor(agents): drop capver 1 support` line).
No agent in the wild speaks this anymore.

### Capver 2 — hierarchical event names

The first version of the dot-separated event-name scheme that the rest of
the protocol is built on. Defines the core message types:

- **Lifecycle:** `session.created`, `session.provisioning.created`,
  `session.provisioning.incomplete`, `session.provisioned`,
  `session.provision`
- **RPC:** `command.request`, `command.response`, `command.error`
- **Liveness:** `heartbeat.ping`, `heartbeat.pong`
- **Status:** `agent.status`, `agent.credentials`,
  `agent.security.lockout`
- **Sync:** `sync.updated`
- **Errors:** `error`

Full message shapes and historical rationale remain in the server
repository's `agent-protocol.asyncapi.yaml` and
`2026-03-21-agent-protocol-v2-design.md`; neither is bundled here.

### Capver 3 — egress streaming

Adds the egress tunnel transport. The agent gains the ability to open
outbound TCP streams on the server's behalf and shuttle data back through
the connection.

**Server → Agent**
- `egress.stream.open`
- `egress.stream.data`
- `egress.stream.close`

**Agent → Server**
- `egress.stream.opened`
- `egress.stream.data`
- `egress.stream.close`
- `egress.stream.error`

Historical schemas and rationale remain in the server repository's
`agent-egress-protocol-v3.asyncapi.yaml` and
`2026-04-12-agent-egress-protocol-v3-design.md`; neither is bundled here.

This is the transport layer only — capver 3 says *"this agent can carry
egress traffic if the server tells it to"*, not *"this agent should be
used as an egress route"*. The decision-and-discovery layer is capver 4.

### Capver 4 — egress advertisement + wire cleanup

Capver 4 makes three changes. Two of them are pure cleanup that capver-3
agents will continue to work through; one is a new feature on
`agent.status`.

#### (a) Egress capability advertisement on `agent.status`

Adds an `egress` field to the inner `status` map that the agent sends on
connect and on subsequent status pushes:

```json
{
  "type": "agent.status",
  "status": {
    "egress": { "enabled": true },
    "...": "..."
  }
}
```

| Field            | Type    | Meaning                                                            |
| ---------------- | ------- | ------------------------------------------------------------------ |
| `egress.enabled` | boolean | The operator has opted this agent in to acting as an egress route. |

**Semantics.**

- Absent field, or `enabled: false`, means the agent should not be
  considered as an egress option. Workflows that bind to a route on
  this machine will hard-fail at compile time (`egress_not_found`).
- On any sighting of `enabled: true` for a given machine, the server
  calls `Gentility.Egress.Routes.upsert_default_for_machine/1`, which
  idempotently inserts a default `Route` row with
  `name = machine.slug`, `machine_id = machine.id`, **`enabled: false`**.
  This is the **Tailscale two-phase commit pattern**: the agent
  *advertises* the capability, but the route is **inert until a human
  approves it** in the UI. A compromised or misconfigured agent cannot
  silently become egress for the org.
- The server **never overwrites** an existing route's `name` or
  `enabled` flag based on subsequent advertisements. The agent
  provisions; the human curates.
- Multiple routes per machine remain a manual, supported case — the
  auto-created default just covers the 90% scenario where one machine =
  one egress identity.

**Forward compatibility.** The `egress` object is intentionally a map,
not a bare boolean. Future capvers may add advisory fields like
`outbound_ip`, `region`, `pool` *without* bumping capver, provided the
server tolerates their absence. A new capver bump is only needed if a
future field becomes load-bearing (server behaviour requires it).

#### (b) Drop the redundant per-frame `v` field

Through capver 3, every server-emitted frame carried a `"v"` key whose
value was `state.capver`. This was a duplicate of the capver established
at handshake — the value never differs from connection state, and tagging
each frame with it told the agent nothing it did not already know.

Capver 4 **stops emitting `v` from server frames entirely**. Agents that
still send `v` on incoming frames are tolerated (we just ignore the
field). Agents that previously *required* `v` on incoming server frames
must stop requiring it.

Capver is now exclusively a **connection-level** concept: declared once
at handshake via `?version=N`, lives on the socket state on both sides,
never repeated per-message.

#### (c) Rename the provisioning flow's overloaded `v` field to `pv`

The progressive-provisioning request handler used to read
`Map.get(request, "v", 1)` and compare it against
`state.provisioning_version`. That `v` was **not** capver — it was a
separate "provisioning protocol version" concept that happened to share
a JSON key. Footgun.

Capver 4 renames that field on the wire to `pv` (provisioning version).
Provisioning is its own version negotiation that exists *before* the
machine has an identity — capver itself doesn't even apply yet at the
point of provisioning. Splitting the names removes the collision.

If the agent is sending provisioning requests (the OAuth-token onboarding
flow, not the machine-key reconnect path), it must change the field name
in those request payloads from `v` to `pv`.

---

### Agent-side migration to capver 4 (Crystal)

This is the concrete checklist for `proxy5.cr`. Every step is independent
of the others; you can land them in one PR or several.

#### 1. Bump the declared capver

In whatever module builds the WebSocket connect URL, change the `version`
query param from `3` to `4`:

```crystal
# before
ws_url = "#{base}?agent_version=#{VERSION}&version=3"

# after
ws_url = "#{base}?agent_version=#{VERSION}&version=4"
```

The machine key is not part of that URL: it travels in the upgrade's
`Authorization: Machine-Key <key>` header. Agents up to 1.2.17 sent it as a
`machine_key` query param, which the server still reads and logs as
deprecated.

That's it for the handshake — there's no new field, just the integer
bump. The server already accepts capver 4 (it accepts any integer; what
matters is which behaviour clauses the server has gated on `>= 4`).

#### 2. Add an `egress.enabled` config flag

Add a top-level `egress` section to the agent config file (TOML / YAML /
whatever the agent uses today), defaulting to `enabled: false`:

```toml
[egress]
enabled = false   # opt-in: set to true to advertise this machine as an egress route
```

The flag is **operator opt-in**. Default off. There is no auto-detection
— the operator declares intent.

#### 3. Include `egress` in the `agent.status` payload

The struct that serializes `agent.status` should gain an optional
`egress` field, sent inside the inner `status` object:

```jsonc
{
  "type": "agent.status",
  "status": {
    "hostname": "...",
    "local_ip": "...",
    "security_enabled": true,
    "security_mode": "...",
    "egress": { "enabled": true }   // ← NEW; omit when config flag is false
  }
}
```

Send it on the same cadence as the existing status fields: at connect
and on any subsequent status push. The server's handler is idempotent —
sending it on every push is fine.

**Omit the field entirely** when `egress.enabled = false` in config. Do
*not* send `egress: { enabled: false }`, because future fields under the
`egress` namespace might be advisory and present-but-disabled has no
useful semantics. Absence means "I don't advertise egress."

#### 4. Stop emitting `v` on outgoing frames

Drop the `"v"` key from every outgoing message struct. The server no
longer reads it. (Old server code did read it for one specific provisioning
check — see step 5 — but never elsewhere.)

If your message-builder helpers always inject `v`, remove the injection.
This is a passive change for the wire — older servers (capver-3-only)
that *expect* `v` will not break, because `v` was always advisory.

#### 5. Provisioning flow: rename `v` to `pv` in request payloads

If the agent uses the OAuth onboarding flow (sends `session.provision`
or progressive provisioning request messages), those request payloads
used to look like:

```jsonc
{
  "type": "session.provision",
  "v": 1,                      // ← OLD: provisioning protocol version, named ambiguously
  "organization_id": "...",
  "environment": "..."
}
```

After capver 4, send `pv` instead:

```jsonc
{
  "type": "session.provision",
  "pv": 1,                     // ← NEW: provisioning protocol version
  "organization_id": "...",
  "environment": "..."
}
```

The integer value (`1`) does not change — only the field name. The
server now reads `Map.get(request, "pv", 1)` for the version compare.

If the agent only uses the **machine-key reconnect path** (not OAuth
onboarding), this step is a no-op — there's no provisioning round-trip.

#### 6. (Optional) Strip `v` from the welcome / provisioning-incomplete reads

Server-emitted provisioning responses (`session.provisioning.created`,
`session.provisioning.incomplete`, `session.provisioned`) used to carry
`v` as well. They no longer do. If the agent reads `v` from those
messages, stop doing so — those fields are gone. The
`provisioning_version` field on the welcome message remains as the
authoritative provisioning version negotiated by the server.

---

### Server changes that landed with capver 4 (for cross-reference)

- `lib/gentility/connector/protocol.ex` — `@current` bumped to `4`.
- `lib/gentility_web/sockets/agent_socket.ex` — all `"v" => state.capver` keys
  removed from outgoing frames; provisioning request handler now reads
  `pv` instead of `v`; hard-coded `"v" => 3` on the `egress.stream.error`
  reply removed; new `egress` detection block in `:agent_status` handler
  that calls `Gentility.Egress.Routes.upsert_default_for_machine/1`.
- `lib/gentility/egress/routes.ex` — new `upsert_default_for_machine/1` that
  inserts a `Route` with `enabled: false` if no row exists for the
  machine, and is a no-op otherwise.
- `protocol/agent-protocol.asyncapi.yaml` — **TODO**: add `egress` to the
  `agent.status` payload schema, remove `v` from the message envelopes,
  rename provisioning request `v` → `pv`. (Not blocking the agent — the
  wire format above is the contract.)

---

### Capver 5 — read-only queries

Capver 5 adds one parameter and changes no event name, frame or reply.

- **Server → agent:** every read the server sends an agent at capver 5 or
  above carries `"read_only": true` in the params of `psql_query`,
  `mysql_query`, `psql_query_encrypted` or `mysql_query_encrypted`
  (`Gentility.DB.Transport.Agent.put_read_only/3`). A write never carries
  the key; it is left out, not sent as `false`. An agent below 5 never sees
  it.
- **The promise:** an agent that declares 5 starts a `read_only` query
  read-only, so an accidental single-statement write on the read path is
  refused by the database. It is not a boundary: a query can switch it off
  (`SET transaction_read_only = off`, `BEGIN READ WRITE`). The server's read
  classifier, which refuses SET, BEGIN and multi-statement SQL on reads, and
  the target's database role are what hold. The Crystal agent sets
  `PGOPTIONS=-c default_transaction_read_only=on` for `psql` and
  `--init-command=SET SESSION TRANSACTION READ ONLY` for `mysql`, the same
  settings the server's direct pools connect with
  (`Gentility.DB.PoolRegistry`). The database's refusal comes back on the
  ordinary failure path (`success: false`, `error`).
- **Which machines read as 5:** the live registration, through
  `Gentility.Connector.capver/1`. A Go Connector node always registers as 5,
  because `node.sql` takes `read_only` (`protocol/node-rpc.md`).
- **Below 5:** a read on a `write_protected` target is still allowed and
  logged (the grace in `Gentility.DB.Transport.Agent`). The grace is meant
  to come out once the fleet speaks 5.

Agent change: gentility-ai/agent#7.

---

# Node capability versions (`node_capver`)

> **Current node_capver:** **1** &nbsp;&nbsp; **Floor:** **1**
> **Source of truth:** `Gentility.Overlay.Capver` (`lib/gentility/overlay/capver.ex`)
> **Design:** `protocol/2026-08-28-org-overlay-network-design.md` §15
> **Phase 1 plan:** `docs/plans/2026-08-28-overlay-phase1-master.md` §2.1

## What this number covers, and what it does not

`node_capver` versions **everything the node and the mothership say to each
other that is not Tailscale**: enrollment and exchange, the config document,
the node `api`, the egress admit call, status and presence.

It also fixes the two protocols on the **gateway → listener hop** — the signed
peer header and the gateway's own certificate fetch. A node speaks neither: they
exist only on the plaintext hop inside the deploy's docker network. They are
versioned here because the gateway is the same binary, shipped and pinned the
same way, and the app and the gateway have to agree on the spelling byte for
byte.

It deliberately does **not** cover the control protocol between the Connector
and the head. That is `tailcfg`'s own capability version, plumbing between two
programs we both ship — the `tsnet` library compiled into the Connector and the
headscale fork — and it moves only when we move the pinned pair
(Tailscale `v1.101.0-pre` × headscale `v0.29.3`, see the Connector's `go.mod`).

## Rules, verbatim from Tailscale's

- The node sends `X-Node-Capver: <n>` at enrollment and on **every** config
  pull, presence post and API call. It is recorded on `node_presence`, so the
  machines list can show nodes below floor the way Tailscale's console shows
  outdated clients.
- **Gentility refuses a request below the floor.** A node between the floor and
  current receives a config document it understands; a node *below* the floor
  receives one that says only "update", and — fail-static — keeps its existing
  paths but may not advertise new services.
- **Additive-only within a capver.** A removal or a semantic change is a bump.
  Never branch: one integer.
- **A floor raise is a dated decision**, taken after the fleet is above it. A
  customer's server Connector lags while the mothership deploys many times a
  day; that gap is the reason this number exists.

## How to add a node_capver

1. Bump `@current` in `lib/gentility/overlay/capver.ex`.
2. Add the new fields to the config document (`Gentility.Overlay.Render`) and
   render per capver — `node_config_revisions` are keyed by
   `(node, revision, capver shape)`.
3. **Add a section to this file** describing what the capver adds.
4. The Connector ships matching support and starts sending the new number.
5. Raise the floor later, on its own dated decision, once the fleet is above it.

## node_capver history

### node_capver 1 — enrollment, config, presence, egress admit, the gateway hop

**2026-08-29, S3 close:** capver 1 now means enrollment exchange, the config
document v1, and presence — the three surfaces below, live against a real
join. (`egress admit` is named for completeness, per master §2.1's full
vocabulary for this capver; S5 is what actually implements it.)

**2026-08-30, S4 close:** capver 1 also covers the gateway hop, both halves of
it: the **signed peer header** the gateway stamps on everything it forwards, and
the **gateway certificate endpoint** (`POST /internal/gateway/cert`) it fetches
its own TLS leaf from. Both are live — a joined node's config pull arrives at
`Gentility.Overlay.Listener` with the peer attributed, and the gateway serves
TLS with a leaf this endpoint issued. Neither is anything a node sends, so
neither moves the number; they are written down here because they are the
contract between the Go gateway (`node/internal/gateway`) and the app, and there
is nowhere else that both sides read.

The first version. Everything a Phase 1 node speaks:

- **Enrollment exchange** — `POST /overlay/enroll/exchange`, authenticated by
  the enrollment nonce (no bearer), answering `{node_id, runtime_token?,
  node_credential, config}`. `config` is `null` unless a document has already
  rendered by the time of exchange — the config channel's long poll is what
  normally delivers the first one — and where one has (a live join's first
  render happens on the head's registration), the number a node actually sees
  is **revision 1**, not 0: a node has no opinion about which number it
  holds, `since` is a cursor rather than an index. Idempotent on
  `(enrollment_id, node_id, nonce)` for one hour.
- **Config channel** — `GET /overlay/self/config?since=<rev>`, bearer = the
  node credential, long-poll up to 60 s, `304` when nothing changed. Document
  shape v1: `renderer_version`, `revision`, `capver_floor`, `node_id`,
  `grants`, `services`, `egress`, `gateway`, `update` (master plan §2.5).
- **Presence** — `POST /overlay/self/presence` with `{version, capver,
  endpoint, relayed, addresses}` → `204`. A heartbeat is **total**: it
  overwrites the node's `NodePresence` row in place rather than patching it,
  so a field a heartbeat leaves out is recorded as absent, not left at its
  previous value.
- `services` is **not in scope for capver 1.** A capver-1 node sends no
  `services` on its heartbeat, and the server records `[]` — never a
  best-effort echo of the config document's own `services`. S5 introduces the
  node reporting its own services under a later capver; until then this field
  is present on `NodePresence` but unread by anything.
- **Egress admit** — `POST /overlay/egress/admit` with `{source_address, host,
  port}` → `{decision, reason, fact_id}` within 2 s.
- **The gateway's signed peer header, byte for byte.** A node never sends
  these: they exist only on the plaintext gateway → listener hop inside the
  deploy's docker network, and this is the contract the Go gateway
  (`node/internal/gateway`) writes and
  `Gentility.Overlay.Ingress.SignedPeer` reads.

  | Header | Value |
  |---|---|
  | `X-Overlay-Peer` | the peer's overlay address, exactly as the gateway's tsnet `WhoIs` resolved it |
  | `X-Overlay-Peer-Ts` | the mint time in unix seconds, canonical decimal — ASCII digits only, no sign, no leading zero, no padding, no fraction |
  | `X-Overlay-Peer-Sig` | the MAC below |

  The signed message is those two values joined by one ASCII colon and
  nothing else — no length prefix, no trailing newline, no header names:

  ```
  message = <X-Overlay-Peer verbatim> ++ ":" ++ <X-Overlay-Peer-Ts verbatim>
  sig     = base64.RawURLEncoding(HMAC-SHA256(key = OVERLAY_INGRESS_SECRET, message))
  ```

  In Go: `base64.RawURLEncoding.EncodeToString(mac)` — **raw**, so unpadded
  and url-safe. `StdEncoding` and any padded encoding are refused as
  `bad_signature`; the app decodes with `Base.url_decode64(padding: false)`
  and does not fall back.

  The timestamp gets the same one-spelling rule.
  `Gentility.Overlay.Ingress.SignedPeer` checks `X-Overlay-Peer-Ts` against
  `~r/^(0|[1-9]\d*)$/` before it is parsed as an integer — `Integer.parse/1`
  alone accepts a leading `+` and leading zeros, and the payload is rebuilt
  from the parsed value, so without this guard several spellings of one
  second would sit under one signature. A timestamp spelled `+1756000000`
  or `001756000000` is **refused as `bad_signature` (401)** — the same
  answer as a MAC that does not check out, and decided the same way, before
  the clock is even read. The Go gateway only ever emits the canonical
  spelling: both `certs.go` and `inbound.go` mint with
  `strconv.FormatInt(unixTs, 10)`, so this guard exists for what a header
  *could* carry, not for anything the gateway itself sends.

  Both halves are in the MAC: signing the address alone would make every past
  header valid forever, and signing the time alone would let one captured
  signature name any address for the length of the window. The timestamp is
  minted **per request**, not per connection — two requests forty seconds
  apart on one keep-alive both have to verify.

  The window is **±30 s**, symmetric because the gateway and the app are two
  containers with two clocks. Outside it the app answers `stale_signature`;
  inside it, a MAC that does not check out is `bad_signature` — decided in
  that order, so a forgery never learns which timestamps the secret covers.

  `OVERLAY_INGRESS_SECRET` may be rotated: the app also accepts
  `OVERLAY_INGRESS_SECRET_PREVIOUS` while both sides roll. The gateway always
  signs with the current one.

  **The gateway deletes every client-supplied `X-Overlay-Peer*` header before
  setting its own** — all three, on every request, whatever their case. That
  deletion is the reason the listener may believe the header at all: without
  it a node could name any address and the MAC would be minted over the lie.

- **The gateway's internal bearer, byte for byte.** A second, separate
  protocol on the same plaintext docker-network hop: the gateway presents
  this bearer to fetch its own TLS leaf from
  `GentilityWeb.OverlayInternalRouter` (`POST
  /internal/gateway/cert`), authenticated by
  `Gentility.Overlay.InternalToken` on the Elixir side and
  `gateway.Bearer` (`node/internal/gateway/sign.go`) on the Go side.

  ```
  payload = "gateway:" ++ <unix seconds, canonical decimal>
  key     = PBKDF2-HMAC-SHA256(secret = OVERLAY_INGRESS_SECRET,
                                salt = "overlay internal bearer v1",
                                iterations = 1000, length = 32 bytes)
  bearer  = base64url(payload) ++ "." ++ base64url(HMAC-SHA256(key, payload))
  ```

  `base64url` is `Base.url_encode64(padding: false)` / Go's
  `base64.RawURLEncoding` — unpadded, url-safe, same discipline as the peer
  signature. The window is **±60 s, symmetric**, twice the peer signature's
  because this bearer only ever crosses the docker network at boot and at a
  renewal, not per request. The key is a **derived** key, not the raw
  secret: `Gentility.Overlay.IngressSignature.derived_keys/1` runs
  `Plug.Crypto.KeyGenerator.generate/2` (defaults: 1000 iterations, 32-byte
  output, `:sha256`) once per configured secret, so `OVERLAY_INGRESS_SECRET`
  is never MACed with directly. The salt (`"overlay internal bearer v1"`,
  `Gentility.Overlay.InternalToken.@key_salt`) is the domain separation
  between this protocol and the peer signature above — they share one secret
  but sign under different derived keys with different subject strings
  (`"gateway:<ts>"` here, `"<peer>:<ts>"` there), so a captured message from
  one can never verify as the other's. Like the peer signature, this walks
  `[current, previous]` and accepts either during a rotation.

  The call itself: the request body is `{"addresses": ["fd06:6e7:…", …]}` — every
  overlay address the gateway's netmap gave it, one per organization it holds a
  grant in — and **the hostname is not the caller's to name.** The subject and
  the `dNSName` SAN are the installation's `platform_hostname`, the one name
  every config document tells a node to dial; a bearer-holder that could choose
  the subject could have the internal root sign a certificate for anything.
  Each address must be one this addressing scheme could have issued
  (`Gentility.Overlay.Prefix.locate/1`), so the SAN list cannot be steered off
  the overlay; a live grant is **not** required, because the gateway asks at
  boot when its own grant may be seconds old and a certificate is not
  authorization. The response is
  `{certificate_pem, private_key_pem, ca_pem, not_before, not_after}` — a fresh
  P-256 key every call, ninety days, `serverAuth` only, nothing about the leaf
  persisted, so a renewal is just another call. `ca_pem` is the same root the
  config document pins as `gateway.ca_pem`. Every answer, refusals included, is
  JSON with one `error` key, because the caller is a program: **401**
  `invalid_bearer` for a missing, malformed, expired or wrongly-keyed bearer;
  **403** `overlay_peer_on_internal_surface` if the request carries any
  `X-Overlay-Peer*` header at all — a request that has one arrived through the
  gateway, so a node sent it, and that check runs before the bearer is even
  read; **422** `invalid_request` for a body that is not a list of addresses and
  `no_addresses` / `invalid_address` for a list the issuer refuses; **500**
  `internal_error` if the root itself could not be read or minted.

- **A 401 is not always the node's own refusal, and the Connector's exit
  codes say so.** The ingress plug answers 401 for several distinct reasons —
  `no_credential`, `bad_credential`, `node_revoked` (the node's own
  credential), and also `no_overlay_peer`, `ambiguous_peer`, a verifier
  refusal such as a stale timestamp (the gateway's own misconfiguration, not
  the node's). Only the first three end the Connector's process
  (`exitcodes.EnrollRefused`); the rest are retried like any other transient
  failure, because a node that took itself off the network over a gateway
  fault it cannot see or fix would be the wrong side closing the loop. On the
  exchange call, which presents no bearer at all, every 401 is retryable —
  there is no credential of the node's own there to have been refused.

Transport pin for this version: Tailscale `v1.101.0-pre`, head base
headscale `v0.29.3`.

### Refused reach reports — 2026-10-11

Within node_capver 1, upgraded connectors POST `/overlay/self/refusals` at
most once a minute: `{edges: [{side, source, destination, protocol, port,
count, first_seen_at, last_seen_at}], overflow}`, ≤256 edges, TCP only.
`side` is `destination` (this node's filter dropped the SYN) or `source`
(this node had no peer for it). The mothership keeps an edge only when both
addresses are active grants in one organization and the reporter is the
named end. 204 recorded; 422 `too_many_edges` or `invalid_request`; 429
`rate_limited`; a mothership older than the route answers 404 and the
connector drops the batch. Old connectors never report. This does not raise
the capability floor.

### Additive presence observations — 2026-10-11

Within node_capver 1, upgraded connectors send optional
`network_report_version: 1` and three fields on each `/overlay/self/presence`
heartbeat:

- `peer_paths`: `[{address, path, relay?}]`, one entry per peer 6net address.
  At most 256 entries, active peers before idle ones, so a gateway that sees
  every organization's peers stays inside the 64 KiB body limit. `path` is
  `direct`, `derp`, `peer_relay`, `idle`, or `unknown`. It comes from the
  peer's status, not `Self.Relay`. `relay` is a DERP region only; public peer
  endpoints and peer-relay addresses are not uploaded. Inactive peers are idle
  even if the library retains an earlier endpoint.
- `local_addresses`: `[{interface, address, subnet}]` for up, non-loopback
  host interfaces, at most 64, private addresses only: RFC 1918 IPv4 and
  unique-local IPv6 (`fc00::/7`). The connector's own overlay interface and
  6net address range are excluded; the subnet is the masked prefix. Public and
  link-local addresses are never sent, and the mothership answers 422 to a
  heartbeat that carries one: every org member can read these, and a global
  IPv6 address names its owner's home network. They are the interfaces visible
  in the connector's network namespace, including other VPNs and containers,
  not a claim about physical LAN topology or a subnet route advertised through
  6net. An interface the connector cannot read is skipped and logged; it never
  costs a heartbeat.
- `home_derp`: the machine's home DERP region, not proof of relayed traffic.

The legacy fields remain unchanged for compatibility. These additions do not
change admission or raise the capability floor. An old sender's next heartbeat
clears the new fields; an empty version-1 report means reporting is supported
but contains no observations. All observations are ephemeral and replaceable.

The console joins observations only onto already-permitted paths within the
selected organization, matching that org's peer address. It uses the newer
report from either end, names its observer, and marks it stale after 120
seconds without a report. A separate server-stamped `network_reported_at`
prevents legacy heartbeat updates from refreshing old observations after an
application rollback. Client clocks do not set freshness. It never classifies
a link using the legacy machine-wide relay flag. The visible page refreshes
every 30 seconds. Local prefixes are scoped to each machine: equal private
prefixes do not identify a common network.
