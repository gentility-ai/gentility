# Window confirmation readings

`confirmation-old-page.json` records real loopback replies to a page that did
not negotiate `machine_confirmation`. Reads and changes retain the existing
`scope` refusal, with no preceding waiting event or new refusal code.

The producer is `confirmation_old_page_keeps_the_existing_refusal` in
`core/tests/pairing.rs`. It regenerates a stale recording outside CI and fails
that run so the diff must be inspected. The page reader is the UI API client's test suite;
run both whenever the recording changes. These are application frames, not
the native shell frames under `shell/`.
