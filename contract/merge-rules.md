# The merge contract: the run stream and posture

Read this before you write a client that changes a session: the web, the
phone, a new core, an agent calling MCP. It says who may write each part of
a session, how the server orders writes that race, and what a client must do
so that nothing is lost. The fixtures beside this file record *exchanges*.
This file records the *rules* those exchanges obey. It is hand-written. Every
rule names the code that enforces it and a test that proves it.

Citations were checked against master `9c6ac2369` on 2026-11-07. Line
numbers drift, so each one also names its function. Trust the function name
over the line number. Where this file and the code or tests disagree, the
code and tests win and this file has a bug. The evidence behind this file is
`docs/research/2026-11-06-desk-foundations/R4-conflict-cases.md`
(T-326 item 5). Where that report and this file disagree, this file has been
checked against later code (see [Corrections to R4](#corrections-to-r4)).

## Two ledgers, two kinds of rule

A session's state lives in two append-only server ledgers. They obey
different contracts.

| Ledger | Table | Writers | How races resolve |
|---|---|---|---|
| **Run stream** | `work_session_events` | the one machine that holds the session, plus the server's own facts | no merging is needed. Ten **admission** rules decide what is taken, and taken facts fold in `seq` order |
| **Posture** | `work_session_inbox_events` (`lease_minted`, `lease_taken`, `inbox_set`; `lib/gentility/sessions/inbox_event.ex:41`) | the host core, every web and phone device, MCP, agents with grants, reflexes, the server's sweeps | a **per-field last-writer-wins register** on a claimed, clamped clock, with three non-clock overrides. Eight rules |

The run stream has no field with two concurrent writers. The holder writes
`branch`, `turns`, `working`, `status`, `prompts_text`, `harness`, `model`
and `mode`. The server writes `pr_*`, `case_id` and `brief_*`. The question
slot is shared, but it is keyed by question id and lives on one stream.
Posture is the only place where many machines write the same field. Arrival
order is wrong there, and the clock is what makes it right.

---

## Part 1: the run stream

Exactly one writer may append numbered facts (`client_seq`) to a session's
stream: the machine that holds its lease. Facts the server mints carry no
`client_seq` (`refuse_numbered!/1`, `lib/gentility/sessions.ex:828-839`), so
they never collide with the holder's counter. Every race that can reach a
session from outside its host is one of three things: an **ask** to the
holder, a **server-only fact**, or a **posture** command (Part 2). The asks
are a closed set of nine kinds (`@kinds`, `lib/gentility/desk/ask_kinds.ex:30-124`).
No outside party writes the run stream in the holder's name.

### The ten admission rules

A new client that appends to the run stream must obey all ten. A client that
only reads the run stream must still fold it as R6, R8, R9 and R10 say.

**R1. One holder, fenced by epoch.** Only the lease holder's machine
appends. A bound host that does not hold the session gets `not_a_holder`. Any
other sender with an older epoch gets `stale_epoch` and writes nothing. That
refusal is how a superseded holder learns it was superseded. A session nobody
holds is adopted by the first of the person's machines that writes to it.
- Code: `ensure_appender/3` `lib/gentility/sessions.ex:603-617`,
  `check_epoch/3` `:630-638`, `ensure_epoch/3` `:642-667`. Desk side: a
  `stale_epoch` reply appends a local `outbox_superseded` and the row folds
  to `superseded` (`desk/core/src/outbox.rs:2232-2247`,
  `desk/core/src/ledger.rs:1500-1504`). A `lease_heard` that names another
  desk does the same (`ledger.rs:1484-1499`).
- Tests: `test/gentility/sessions/appender_fence_test.exs:80` "an older
  epoch is stale whoever sends it, and writes nothing";
  `test/gentility/sessions/host_holder_fence_test.exs:110` "a host that does
  not hold the session appends nothing, current epoch or not";
  `test/gentility/sessions/lease_test.exs:79` "stale epochs take nothing and
  are checked before replay identity"; Desk
  `outbox.rs:8233` `an_append_carries_the_rows_epoch_and_stale_epoch_supersedes_only_that_session`.

**R2. Server-only kinds come from the server alone.** An app may not mint
any of these kinds: `promoted pr_opened pr_unlinked pr_status pr_commented
pr_heard unarchived status_set question_expired question_cancelled
question_joined event_heard fork_refused brief_bound`, nor
`pr_merged pr_closed pr_reopened` (`Event.followed_types/0`). Sending one is
refused with `server_only_event`. A bound host's `thread_management_grant` is
refused the same way; only the owner's own core may grant.
- Code: `server_only/0` `sessions.ex:884-889`; `ensure_appendable/1`
  `:957-963`; `ensure_grants/3` `:898-921`.
- Tests: `test/gentility/sessions/ledger_test.exs:1181` "the app may not
  write an event only the server writes"; `:1600` "the app's door refuses a
  followed fact"; `:2071` "brief_bound is the server's: a desk or agent
  append of it is refused".

**R3. `opened` lands once.** A second `opened` is refused as
`server_only_event "opened"`.
- Code: `ensure_opened_once/3` `sessions.ex:970-981`.
- Test: `test/gentility/sessions/fork_parent_test.exs:48` "no Desk may open
  a session a second time through append".

**R4. The holder's sequence is contiguous, and a replay is identical.**
Numbered facts must run on from the server's last `client_seq` with no gap
(`sequence_gap`, which says what it `expected`). A replayed `client_seq` must
be the same fact: same type and the same `emitted_at` where both sides have
one. Anything else is `replay_mismatch` and nothing is taken. Replaying the
batch that closed a session is answered `ok`, because it changes nothing. The
Desk diverges on a gap or a mismatch, holds the queue for a person, and never
feeds the server's `expected` back into its counter.
- Code: `take_new/2` `sessions.ex:1057-1061`; `ensure_replayed/2`
  `:1132-1142` (`same_event?` `:1157-1172`); `contiguous_from/2`
  `:1180-1193`; `ensure_open(_, [], _)` `:1023`. Desk: rules 4 and 8 in the
  module doc, `desk/core/src/outbox.rs:22-23, 34-41`; `append` `:2168-2180`
  (gap), `:2199-2202` (mismatch); `diverge` `:2463`.
- Tests: `ledger_test.exs:202` "appends are ordered by client sequence and
  idempotent"; `:256` "is taken only as the event it replays"; `:1848` "the
  batch that closed a session may be replayed"; Desk `outbox.rs:8367`
  `a_sequence_gap_freezes_the_session`, `:9145`
  `a_replay_the_server_holds_otherwise_is_divergence_and_acks_nothing`.

**R5. After the end, only the after-end words land.** On a closed row, a
batch is taken only if every fact in it is one of these: a park word
(`settled unsettled snoozed woken archived unarchived ranked`),
`pr_merged pr_closed pr_reopened`, a message word, `pr_status`,
`unsubscribed`, `fork_refused`, `retitled`, `question_cancelled`,
`question_expired`, any `internal` fact, or a `thread_management_grant` with
`allowed: false` (revoking stays possible after the end; granting does not).
Any other batch gets `session_closed`. An **archived** row is gated the same
way for a core without the `lease` feature (`session_archived`). A `lease`
core still owns its run log, so it may finish the turn and close that were
already under way. The Desk keeps the same list (`AFTER_CLOSE`). On
`session_closed` it abandons the queue and appends a server `closed`.
- Code: `lands_after_end/0` `sessions.ex:1000-1012`; `ensure_open/3` and
  `ensure_not_closed/2` `:1022-1041`; `lands_after_end_event?/1`
  `:1043-1055`; the `lease?` flag is read at the channel join
  (`lib/gentility_web/channels/work_session_channel.ex:51-52`). Desk:
  `AFTER_CLOSE` `desk/core/src/store.rs:1927-1943`; `closed_over_there`
  `outbox.rs:2279-2297`; `archived_over_there` `outbox.rs:2312-2350`.
- Archived on the Desk side: when the server has said `inbox` (the
  environment's features; `Store::environment_inbox`, `store.rs:1884`),
  `session_archived` makes the core ask the lobby for a read-back
  (`lobby_sync_requested_by("archive_readback")`) and retry. The posture
  ledger is the truth, and the core neither writes nor holds anything of its
  own. Without `inbox`, the old path stands: the core appends a server
  `archived` and a local `outbox_held` and holds the queue. When it hears the
  unarchive it lifts the hold and rewinds (`ledger.rs:1366-1368, 1417-1420`;
  `desk/core/src/reactors/lobby.rs:1348-1361`).
- Tests: `ledger_test.exs:1878` "a park fact lands after closed"; `:1957` "a
  run fact after closed is still refused"; `:1995` "a run fact on an
  archived session is refused"; `:2039` "a session that is closed and
  archived is told the truer of the two";
  `test/gentility/sessions/inbox_bridge_test.exs:61` "bridged archive
  bypasses live policy; only a lease core can finish the turn"; Desk
  `store.rs:10878` `what_lands_after_a_close_is_the_park_and_message_words`,
  `outbox.rs:8541` `session_closed_abandons_the_queue_and_closes`, `:8568`
  `session_archived_holds_the_queue_and_archives`, `:8413`
  `a_sequence_gap_on_an_archived_row_holds_rather_than_diverges`,
  `ledger.rs:7239` `an_unarchive_lifts_the_archive_and_the_hold_and_dates_nothing`.

**R6. The question slot: it shows the newest card nothing has ended, the
first terminal state wins, and a person's turn clears any card but an
agent's ask.** A question moves from `open` to one terminal state at most
once. A later answer, expiry or cancellation does not move it. The one
exception is `escalated`, which overrides `answered`. An agent's
ask is a `question_asked` with `source: "agent"` and no `request_id`: what
`ask` writes, and not what a harness holds. The row keeps every open card: a
newer card takes the slot, and the one it replaces waits behind it. An
answer, expiry or cancellation clears the card it names, in the slot or
behind it, and no other; an ask clears the card it escalates. A person's `turn_started` clears every card that is
not an agent's ask, whether the turn was typed or begun by an answer. Any
turn clears every card at or past its expiry. When the slot's card goes, the
slot takes the newest card behind it whose expiry is after that fact's time,
and the attention and its line follow; that writes no fact. A fact's time
here is `emitted_at`, or its arrival where it has none. So the cards kept are
bounded: each goes at its end, or on the first turn past its expiry. A
non-person turn marks a harness question without a card (one with a
`request_id`) `replaced`. A `question_cancelled` by the asking session with
`outcome: "replied_in_thread"` ends its own ask as `replied_in_thread`, with
no answer; any other outcome, actor or question is a plain cancel. Answering
through the server: the same value again returns `ok` with the original,
and a different value returns `already_answered` naming the winner. A
question that is no longer live and was never answered returns
`question_ended`.
- Code: `finish/3` `lib/gentility/sessions/questions.ex:673-717`; `step/2`
  `:634-655`; `Reducer` `cards/1` to `live?/2`
  `lib/gentility/sessions/reducer.ex:264-297`, `turn_started` clause
  `:367-381`, `question_asked` `:391-415`, answered/expired/cancelled
  clause `:421-429`, `fold/2` passes `emitted_at` `:186-191`;
  `Commands.answer/3` `lib/gentility/sessions/questions/commands.ex:254-281`,
  `repeat_answer/3` `:387-399`. Desk: harness approvals refuse
  `already_decided` (`desk/core/src/reactors/asks.rs:710-743`, raised in
  `desk/core/src/api/desk.rs:6877, 6954`), and `Question::step`
  `desk/core/src/ledger.rs:849`.
- Tests: `test/gentility/sessions/questions_fold_test.exs:89` "the canonical
  first answer wins even when a losing offline answer has an older clock";
  `test/gentility/sessions/reducer_test.exs:68` "only a person turn clears
  the question and its line"; `:117` "the slot shows the newest card nothing
  has ended"; "a non-person turn keeps a question only before its expiry,
  using emitted time"; "an answer or expiry clears only the question it
  names";
  `test/gentility/sessions/question_commands_test.exs:890` "a person answers
  once and conflicting answers name the original winner and time"; Desk
  `ledger.rs:8699` `only_person_turns_end_or_overtake_a_reflex_question`,
  `the_slot_shows_the_newest_card_nothing_has_ended`.

**R7. An ask settles once.** The first answer or expiry settles an ask. A
later answer is kept on the ledger and changes nothing. An answer settles
the ask only if its word is in that kind's `decisions`. An ask lives ten
minutes. The reader that finds it past due expires it, and so does the
sweep. The core refuses a stale ask as `expired`. It refuses a stop with
nothing running as `nothing_running`, which is a harmless refusal when a
stop races a `turn_completed`.
- Code: `lib/gentility/desk/ask_reducer.ex:50-66`, `settle/3` `:87-92`;
  `@ttl_seconds 600` `lib/gentility/desk/asks.ex:37`; `open_for/3`
  `:126-139`, `expire/2` `:149-168`. Desk: `Refusal`
  `desk/core/src/reactors/asks.rs:710-743`, `word()` `:785-792`.
- Tests: `test/gentility/desk/asks_test.exs:184` "a second answer is kept and
  changes nothing"; `:215` "an ask past its ten minutes is expired by the
  reader that finds it, and not listed"; `:298` "a word its kind does not
  list settles nothing, stays on the ledger, and is warned"; Desk
  `asks.rs:1912` `the_refusal_words_are_the_declared_ones`.

**R8. A late `held` receipt cannot take back an outcome.** The latest
message receipt is what the sender last heard. The exception is `held`,
which counts only for a send that nothing has answered yet. A later
`delivered`, `refused` or `failed` does replace an earlier outcome.
- Code: `apply_event/4` for `message_receipt`
  `lib/gentility/sessions/messages.ex:58-73`; Desk `MessageView::moved`
  `desk/core/src/messages.rs:1915-1986`.
- Test: `test/gentility/sessions/messages_test.exs:398` "a held receipt heard
  after the outcome does not take it back"; `:367` "a receipt in an unseen
  status moves nothing, and cannot take back an outcome". The Desk has no
  test of its own for this rule (see [Known gaps](#known-gaps)).

**R9. An unknown kind is kept, folded only for its time.** The server stores
any fact of the right shape. A kind its fold does not know moves nothing but
`last_event_at` and the sequence. An unknown `mode` keeps the last mode the
fold could read. The Desk does not send a kind that is missing from the
server's `accepts.facts`: it parks the session until a join lists that kind.
Its own fold is total.
- Code: `@type_shape` `lib/gentility/sessions/event.ex:129-135`; catch-all
  `reducer.ex:475`; `readable_mode/2` `reducer.ex:257-259`. Desk: module doc
  `outbox.rs:43-53`, `sendable` `:4162-4178`; `apply` doc
  `ledger.rs:1120-1125`; unknown mode `ledger.rs:1197-1201`.
- Tests: `ledger_test.exs:907` "a fact from a newer app is kept and folds to
  nothing, after append and after rebuild"; `reducer_test.exs:1430` "an
  undeclared event type folds to nothing but the stamp and the sequence";
  `:560` "an unseen mode leaves the last it could read"; Desk `outbox.rs:5805`
  `a_fact_the_server_does_not_fold_parks_the_session_until_a_join_lists_it`,
  `ledger.rs:5198` `a_mode_this_build_has_not_heard_of_leaves_the_last_it_could_read`.

**R10. `last_event_at` is the fact's own time, and the bookkeeping words
never set it.** Park, message, setting, wait and subscription words never
date the row. Neither do `pr_unlinked pr_status pr_commented pr_heard
recapped status_set question_expired question_joined question_cancelled
brief_bound`, nor a model's `retitled`. The **server** stamps each other
fact's `inserted_at` in `seq` order. It has no "never backwards" guard,
because `seq` order and an `inserted_at` taken under the ledger lock already
keep it monotonic. The **Desk** receives server copies out of order, so it
refuses to move the date backwards.
- Code: `stamp/4` `reducer.ex:188-228`. Desk: end of `apply`
  `ledger.rs:1520-1534`; `stamps` `:3088-3116`.
- Tests: `reducer_test.exs:1155` "a park word leaves the time of the last
  event where it was"; `:1164` (wait); `:1172` (message); `:1247` "a model's
  retitle leaves the time of the last event where it was, and a person's
  moves it"; Desk `ledger.rs:7664`
  `a_copied_server_fact_dates_the_row_at_its_own_time_and_never_backwards`.
- A session whose server says `turn_bodies` sends its transcript as
  `turn_body` parts, not one `transcript_updated` per item (Bodies, below).
  A part is one fact and dates the row at its own time, so on the server
  `last_event_at` moves at part grain, at most `PART_EVERY` (10 s) apart
  while a turn runs, and no longer per item. The holder's own row still
  moves per item, because its transcript facts are on its log.

### Bodies

A server that says `turn_bodies` in the hello takes a turn's transcript as
**parts** (`docs/plans/2026-11-07-turn-bodies.md`). The holder then writes
its `transcript_updated` facts unnumbered, so they never reach the wire, and
ships a numbered `turn_body` fact instead: `{turn, part, complete, said?,
items}`. `items` holds each transcript item that changed since the session's
previous part, whole, as a `transcript_updated` payload with `operation:
"replace"`. Under `turn_bodies` the host numbers no
`transcript_updated` except kind `compaction`, which parts never carry: a
compaction is rare and small, and the server reads it off its log as it
draws the page. The holder cuts a part just before `turn_completed`,
`turn_aborted` or `closed` (`complete: true`). While a turn runs, it cuts one
when the oldest waiting item is `PART_EVERY` old or 256 KB wait. On the wire,
a batch stops at 1 MiB unless it is a single fact.

- **Single writer.** Only the holder appends `turn_body` (R1), and only that
  append writes the server's cold copy, `work_session_bodies`, keyed by
  `(session, turn, part)`. A replay replaces the copy with the same body.
- **R4 is unchanged.** The holder keeps only the window a part stands for
  and folds `items` from its log as it sends. That fold is deterministic,
  so a replay is identical.
- **The run stream keeps an index.** The server stores the fact with
  `count` and `bytes` in place of `items`. It projects the items into the
  run's rows as it projects `transcript_updated`. The lobby's `facts`
  read-back puts them back, so a move copies them.
- **A hollow part is taken.** A build from before parts were folded at
  send time sends a queued part as the holder keeps it, with no `items`.
  The server takes it with `count: 0`, so the queue goes on. The copy keeps
  the row with no content, which is a hole there, and reads as behind from
  the hole on. The holder still has the items.
- **R9 still holds.** A server without `turn_body` in `accepts.facts` parks
  the session behind its next part, lifecycle facts included, until a join
  lists it. Nothing is lost, because the holder keeps every fact.
- Code: `Gentility.Sessions.Bodies` (hollow parts: `copy/2`, `freshness/2`); `Event.validate_turn_body`; Desk
  `store/bodies.rs`, `Store::as_sent`.
- Tests: `test/gentility/sessions/bodies_test.exs`; the transcript golden
  case `turn_body_parts`; Desk `store::bodies::tests`.

**Both folds are one fold.** `Gentility.Sessions.Reducer` and the Desk's
`ledger::fold` both stamp fold version 8 (`@version`, `reducer.ex:119`;
`FOLD_VERSION`, `desk/core/src/ledger.rs:54`). Each folds the golden cases
in [`fold/`](fold/README.md) and asserts the manifest's `fold_version`
(`test/gentility/sessions/fold_fixtures_test.exs:800`;
`desk/core/src/ledger/fold_fixtures.rs:116`). A fold in a new client should
read those cases before it reads this file.

---

## Part 2: posture, the register

### What it holds

`inbox_set` is a **per-field last-writer-wins register** over eight fields:
`settled_at`, `snoozed_until`, `archived_at`, `rank`, `title`, `read_at`,
`waiting_on` and `waiting_reason` (`@inbox_fields`,
`lib/gentility/sessions/inbox/reducer.ex:4`). A command is
`{session_id, op_id, at, changes, by}`. `at` is the instant the writer
*claims* it acted. `by` describes the actor: `kind` is `person`, `agent`,
`desk` or the like, and the actor may also carry `session_id` and `machine`.

### Clock semantics

1. **Each field has its own clock**, `%{at, by}`, keyed by the field name.
   The one exception is `read_at`, which is keyed `{:read_at, user_id}`:
   every person has their own read clock. The server always sets `user_id`
   to the signed-in person, so nobody can write someone else's read receipt
   (`clock_key/2` `inbox/reducer.ex:79-80`, `put_field/5` `:82-85`;
   `lib/gentility/sessions/inbox.ex:300`).
2. **The claimed instant is clamped to the server's now.** A claim from the
   future is dated *now*. The fact records both `at` and `at_claimed`
   (`record_command/8`, `inbox.ex:336-337`).
3. **Each field is decided on its own.** A field is **stale** if its clock
   is strictly later than the command's `at`. Otherwise it applies and its
   clock becomes `{at, by}`. A field that has never been written always
   applies (`apply/5` `inbox.ex:712-723`, `stale?/4` `:725-727`).
4. **Ties go to the later arrival.** At an equal instant the incoming write
   applies, so ledger order breaks ties.
5. **A command can be half applied.** The reply is
   `{applied: [...], stale: [...], row_seq}` (`reply/2`, `inbox.ex:416-431`).
   A client must not assume its whole change landed. It shows what `applied`
   names and reads the rest back.
6. **`op_id` makes a command idempotent.** Sending the same `op_id` again
   returns the first outcome (and the current `row_seq`) and appends
   nothing (I2).

The consequence that matters: **a client must send the instant the person
acted, not the instant it flushed.** A phone that parks a thread at 10:00
while offline and flushes at 11:00 must send `at: 10:00`. If it does, a Desk
settle made at 10:30 still wins. If the server ordered by arrival instead,
the phone's older park would win.

### Who writes it

| Writer | `at` it claims | Path |
|---|---|---|
| Host core, its own park facts bridged off the run stream (`settled unsettled snoozed woken archived unarchived ranked retitled`) | the fact's `emitted_at` | `bridge_inbox/4` `lib/gentility/sessions.ex:544-562` (`policy: false` `:553`); `bridge_by/2` `:564-569` maps a person's `retitled` to `kind: person`, a model's to `kind: agent`, and every other fact to `kind: desk` |
| Host core, direct commands from its `inbox_outbox` (read receipts, title, rank, settle) | when the person acted | `send_inbox` `desk/core/src/outbox.rs:2618-2733` → lobby `inbox` verb |
| Every web and phone device | when the person acted | lobby `inbox` `lib/gentility_web/channels/work_sessions_channel.ex:420-439`; `archive`/`unarchive` `:726-748` |
| MCP `session_manage` settle, unsettle, snooze, wake, archive (agents, phone, web; no Desk needed) | server now | `execute/4` `lib/gentility/desk/session_manage.ex:22-36` |
| MCP `session_unarchive`, the web controller | server now | `lib/gentility/mcp/tools/session_unarchive.ex:46`; `unarchive/2` `lib/gentility_web/controllers/work_session_controller.ex:212-215`; both through `set_archived/4` `sessions.ex:786-821` |
| Agents acting on another thread | as their path | any of the above, authorized by a `manage_threads` grant (`Grants.authorize/4`, `lib/gentility/desk/grants.ex:155-219`) |
| Reflex runs (tag, settle) | the run's time | `tag/4` and `tagging/6`, `settle/3`, `lib/gentility/reflexes/actions.ex:323-430` |
| Server: snooze sweep | server now | `sweep_snoozes/1` `inbox.ex:81-120` |
| Server: implied clear of the waiting tag on a turn or answer | the cause's time, floored at the tag's clock | `clear_waiting/3` `inbox.ex:141-166` |
| Server: `keep_open_set true` clears the settle | the fact's time | `keep_open_inbox/3` `sessions.ex:1329-1348` |
| Server: the opening title | the Desk's `opened.emitted_at`, never later than server now | `seed_opened/4` `inbox.ex:252-285` |

A thread may also set its own `settled_at` and `snoozed_until`, and no other
field, without a grant. Three conditions apply: the organization's and the
person's `threads_self_manage` allow it, the caller's machine holds the
thread's lease, and the thread is not waiting on its person
(`own_posture?/4` and `itself?/3`, `grants.ex:178-197`; `@own_posture`
`:44`).

### The three non-clock overrides

The clock decides everything except these three.

1. **A person's title beats an agent's title, whatever the clocks say.**
   If the title's clock was written by a person, a write by an agent is
   stale (`stale?(:title, %{by: %{"kind" => "person"}}, _, %{"kind" => "agent"})`,
   `inbox.ex:726`). The rule runs one way only: a person still overwrites an
   agent, and agent against agent is plain LWW.
2. **Policy refusals come before the clock.** A command from anyone but the
   host's own bridge can be refused outright, and refused means nothing is
   written (`policy/4`, `inbox.ex:447-478`):
   - an archive while the thread is working is refused `session_running`;
   - a snooze while the thread has attention is refused `attention_pending`;
   - a command that manages *another* thread is refused `session_running`
     while that thread works, and `attention_pending` while it has an open
     question, a waiting tag or attention;
   - a thread settling or snoozing *itself* while it waits on its person is
     refused `attention_pending`.

   The host's bridged park facts skip policy (`policy: false`), because the
   host is the one that knows its agent's state. A lobby socket that never
   announced the `inbox` feature also skips policy on `archive`/`unarchive`
   (`socket.assigns.inbox?`, `work_sessions_channel.ex:322-332`).
3. **An implied clear is clocked no earlier than the tag it clears.** Three
   changes clear other fields without anyone asking. Settling a thread that
   is waiting on its person clears `waiting_on` and `waiting_reason` in the
   same fact. `keep_open` drops any settle. A person's turn or answer clears
   the waiting tag, but only when no question is live and the ending fact
   names the card immediately before it. Each implied field is stamped at
   the later of its cause's time and the clock of the tag it clears, so a
   Desk whose clock runs behind cannot lose to the very tag it is clearing.
   A tie applies (`implied_clocks/4` `inbox.ex:399-410`; `record_command/8`
   `:339-351`; `@clearing` `:31`, `clear_waiting/3` `:141-166`,
   `clears_waiting?/3` `:198-221`). The inbox lock is taken before the run
   append for every clearing cause, so a tag cannot race the turn that
   answers it (`lock_inbox_for_clear/2`, `sessions.ex:1352-1359`, called at
   `:533`).

### The eight rules

| # | Rule | Code | Test |
|---|---|---|---|
| I1 | Same field from two writers: LWW on the claimed `at`, clamped to server now; ties to the later arrival | `record_command/8` `inbox.ex:336-337`; `apply/5` `:712-723`; `stale?/4` `:725-727` | `test/gentility/sessions/inbox_command_test.exs:31` "commands resolve each field by clock and replay an op id without appending"; `:185` "future claims are clamped and both clocks are recorded"; `test/gentility/sessions/inbox_ledger_test.exs:19` "field clocks compare instants, with arrival breaking ties" |
| I2 | `op_id` replay returns the first outcome and appends nothing | `command/6` `inbox.ex:313-328` (under the org and inbox locks) | `inbox_command_test.exs:66` "a retry keeps its first outcome and answers the current projected cursor" |
| I3 | A person's title beats an agent's; the opening title is dated when the Desk opened the session | `stale?/4` `inbox.ex:726`; `seed_opened/4` `:252-285` | `inbox_command_test.exs:199` "a person rename resists a later model title"; `:221` "the opening title is dated when the Desk opened the session, not when the server heard"; `test/gentility/sessions/inbox_bridge_test.exs:95` "a person's bridged title resists later model naming" |
| I4 | Policy refusals (`session_running`, `attention_pending`) at the command; the host's bridge skips them | `policy/4` `inbox.ex:447-478`; `bridge_inbox/4` `sessions.ex:553` | `inbox_command_test.exs:308` "archive and snooze policy is enforced at the command; the bridge skips it"; `:341` "a manager cannot change a working or waiting thread through either server path"; `test/gentility_web/channels/desk_contract_test.exs:173` "a released core's archive never receives an inbox-only refusal" |
| I5 | Implied clears are cross-field invariants, clocked no earlier than the tag | `record_command/8` `inbox.ex:339-351`; `implied_clocks/4` `:399-410`; `clear_waiting/3` `:141-166`; `keep_open_inbox/3` `sessions.ex:1329-1348`; `lock_inbox_for_clear/2` `:1352-1359` | `test/gentility/sessions/inbox_waiting_test.exs:714` (settle clears the tag "in the same fact"); `:822` "a turn's clear is stamped at least at the tag's own clock"; `:441` "an old expiry cannot erase a later blocked tag with no live question"; `inbox_command_test.exs:123` "keep-open clears settlement and every direct command silently ignores settles until disabled"; `test/gentility/sessions/waiting_lock_order_test.exs:79` "50 rounds of a tag racing the turn that answers it" |
| I6 | Read receipts are per person: one clock per user | `clock_key/2` `inbox/reducer.ex:79-80`; `inbox.ex:300` | `inbox_command_test.exs:513` "read commands stamp the authenticated person's read clock"; `inbox_ledger_test.exs:162` "read values and clocks are independent per person" |
| I7 | The snooze sweep re-reads under the command's locks and wakes only what is still due; LWW then decides against a person's own wake | `sweep_snoozes/1` → `wake_due/2` `inbox.ex:81-120` | `test/gentility/desk/snooze_sweep_test.exs:13` "the server wakes due sessions once and leaves future snoozes alone" (this does not race a person's wake; see [Known gaps](#known-gaps)) |
| I8 | The Desk shows its own park until the server's posture is read back; fields with a command still pending are shielded from the read-back | `read_back` `desk/core/src/reactors/lobby.rs:1123`, filter `:1329-1331`, guard `:1385-1389`; `send_inbox` stale fields announced `desk/core/src/outbox.rs:2664-2670` | `lobby.rs:3648` `inbox_readback_preserves_pending_fields_even_if_acknowledged_during_the_ask`; `outbox.rs:6725` `inbox_sender_keeps_applied_fields_when_other_fields_are_stale` |

Leases sit on the same ledger, but they are not register fields. A lease
take appends `lease_taken` with epoch + 1 (`take_lease/5`, `inbox.ex:543-589`).
Every open harness question on the old machine gets a
`question_cancelled by: system` (`end_moved_questions/2`, `:591-622`). An
agent may never take a lease, grant or no grant (`not_an_agent/1`,
`:624-630`; `lease_test.exs:222` "a management grant does not let an agent
call lease_take directly").

---

## Part 3: how the Desk core differs

The Rust core runs **no clock register**. It trusts the server's.

- **Its own parks fold in local log order.** `settled`, `unsettled`,
  `snoozed`, `woken`, `archived` and `ranked` (`PARK_EVENTS`,
  `desk/core/src/store.rs:1906-1913`) apply in the order the local log
  holds them. No clock is compared (`apply`, `desk/core/src/ledger.rs:1400-1432`).
  `settled` is ignored while the row is `keep_open`, and `keep_open_set true`
  clears `settled_at` (`ledger.rs:1392-1400`). A `retitled` applies in
  arrival order, and a blank title is ignored (`ledger.rs:1274-1281`).
- **The server's word comes back as a local `inbox_heard` fact.** The
  server never sends a word called `inbox_heard`. It bumps the feed
  (`Feed.stamp_row`/`stamp_reads`, `inbox.ex:693-699`) and pushes `changed`
  on the lobby (`work_sessions_channel.ex:1246-1273`). The core then pulls
  `feed`/`list` and its lobby reactor compares the server's posture with its
  own fold (`read_back`, `lobby.rs:1123`; read receipts in `sync_feed`,
  `lobby.rs:797`). Every field that differs, except a field with a command
  still pending in `inbox_outbox`, is written as a local `inbox_heard` fact
  (`lobby.rs:1302-1337`). The fold applies `inbox_heard` in log order, like
  any other fact (`ledger.rs:1363-1389`). `inbox_heard` is `NEVER_WIRED`
  (`store.rs:1782`), so it never goes back up.
- **Pending fields are shielded in the reactor, not in the fold.** The
  reactor filters pending fields out before it writes, and refuses to write
  at all if they would overlap (`lobby.rs:1329-1331, 1385-1389`). The fold
  itself applies every field an `inbox_heard` names.
- **A refused command is told, not retried.** `send_inbox` retries only
  `unavailable`, plus a `session_running` refusal of an archive under 30 s
  old while the core's own close is still owed. Any other refusal is
  dropped, the thread that asked is told why, and a lobby sync is requested
  (`outbox.rs:2690-2733`; tests `:6868`
  `inbox_sender_retries_only_transient_or_a_close_in_progress`, `:6654`
  `inbox_refusal_names_the_thread_that_asked`).

So the two sides **differ in mechanism and converge in outcome**. The server
is the single fold of record for posture. The Desk shows its own value until
the read-back corrects it (I8). A client that moves the posture fold off the
server must carry the clocks with it. Any client that folds posture locally
must either carry them too or do what the Desk does: show its own value,
then defer to the server's read-back.

Test of the whole local sequence: `ledger.rs:3741`
`inbox_heard_and_local_actions_fold_in_sequence`.

---

## What a new client must do

1. Append to the run stream only while you hold the lease, at the epoch you
   were given, contiguously, and replay byte-identical batches (R1, R4).
   Stop at `stale_epoch`. Diverge, and do not renumber, on `sequence_gap` or
   `replay_mismatch`.
2. Never mint a server-only kind (R2). To act on a session you do not hold,
   raise an ask (R7) or send a posture command (Part 2).
3. Send posture commands with the instant the person acted, a fresh
   `op_id`, and an honest `by`. Retry with the same `op_id`. Read
   `applied` and `stale`, and read back everything else (I1, I2).
4. Treat `session_running` and `attention_pending` as final answers for that
   command, not as transient failures (I4).
5. If you fold, fold the golden cases in [`fold/`](fold/README.md) and
   honour R6, R8, R9 and R10. If you receive server copies out of order,
   keep `last_event_at` from moving backwards.

---

## Known gaps

- **`mode_changed` has no server-side writer** (T-346). Only the holding
  core's window, the agent's own report (which may only tighten the mode)
  and bring-up write it (`desk/core/src/harness/mapper.rs:1455-1458`). No
  ask kind carries a mode change. The run stream stays single-writer, so
  this is not a merge defect. But nobody off the host can change a thread's
  mode, and the mode decides whether peer messages are held.
- **I7's race is not tested.** The snooze sweep's re-check under the lock is
  in the code, but no test races it against a person's wake.
- **R8 has no Desk-side test.** `MessageView::moved` carries the rule, but
  no Rust test asserts that a `held` after `delivered` leaves it
  `delivered`.

## Corrections to R4

These are places where the 2026-11-06 report differs from master
`9c6ac2369`:

- Line drift: `stale?/4` is `inbox.ex:725-727` (was 697-717), `policy/4`
  is `:447-478`, the lease take is `:543-589`, and the Rust `ledger.rs` arms
  sit about 40 lines lower than the report says.
- `policy/4` has grown two refusals: managing another thread that is
  working or waiting, and a thread parking itself while it waits.
- A fenced `superseded` append reply does not mark the Desk row. Only
  `stale_epoch`, or a `lease_heard` naming another desk, does that (R1).
- With the `inbox` feature on, `session_archived` triggers a read-back and
  a retry, not a local hold. A sequence gap on an archived row is held only
  without `inbox` (R5).
- There is no `unknown_request` ask refusal. That word is a window API
  error code.
- The server has no `last_event_at` "never backwards" guard. Sequence order
  makes one unnecessary there (R10).
- `inbox_heard` is a Desk-local fact, not a server echo (Part 3).
