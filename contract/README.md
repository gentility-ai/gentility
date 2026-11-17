# The Desk contract

Plain files describing the agreement between the server, core, window and
phone. Readers use these paths in place; publishing copies them without
moving their source. Recorded examples use invented identities.

| Part | Owner | Protection |
|---|---|---|
| `server/` | server | Recorded exchanges checked against the server schema and read by the core and phone |
| `fold/` | server | Golden session facts and rows replayed by both reducers; versions must agree |
| `transcript/` facts and items | server | Golden transcript cases replayed by server and core |
| `transcript/*/turns.json`, `transcript/start.json` | core | Recorded lifecycle/turn shapes checked by the window |
| `grants/` | server | Golden grants passed on by a thread, read by the server's authority and fold and the core's replica |
| `config_change/` | server | Recorded configuration changes checked by their readers |
| `server_tools.json`, `repository_forms.json` | server | Tool and repository vocabulary shared with core readers |
| `tolerated_words.json` | server | Server tests decide which unfamiliar words it tolerates hearing |
| `merge-rules.md` | server | Admission and merge rules cite their implementation and tests |
| `released.json` | core | Window protocol compatibility tests prevent removal within a major |
| `door/` | core | Real core frames recorded in the phone's transport envelopes |

Read [the server contract](server/README.md), [the session fold](fold/README.md),
[the transcript fold](transcript/README.md), and [merge rules](merge-rules.md)
before implementing a reader. Recordings are regenerated from their inputs,
never edited by hand. A stale recording fails its owning test; locally the
test rewrites it, and a second run must pass. CI compares without writing.

In `server/facts.json`, `golden_case_kinds` includes core-local and deliberately
unknown kinds exercised by the golden cases; it is not a subset of `kinds`.

## Door recordings

`door/` records the frames the phone reads: replies are `{id, response}` and
events are `{event}`. The Rust integration test
`records_the_frames_a_phone_reads_through_a_door` uses the core's own values
and transport envelopes. It normalizes identities, instants, hashes, timings,
paths, builds and machine names. Only moments the phone interprets are
recorded: text, tool calls, turn endings, heard, handoff, continued and lagged.
Permission questions and answers, prompt queue actions, forms, links and
refusals are recorded too. These are examples of supported exchanges, not a
complete transcript of every event the core can emit.

## Compatibility and publication

The server contract only grows under its compatibility check. The window
protocol preserves released shapes within a major; feature words gate new
behavior where an old reader would silently ignore it. Unknown reply words
must have a safe fallback; a reader must not forward an unknown word.
Fold and transcript-fold versions belong to their respective manifests.
A version bump must preserve the previous published golden cases or name
the reader floor that ends them. The publication number represents order
only, not compatibility, and consumers include it in their build string.

The private source repository's `bin/contract-check` checks publication inputs.
Its deny-list stays outside this directory. Publisher metadata and consumer
tools accompany the built artifact; the source fixtures remain here.

## Using this artifact

This directory is the single contract/ copy in the public repository.
All consumers read it in place. Run tools/verify in CI to check every file
against the manifest without network access. During joint development,
GENTILITY_CONTRACT_DIR names a local build; consumers must refuse this override
in CI. Include VERSION in the consumer's build string.

The source repository's bin/contract-build --number <number> --out <directory>
builds HEAD by default; --revision names another commit. Commit local source
changes before building them. The same commit and number produce the same
bytes. shared/instructions.md is excluded pending publication approval.
manifest.json inventories every other file, its owner, mode and SHA-256;
it cannot hash itself. Publishing permissions are enforced by repository CI,
not by this offline integrity check.
