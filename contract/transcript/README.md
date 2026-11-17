# The transcript fold, recorded

The manifest's `desk_cases` are separate from its shared server `cases`.
`start.json` is recorded by the Rust `store::start_row::tests::record_start_contract`
test and read by the window's start-stage tests. Each entry is the Desk-only
row after a lifecycle fact. Its ordinal is reserved zero; the window places
it after the first prompt. The row contains timestamps, never ticking elapsed
values or terminal output. No server fold version changes for this row.

Every file here but this one is recorded by a test and read by the others.
Do not edit a fixture by hand. For the shared cases, change the Elixir test, run it
twice locally, commit what it wrote; then run the window's suite twice and
commit what it wrote. The first run of either rewrites every stale file and
fails; the second passes. On CI a stale file fails and writes nothing.
For `start.json`, change and run the Rust recorder instead, then run the
window's start-stage tests against its output.

`placement.json` is recorded from `Gentility.TranscriptPlacementCases` by
`transcript_placement_goldens_test.exs`. It holds lifecycle checkpoints read
by the Elixir database copy reader and Rust's `shared_transcript_placement_cases`:
explicit-position collisions, implicit configuration positions, late endings,
continued rows, and recorded reverts. Revert cases state the known refs-ahead
numbering difference: the holder knows its checkpoint refs and the copy uses
the ledger's step. Content and order agree. Reverts after closure write no
server fact and therefore have no copied row.

Phase 1 of the Desk distributed-data roadmap, PR B
(`docs/research/2026-10-28-desk-distributed-data-architecture.md`, cause 4:
transcript interpretation lives in five languages with no shared contract).

`<case>/facts.jsonl` is one session's events in server `seq` order, as the
server's table holds them, in the shape `desk/core/protocol/fold/` records
(one JSON object a line: `seq`, `client_seq`, `event_type`, `payload`,
`inserted_at`, `internal`). Written by
`test/gentility/transcripts/transcript_fold_fixtures_test.exs` through
`Gentility.TranscriptFixture`.

`<case>/items.json` is what those facts fold to: every `message`, `thought`
and `tool_call` the stream named, in position order, text whole — `id`,
`kind`, `role`, `content`, `message_id`, `parent_call_id` (the provider's id
of the call whose sub-agent produced the item), `position`, `turn`, and for
a tool call `tool`: `name`, `provider_tool_call_id`, `status`, `tool_kind`,
`title`, `arguments`, `result`. Written by the same Elixir test from
`Gentility.Transcripts.SessionStream`, and held by the Desk core's
projection of the same facts into its `transcript_items`, as an append
projects them and as a rebuild folds them from scratch
(`desk/core/src/store/transcript_fixtures.rs`). The synthetic rows the core
projects from lifecycle facts (`continued`, `handoff`, `head_moved`,
`turn_ended`, `plan`) are not items in these three-kind cases. The copy reader
places them from lifecycle facts and the structural index; `placement.json`
holds that separate rule.

`transcript_copy_goldens_test.exs` also reads the three-kind cases through
the actual `work_session_transcript_items` index and `agent_*` content rows.
It checks ordering, turn membership, and tool shape after the database's
documented role/tool-kind cuts. Privacy and cursor tests cover the database
boundaries separately, including a private call changing an older item's
visibility. The index is content-free; withheld items omit content, never
replace it with marker prose.

`<case>/turns.json` is what the window draws from those items: each turn's
number, prompt, text blocks, thoughts and calls (name, kind, title, status,
and a launcher's children in order). No outcome: the window's fold guesses
`end_turn` for a turn read with no live tail, and an outcome is the facts'
to say. Written by `desk/ui/src/session/transcript_fixtures.test.ts` from
`timeline.logic`'s `fold`, after it has held every item to its place. The
order of items within a turn is `items.json`'s position order.

The phone's `TranscriptFold` (`desk/mobile/ios/Kit/Transcript.swift`) reads
one case so far, `tool_call_declined`, from the checkout
(`desk/mobile/ios/KitTests/TranscriptFixtureTests.swift`). Deferred to a
follow-up: the rest of its cases (it interleaves texts and calls by position,
so it is held to `items.json` for the order and to `turns.json` for
membership), the web's `_exchanges`
(`assets/js/pages/sessions/_exchanges.ts`, whose rows carry no turn, no
`parent_call_id`, no tool title and no position, so it can be held to
`items.json` only in part), and the Rust brief cut
(`desk/core/src/api/transcript_shape.rs`). The manifest's `notes` say what
the Elixir check does not hold (the table's own doors and cuts) and what no
stream can record.

`manifest.json` names the cases, `notes` on what the fixtures mean, and
`transcript_fold_version`: the number
`Gentility.Transcripts.SessionStream.version/0` and
`store::TRANSCRIPT_FOLD_VERSION` both stamp. Each suite asserts its own
against it, so a fold change that bumps one side fails the other until it
is brought level.
