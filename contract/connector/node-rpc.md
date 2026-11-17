# The node control socket: `/node/rpc`

Spec: `docs/superpowers/specs/2026-11-10-connector-as-the-one-agent-design.md`.

One JSON-RPC 2.0 WebSocket from `gentility serve` to the mothership. The
node holds it for as long as it runs and reconnects with capped, jittered
backoff. It is the node's presence.

The server side is `GentilityWeb.NodeRpc` (the upgrade) and
`GentilityWeb.NodeRpc.Connection` (the socket).

## Upgrade

`GET /node/rpc` with `Authorization: Node <node_id> <credential>`. The node
names itself; the credential confirms it. 403 `{"reason":"unauthorized"}`
otherwise; 426 when the request is not a WebSocket upgrade. The credential is
checked first, so a request that is not an upgrade and has a bad credential
gets 403, not 426. A node whose credential is revoked is refused the same way
(403) at its next connect, and a socket it holds open is closed `4401` as soon
as the revocation commits.

Text frames only; a frame is at most 64,000,000 bytes. A larger frame closes
the socket (1009).

The server sends nothing to an idle node. An idle socket is closed (1002)
after 60 seconds without a frame from the node. A WebSocket ping inside that
window keeps it open. A node that does not ping reconnects every minute, and
its machine's presence flips offline and online each time.

## From the node

Exactly one request, first:

| method | params | result |
|---|---|---|
| `rpc.hello` | `{version, overlay_address?, services: [string]}` | `{principal: {kind: "node", node_id, machine_id}, update: {latest_version, min_version, manifest_url}}` |

`principal.machine_id` is `null` for a personal node. Each `update` field may
be `null`: no release found, or no floor set.

