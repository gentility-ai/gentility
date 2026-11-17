# The session fold, recorded

Every file here but this one is written by the Elixir suite
(`test/gentility/sessions/fold_fixtures_test.exs` through
`Gentility.FoldFixture`) and folded again by both reducers: the server's
(`Gentility.Sessions.Reducer`, the same test) and the Desk core's
(`ledger::fold`, `desk/core/src/ledger/fold_fixtures.rs`). Do not edit a
fixture by hand: change the cases in that test, run it twice locally, commit
what it wrote. The first run rewrites every stale file and fails; the second
passes. On CI a stale fixture fails and writes nothing.

Phase 1 of the Desk distributed-data roadmap
(`docs/research/2026-10-28-desk-distributed-data-architecture.md`, cause 4:
the fold lives in several languages with no shared contract), and the gate
Phase 2's Slice 3 waits on (§13 of its design).

`<case>/facts.jsonl` is one session's events in server `seq` order, as the
server's table holds them, one JSON object a line: `seq`, `client_seq` (null
for a fact the server minted itself), `event_type`, `payload`, `inserted_at`
and `internal` (a machine's own fact, whose payload the server never reads;
`agent_gone` and `keep_open_set` move its row).
The Desk core reads an internal fact as a local one, an unnumbered one as
the server's, and the rest as the app's. Instants are fixed: the first fact
lands at `2026-01-01T00:00:01.000000Z` and each next one a second later.

`<case>/row.json` holds those of the thirteen row fields the server's fold
projects from those facts — `status`, `attention`, `attention_line`,
`working`, `turns`, `branch`, `last_event_at`, `pr_number`, `pr_state`,
`pr_url`, `pr_head`, `keep_open`, `mode` — whose value differs from the
manifest's `defaults`, the thirteen as the fold of no facts gives them. A
case's row is its `row.json` over `defaults`: a field the file leaves out is
held to its default by both suites, never skipped, and a null the file holds
is a value. So a new field with a default most cases share rewrites only the
cases it moves, and `defaults` changes only when the fold of no facts does,
which is a fold change: the fold version rule below applies, and on CI a
stale `defaults` fails and writes nothing, as a stale row does.
`settled_at` belongs to the inbox aggregate and left these run-fold fixtures
under Phase 2 ruling 14.14. A field whose two folds disagree,
and whose ruling is still owed, is `{"pending": "<why>", "server": <value>}`
instead of a value, written whether or not the value is the default: the
Desk's suite skips that field of that case, the
Elixir suite still holds the server's fold to `server`, and every other
field is held by both. `attention_line` is the server's alone (R2): the
Desk's suite never checks it.

Each suite also holds its fold's other fields to a list: the Elixir suite's
reducer keys, and the Desk's `ledger::Row`, destructured whole. A field
either fold grows fails there until it joins the thirteen or is named, with
why, as one these cases do not hold.

`manifest.json` names the cases, the fields, their `defaults`, `fold_version` — the number
`Gentility.Sessions.Reducer.version/0` and `ledger::FOLD_VERSION` both
stamp; each suite asserts its own against it, so a reducer change that
bumps one side fails the other until it is brought level — and `notes`:
what a field means and the rulings taken on it, beside the field, so the
next reader knows why a mark is where it is.

Before adding or changing a case, grep every reader of the words it holds.
Then run every suite that reads these cases, not only
`fold_fixtures_test.exs` (`grep -rl 'FoldFixture\|protocol/fold' test`):
`fold_parity_test.exs` lands every case on the server's projection in one
database, and refuses facts the fixture test takes. So each case needs its
own message ids, and a tool row needs its message written first.
