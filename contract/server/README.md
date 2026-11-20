# The Desk ↔ server contract, recorded

Every file here but this one and `contract.json` is written by the Elixir suite
(`test/gentility_web/channels/desk_contract_fixtures_test.exs` through
`Gentility.DeskContractFixture`) and read by the Desk crate
(`desk/core/src/testing.rs`, `contract`). Do not edit a fixture by hand:
change the server, run that test twice locally, commit what it wrote. The
first run rewrites every stale file a test records and fails that test; the
second passes. On CI a stale fixture fails and writes nothing.
`share/` is no channel's: it holds the installer's HTTP exchanges with
`POST /desk/share/redeem` (`desk_share_controller_test.exs`), each
`{request, status, response}`, and the installer that shares a Linux machine
reads them; the schema test and the crate's walkers pass them by.
`../repository_forms.json`, beside this directory, is kept by hand, written
by no test, and read by both suites. It holds
`Gentility.Projects.repository/1` and the core's normaliser to one form.

`contract.json` is the contract itself, kept by hand: an OpenRPC 1.3
document whose methods and notifications are named `<domain>.<verb>`, each
with its Phoenix address (`x-phoenix {channel, event}`) and the words it may
refuse with (`x-refusals`; `x-common-refusals` are every verb's). A typed
message has JSON Schema 2020-12 params and result. Every message is typed,
and the schema test forbids untyped additions. Params are the client's and stay open; an object
the server writes says `additionalProperties`, and is closed unless there
is a reason. `desk_contract_schema_test.exs` holds every recorded exchange
of a typed message to its schema and asks for an ok answer of each;
`outbox.rs` reads each one as the crate reads it, and names who reads any it
does not. What the phone sends is read the same way by
`bin/desk-phone-readers`, which compiles the phone's own wire types with
`swiftc` and runs on Linux in CI.

`mix desk.contract.compat <base>` holds `contract.json` to the one at a base
commit, read from git (`GentilityWeb.DeskContract.Compat`): a typed message
is not removed or retyped without being `deprecated: true` first, a param
the client must send is not added, a field the server writes is not dropped
or loosened, and a written enum gains no value unless `x-open`. CI runs it
against the pull request's base, `bin/check` against `origin/master`. It
also fails when the working tree has no contract, when a typed message goes
back to `x-untyped`, and when it compared no typed message, since a check
that compared nothing proved nothing. Mark an enum `x-open` only when every
reader has a test that reads a value it does not know; otherwise a new value
rides a message gated by an `x-feature` hello word. A major `info.version`
bump waives the check and requires the project owner's explicit approval.

The same methods are served as JSON-RPC 2.0 over one WebSocket at
`/desk/rpc` (`GentilityWeb.DeskRpc`; the document's `x-rpc` says the rules),
a domain at a time: the server's hello word `rpc_<domain>` says which
(`x-rpc`'s `features`), `loopers` first, `machine` second and `session`
third, then the lobby's `sessions`, `inbox`, `grants`, `hosts`, `door` and
`refusals`, `pulls`, `projects` and `questions` domains. Questions serves list,
answer and dismiss with separate limits of four reads and four combined
writes; a dead write answers RPC-only `outcome_unknown` without retry.
Projects serves `projects.list`
and `projects.repositories`; `lobby.projects_changed` stays Phoenix-only
until a watch exists. Pulls has separate bounds of four lists and four
writes per connection. Excess calls return `unavailable`; a write task's
death returns RPC-only `outcome_unknown` (-32603), with no retry or replay.
The `lobby.refusals_changed` hint stays Phoenix-only
until its RPC watch is built. The scope and feature maps
are keyed by the method-name domain, with the word mechanically
`rpc_<domain>`. The `rpc` prefix is control; `lobby` remains explicitly
unserved. `phone` serves register, devices, revoke, mute and mutes under
`rpc_phone`, with param-less `person` scope: the authenticated person, whose
credential is rechecked on every call. Bound hosts are forbidden. Other
domains take their scope beside their params (`org`
for `loopers` and `machine`, `session` for `session`; a `machine` method
acts for the machine the connection's hello named; a `session` method
passes the session's door on every call, as the join did once, and a
session not there or not the connection's is `forbidden`,
`rpc/session_forbidden.json`). Retired `session.message` and `session.tool_call`
are unknown on both framings and write nothing. A refusal is an
`error` whose `data` is the Phoenix framing's refusal, and the `rpc.*`
methods (`hello`, `watch`, `unwatch`) and the `rpc.revoked` notification
have no Phoenix address. Watching `machine` (`rpc/watch_machine.json`) is
what joining `desk:<org>:<machine>` was: the machine is held live until the
connection's last machine watch goes, and `machine.ask` is pushed, carrying
`org`, for every ask still open and each one raised (`rpc/machine_ask.json`).
`rpc/<name>.json` is one exchange there, as its frames: `request` and
`response`, or a `notification`. Each recorded exchange of a domain served
there is asked over `/desk/rpc` too and must answer as the channel does
(`Gentility.DeskRpcFixture`). Existing domains use a rollback fast path.
Migrated lobby scenarios run independently per transport in fresh sandboxes, so
asynchronous work and after-commit notifications run normally. Both runs pin
the same JSON; an exact coverage check rejects missing exchanges and unused
exceptions. Transport-specific refusals have explicit RPC recordings.
`sessions.ask` declares its RPC-only task failures in `x-rpc-only-refusals`,
a subset of `x-refusals` excluded from Phoenix's generated vocabulary.
Both use -32603: `task_died` follows a serialized recovery and includes an
ask id only if the call committed one; `outcome_unknown` means recovery
failed and carries the minted id without asserting whether it committed.
The sessions watch carries `sessions.changed` hints with `org`; the feed
reply remains the authority for cursor and generation. Watch first, then
pull from the stored cursor until the feed has no more pages.

`groups` (T-622, ruling G) is the first domain served only on `/desk/rpc`:
`groups.list`, `groups.get`, `groups.convene` and `groups.stop` have no
`x-phoenix` address, under `rpc_groups`, and their words sit under `rpc` in
`refusals.json` by whole method name. The core asks them on the lobby's
topic by method name; on a link not carrying them it refuses each at once,
`unavailable`, "Groups need Desk's newer connection to the server, which
this link is not using." Anyone in the organization lists and reads; Desk's
own sign-in convenes for its person, the operation key bound to the person
and the hello's machine, members defaulting to that machine; the convener
or an organization admin stops. Bound hosts are forbidden. Reads and writes
have separate bounds of four; a dead write answers RPC-only
`outcome_unknown`. Watching `groups` (`rpc/watch_groups.json`) hears
`groups.changed {org, group_id}` on every append to a group's ledger.

Migrating lobby declarations are renamed in place, retaining their Phoenix
addresses and recordings. Compat permits a rename at the same unique
Phoenix address only when the base did not serve that method over RPC, and
still checks its complete shape. Once served, the name cannot be renamed
under that allowance.

Keep `/desk/rpc` as `GentilityWeb.DeskRpc` sets it up: no browser pipeline,
so no session or cookie is read and the `Authorization` header is the only
credential; no log line carries that header; an explicit frame limit and
idle timeout; its own drainer on deploy (`GentilityWeb.DeskRpc.Drainer`);
and telemetry on connect, refuse and close. A close with 4401 means the
credential is dead: the client logs in or binds again, and never retries the
same credential.

`refusals.json` is `GentilityWeb.DeskContract.words/0`, read out of
`contract.json`: channel → verb → the words that verb can refuse with
(`"*"` is every verb's). Add a word to the contract, never to this file.
A ratchet in `desk_contract_test.exs` proves every word a channel spells is
declared; `outbox.rs` proves Desk decides every word declared for `open` and
`append`.

`<channel>/<name>.json` is one exchange: `event`, `payload`,
`reply {response, status}` and `topic`, or `join` params, `reply` and
`topic` for a join. Keys are sorted. A refusal — an exchange whose reply's
status is `"error"` — is not a file but one line of `refused.jsonl`: the
same exchange with `name` (`<channel>/<name>`) first, one a line, the lines
in name order, so a new refusal word is one line beside the others and two
pull requests adding words conflict only where their lines are neighbours.
Where this file names a refusal as `<channel>/<name>.json`, it is that line.
A name is a file or a line, never both; the recorder moves it when its
status changes, and both suites hold every line to a word declared for its
verb.

A session row in an exchange is written as its difference from one
canonical row: `{"$row": "<kind>"}` and the fields where it differs from
`rows/<kind>.json`, a field it holds, a null included, being its value. The
kinds are `work_session`, the lobby `list`'s row (`WorkSessionRow` in
`contract.json`), and `feed`, the `feed`'s (`FeedRow`); the canonical rows
are the first row `work_sessions/list` and `work_sessions/feed` record. The
recorder knows a row by its keys being its component's properties, and
every reader reads a row back whole (`contract::fixture`, `Fixture.server`,
`DeskContractFixture.expand/1`), so a field the server adds touches the
component, the canonical row and only the exchanges whose rows differ in
it. A change to a canonical row can take a third run of the suite. `rows/`
holds no exchanges. The recorder takes a lock file in the system temporary directory
before it rewrites the file, so partitioned local runs at once do not lose
a line. Ids are
`00000000-0000-7000-8000-00000000000N` by first appearance, inside a
string too; every instant is `2026-01-01T00:00:00.000000Z`; the
deployment's address is `https://gentility.test`, and its MCP host's
`https://mcp.gentility.test`. A topic names its ids by
role: `work_sessions:<org>`, `looper_threads:<org>`,
`work_session:<session>`, `desk:<org>:<machine>`.

A push the server makes unasked — `desk/ask.json` (kind `phone_claim`),
`desk/ask_session_start.json` — is `topic`, `event` and `payload`, with no
`reply`. `desk:<org>:<machine>` pushes `ask` with
`{id, kind, payload, expires_at}`. Its join replies `{user_id}`, the
socket's person, or null for a bound host (`desk/join.json`). A
`phone_claim` ask is answered on the `answer` verb (`desk/answer*.json`).
An `allow` carries `match`, the two digits the redeeming phone shows; one
without them, or with others, is an ok reply that denied the claim,
`{id, status: "denied", reason: "wrong_number"}`
(`desk/answer_no_number.json`, `desk/answer_wrong_number.json`). The ask
never carries them. Phoenix's debug channel log prints an `answer`'s `match`
by design: `"match"` is not in `:filter_parameters`, whose substring filter
would blank every `*match*` parameter in every log, and the two digits are
one attempt at a claim dead within minutes (F6). Do not add it.

A `session_start` ask is pushed for each open ask of that machine, once
per join: after the join, or when it is raised. The core answers with an
`ask_answered` fact on the same topic's `fact` verb
(`desk/fact_ask_answered.json`, `desk/fact_ask_refused.json`), and the
reasons it may refuse with are `refusals.json`'s `desk` → `ask_answered`.

`asks.json` is `GentilityWeb.DeskContract.asks/0`, read from
`machine.ask`'s `x-kinds`: each ask kind → its `payload` keys, the
`decisions` an answer may settle it with (`refused` among them, with a
`reason`) and the `refusals` the core may give. Like `refusals.json` it is
not an exchange. `contract.json` declares each kind's payload as a schema,
and `desk_contract_test.exs` holds `Gentility.Desk.AskKinds` to it word for
word; every recorded ask's payload is held to its kind's schema, and
`mix desk.contract.compat` holds each kind as the server's writing and its
decisions as the core's. The session commands —
`session_resume`, `session_stop`, `session_answer`, `session_manage` — are
pushed as `ask` like a start (`desk/ask_session_<kind>.json`, each payload
carrying `session_id`, the holder `epoch`, and `by {id, name}`), and answered with an
`ask_answered` fact `{ask_id, decision, session_id}`
(`desk/fact_ask_answered_session_<kind>.json`). An agent's ask — a command
called from inside a Desk thread — carries the asking thread as
`by.session_id` (`desk/ask_session_manage_by_agent.json`); the core holds it
to the `manage_threads` grant and refuses in `management_grant_required`,
`self_management_off` or `self_answer`
(`desk/fact_ask_answered_session_manage_by_agent.json`). The server raises
such an ask only of a Desk whose hello said `agent_asks`.

A lobby `inbox` whose `by.session_id` names the session it changes is a thread
acting on its own posture (T-256): the server takes `settled_at` and
`snoozed_until` and no other field without a grant, where the organization's and
the person's `threads_self_manage` allow it and the socket's machine holds the
session's lease (`work_sessions/inbox_own_posture.json`, and the
`work_sessions/inbox_own_*_refused` lines of `refused.jsonl`), refusing in
`management_grant_required`. A settle is `attention_pending` while the thread is
tagged as waiting on its person or has live attention. The core tells the thread
named by a refused command's `by.session_id` in a note.

A `session_tags` ask (T-196) sets a thread's `role` and `manager_id` at the
Desk that holds it, whichever machine holds the manager
(`desk/ask_session_tags.json`). `manager` is `{id, title,
organization_id}`, the server's voucher for a manager the holder may not
hold, or null when unassigning. The answer is `set`, `unchanged` or
`refused`, with `seq` (the `thread_tags_set` written, as a string) and
`before`, the manager it replaced (`desk/fact_ask_answered_session_tags.json`).
The server raises it only of a Desk whose hello said `session_tags_ask`;
otherwise the lobby's `ask` refuses `desk_update_required`. The server has
already walked the manager's managers across every machine; a loop, a
chain past sixteen threads, a self-assignment, a looper's thread or a
role off the list is refused on the lobby as `malformed_payload` with the
core's own sentence (`work_sessions/ask_session_tags_malformed` in `refused.jsonl`). The
server's own stamp of a thread a coordinator opened elsewhere (T-200) adds
`opened_ask`, the start ask's id, with the opener as `by.session_id` and
`manager_id` (`desk/ask_session_tags_opened.json`): the holder admits it
without a grant only when the child's `start_requested` names that ask.

A lobby `list` row (`work_sessions/list*.json`) carries `attention_line`: what
a waiting row waits on, in one line — an approval's `"Wants to <kind>"` (never
the tool's title, which for a shell call is the command), a question's first
line — or `null`, as it always is on a row the reader does not own.
`work_sessions/list_attention.json` is a row waiting on an approval.

## Rules for a new verb (D11 of the client contract review)

- Declare the method in `contract.json` first, typed: its params, its
  result and its `x-refusals`.
- A refusal is `{reason: <word>}` and, when a person will read it,
  `message: <sentence>`.
- A reply is a map, never a bare value. Wrap it: `{hosts: [...]}`.
- A session in a payload is `session_id`, never `id`. `machine_id` names
  one of two things. It is a Desk store id — the UUID the store minted,
  which `Sessions.Machines.register/3` keeps as its row's key — on the
  connect query, the `desk:` topic and the lobby's `open`, `find` and
  `list`. It is an organisation machine in `machines`, `desk_hosts`,
  `desk_bind_code`, `desk_token` and the `refusal_*` verbs (whose
  `machine_gone` means one). `machines` entries also carry
  `desk_machine_id`: the Desk store id linked to that entry's node, or null
  (a bound host's node is always null: only a person's door links). A new
  verb that needs both must name the store's `store_id`. `desk_machine_id`
  is the one exception: the phone contract pins that name, and the phone
  app decodes it.
- One meaning per word: do not reuse `accepts`, `seq`, `kind`, `type`.
- Instants are ISO 8601 UTC microseconds; ids are UUID strings; counts are
  integers.
- A verb that is not about sessions does not go on `work_sessions`. Put it
  on a topic named for its domain (`overlay:<org>`, `desk_hosts:<org>`) and
  record its exchanges here. The door verbs — `desk_peer`, `desk_door` and
  its inverse `desk_door_withdraw` — are a ruled exception (T-135): they
  stay on `work_sessions` beside each other, where the core's door already
  asks. Do not move them.
- Record the new verb's ok reply and each refusal word in
  `desk_contract_fixtures_test.exs` before the Rust side reads it.

## Starting on a chosen host and fencing run asks

The lobby's `ask` accepts `session_start` with `session_id` (a caller-minted
UUIDv7), `host` (the explicit target Desk), and `payload`. The target may be
the person's personal Desk or a bound Desk they may operate. The organisation
keeps one open start ask per id: a repeat returns that ask, even if it names
another host (`work_sessions/ask_start_repeat.json`).

A target core advertising `start_without_prompt` may receive a start without
`prompt` (`desk/ask_session_start_without_prompt.json`): open and wait for the
first `SessionPrompt`. Without the word, that start is refused `no_host`;
prompt-bearing starts keep the released behavior. Creation options the ask
cannot carry (`make`, `rename_branch`, `later`) are refused `malformed_payload`.
A new start has no holder and no epoch; the target's `open` mints the lease.

When the row already exists, a caller advertising `start_without_prompt`
receives `{status: "opened", session: <Feed.Row>}` without `ask_id`
(`work_sessions/ask_start_opened.json`). The row has the same authorisation
and filtering as its feed replica. A released caller receives `no_host`.

The server hello word `lease_asks` promises an epoch on all four run asks.
A core refuses a mismatching epoch, or a missing epoch under that word, with
`stale_epoch` (`desk/fact_ask_stale_epoch.json`). A missing epoch from a
released server without the word retains the released behavior. The core's
current-local-holder check still applies in every case.

## Addressed harness questions

`addressed_questions` in the server hello admits the bounded holder form in
`work_session/append_addressed_question.json`. The display-only `form` has no
defaults; the original mode and callback remain local. Its server deadline is
clamped to two days and an expiry job is queued in the ingest transaction.
The holder's answer (`work_session/append_addressed_answer.json`) carries only
question identity, request identity and action, never the typed value.

`desk/ask_session_answer_question.json` records the question branch of the
existing ask: `session_id`, `epoch`, `question_id`, `action`, optional `value`
and `by`. It is exclusive with the approval's `request_id` and `option_id`,
including null competing keys. Released cores fail their approval reader and
refuse this payload. The public tool retains two forms; callers get the form
and question identity from `questions`.

An `answered` receipt means recorded before attempting the callback. Live
retries arbitrate locally; after the turn is gone or the holder is unavailable,
the server returns its recorded winner. A late holder answer after server
expiry or cancellation is acknowledged without reopening the question. The
local card remains unchanged until its turn ends or is answered locally.

Addressed holder exchanges retain the released legacy facts. An asked batch
contains the structural record first, then `{request_id, message}`; the latter
sets attention and is the one session event and phone notification. A person
answer records `{question_id, request_id, action}` before callback delivery;
the callback emits the legacy `{request_id, action}` which clears attention.
The answer fixture pins their order; they may arrive in separate appends.
Lost and Withdrawn emit only the legacy ending. Ingest brings the still-open
record's unique expiry job forward, without recording a person answer or
appending a server fact inside holder ingest. Typed `value` and `content` are
removed at the holder door on both answer shapes.

`session_message` is a core-only, grant-free ask gated by
`session_message_ask`. Its server-vouched sender carries the title,
organization and narrower claimed/recorded mode. The core refuses agent
relays of this tool and delivers the ask through the ordinary peer inbox,
including its holds, FYI behavior and loop limits. The ask expires after ten
minutes. Its text persists in append-only `desk_ask_events`; a pending answer
is not a delivery receipt.

A group's message (T-622) is the same ask with `sender` `{kind: "group",
id, title, organization_id}`, raised only of a core whose hello says
`group_message`. The server asks it as the group's person, so `by` names no
`session_id`; a thread sender's `by.session_id` is still its own id. The
core delivers it through the same inbox, framed as a peer message from
`the group "<title>"` with no `reply_to`, and records no thread as its
sender. A `kind` the core does not know is `malformed`.

A remote `queued` answer is recorded as `delivered` in the sender's local
receipt: it confirms the holder queued the message, not that an agent read it.