A refusal is a JSON-RPC error whose `error.data.reason` is its word and whose
`error.message` is prose for a person. Match on `data.reason` and the code,
never on the message. Below the floor: `-32001`, `data.reason`
`upgrade_required` with `data.manifest_url` (the message reads "Connector X is
below the floor Y; update from the manifest."), then close `4426`. A second
hello: refused `-32001 hello_said`. Any other method: `-32001 hello_required`
before the hello, `-32601 unknown_event` after. (`-32001 hello_said` means
code `-32001`, `data.reason` `hello_said`.) Notifications are ignored.

Responses to the server's requests carry the server's id (`"s:<n>"`).

### The floor

`update.latest_version` is the Connector build this deploy ships.
`update.min_version` is the setting `connector_min_version`, published only
when it and the shipped version both parse and the floor is not above the
shipped version; any other floor is published as `null`. Versions are compared
as semver. A version that does not parse, or is missing, is unknown: it is
never below the floor and never refused. A `version` that is not a string is
treated as missing. A `latest_version` that does not parse (today's stamp is
`server-<sha>`) is not an update target on the node's side either: the node
compares it to nothing and updates to nothing.

### The manifest

`update.manifest_url` is `GET /downloads/connector/manifest`, beside the
binary download and, like it, public and unauthenticated:

```json
{"version": "0.1.40", "min_version": "0.1.30",
 "platforms": {"linux/amd64": {"url": "…/downloads/connector/linux/amd64", "sha256": "<hex>"}}}
```

`version` is the config document's `update.latest_version`; `min_version` is
the published floor, or `null`. `platforms` names each binary this deploy
ships by `<os>/<arch>`. A deployment that ships no binary answers 404
`{"reason":"no_release"}`. The config document's `update` block carries the
same digests as `"checksums": {"<os>/<arch>": "<sha256 hex>"}` (lower hex).
The manifest is for discovery: a node verifies a download against the copy in
its authenticated config document, never against the manifest's.

### Presence

A machine-owned node (the hello's principal has a `machine_id`) registers in
the presence registry under `{:node, machine_id}` at hello, with meta
`version` (the binary, or `nil`) and `capver: 5`. The machine reads as
online, `via: "node"`, until the socket closes. Its `last_seen_at` is touched
at hello and on each WebSocket ping (written at most once every three
minutes), which is what keeps it in `list_machines`. A personal node holds the
socket for its own presence and updates, registers nothing, and is sent
nothing. A node whose machine has been deleted is served as a personal node:
its principal's `machine_id` is `null`.

## From the server

Answered only by a machine-owned node. A personal node refuses each with
`-32001 forbidden`; the server never sends one.

| method | params | result |
|---|---|---|
| `node.ping` | `{}` | `{version, uptime_s, engines: ["postgres" \| "mysql"]}` — `engines` required |
| `node.exec` | `{command, timeout_ms, cwd?}` | `{exit, stdout, stderr, truncated}` — `sh -c`, output capped |
| `node.sql` | `{engine, host, port, database, user, password, sql, read_only, timeout_ms, max_rows?}` | `{columns, rows, row_count, truncated}` — one transaction, read-only when asked |
| `node.gateway_install` | `{move_id, bundle_url, sha256}` | `{result, reason?, running: {bundle, upstream_version, plugin_version}}` |

The server sends `timeout_ms` on `node.exec` and `node.sql` only. It sends
neither `cwd` nor `max_rows` today. `port` and `user` may be `null` (the
driver's default). The node must apply its own row and output caps so that
every answer fits in one frame: the server refuses a frame over 64,000,000
bytes by closing the socket, the call then answers `closed`, and an exec or
write op is not retried (see the fallback rule below).

`node.sql`'s `columns` is the column names in order, and `rows` is an array of
arrays aligned with `columns`; each value is a JSON scalar or a string, as the
driver renders it. A result with rows and no `columns` is refused. A result
with `truncated: true` is failed in band by the server ("the node truncated
the result (N rows); raise its cap or narrow the query"), never passed on as
a partial answer.

A query that fails is a JSON-RPC error whose `message` is the database's.

`engines` is the database engines this build can speak: always both,
`postgres` and `mysql`, for the Go drivers. It gates agent-mode Sources on the
machine: the server records each engine as the machine's database client
(`psql`, `mysql`) when the capability probe runs, which it does half a second
after every machine-owned node's hello. A `node.ping` answer without an
`engines` list is an error the server logs; it keeps the capabilities it had.

### Timeouts

The server's timer is the caller's timeout, for every method, whether or not
the params carry `timeout_ms` (`node.ping` and `node.gateway_install` do not).
When it fires the call is over: a caller still waiting is answered `timeout`,
and an answer that arrives later is dropped silently. `node.exec`'s
`timeout_ms` is a second shorter than the caller's (at least a second), so the
node's own timed-out answer, with its exit code and partial output, arrives
first.

Cancellation on timeout: the node slice defines whether a timed-out
`node.exec` is killed (process group) or allowed to finish with its result
dropped. Until then the server treats a timeout as "effects unknown" and never
retries it.

## Codes and closes

Errors: `-32700` not JSON and `-32600` not JSON-RPC or a binary frame, both
with `data.reason` `malformed_payload`; `-32601 unknown_event`; `-32001` any
other refusal word. This door never sends `-32602`.

Closes:

| code | why |
|---|---|
| `1002` | idle: no frame from the node in 60 seconds |
| `1009` | a frame over 64,000,000 bytes |
| `1012` | deploy drain (reconnect) |
| `4401` | `credential_revoked`: the node was revoked, at once |
| `4426` | `upgrade_required`: the hello was below the floor |

## Bridge

Until the Crystal agent is deleted, `Gentility.Connector.NodeBridge` maps
Crystal's `ping`, `check_capabilities`, `execute`, `psql_query`,
`mysql_query` and `gateway_install` onto these methods and back, so every
`Gentility.Connector.call/4` caller is served unchanged. Any other Crystal op
is `unsupported`.

### Fallback during cutover

`Gentility.Connector.call/4` asks a live node socket first. It falls back to
the Crystal agent, when one is connected, as follows:

| node outcome | Crystal agent connected | no Crystal agent |
|---|---|---|
| `unsupported` (a Crystal-only op, or the node answered `-32601` method not found) | the op runs there | `{:error, "The Connector does not support <op>"}` |
| `timeout`, or the socket closed mid-call | the op runs there only if it is safe to repeat | `{:error, :timeout}`, or `{:error, :not_connected}` when the socket closed |

Any answer the node does give, success or error, is final.

Safe to repeat means `ping`, `check_capabilities`, or a query sent with
`read_only: true`. For every other op the node's error is returned and the
server logs that it was not retried because the node may have run it. This is a
rule, not a hint: an `exec` or a gateway install is never run twice to cover a
silent node. A repeatable op that times out on the node and then runs on the
Crystal agent can take up to twice the caller's timeout.
