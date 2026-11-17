# A change of mode or model, recorded

The transcript draws a line where the session's mode or model changed
between two turns ("Mode changed from Supervised to Auto-accept edits",
"Model changed from X to Y"). Two readers decide where: the Desk core's
`ledger::config_change`, which the store projects each line from, and the
server's `Gentility.Sessions.config_changes/2`, which the web session page
is handed. These cases hold the two to one rule.

Every directory here is one case, written by the Elixir suite
(`test/gentility/sessions/config_changes_test.exs`) from the table in that
test. Do not edit a case by hand: change the table, run the test twice
locally, commit what it wrote. The first run rewrites every stale file and
fails; the second passes. On CI a stale case fails and writes nothing.

`<case>/facts.jsonl` is one session's events in server `seq` order, in the
shape the fold cases share (see `../fold/README.md`): `seq`, `client_seq`,
`event_type`, `payload`, `inserted_at` and `internal`, one JSON object a
line, every visible fact one the server's door takes. The first fact is
always the `opened`.

`<case>/expected.json` is every change the facts make, oldest first: `seq`
(the fact's, in `facts.jsonl`), `kind` (`mode` or `model`), `from`, `to`,
`turn` (the session's count of turns so far, a checkpoint revert counting as
one) and, for a model, the `harness` it is named by. It is stated in the
test's table, not read off either reader, so both are held to it:

- the Desk core asks `config_change` of each fact against the facts before
  it, the whole log and only `CONFIG_CHANGE_WORDS` of it alike
  (`config_change_cases` in `desk/core/src/ledger.rs`);
- the server opens a session on the case's `opened`, appends the rest as a
  Desk sends them, an internal one marked internal on the wire, and reads
  `config_changes/2`, which carries no `harness`. Both read an internal fact
  as any other, as the Desk reads its log whatever each fact's origin.

These are not fold cases. They are not in `../fold/manifest.json`, and the
fold suites never read them.
