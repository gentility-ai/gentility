# Shell confirmation carrier

Core-owned frames for the same-user shell attachment and the native helper's
private pipes. These are not pairing admission or application Request frames.
The shared Rust reader lives in `core/src/confirmation/wire.rs`; core, shell
and helper use it. Its golden test reads every line in `confirmation.json`.

Admission is limited to 16 KiB and five seconds after choosing the shell role.
The deadline never applies to ordinary control-socket CLI connections.
`bundle_path` is recorded here, not consumed or trusted as an executable.
The native renderer answers only its live confirmation ID. It acknowledges
closure even after cancellation; an Allow after cancellation executes nothing.

No bearer, pairing URL or renewal secret crosses these frames. Debug output
redacts prompt contents, including pairing names and phone codes.

## Shell models

`models.json` pins Step 3's additional frames. They are enabled only for an
attachment accepting `shell_models`, after its shell read the feature in the
current control hello. A reconnect repeats that negotiation. Unknown wants
are ignored; old shells receive no new frames. An old core's strict attach
reader refuses `wants`, so the new shell must omit it for such a core.

Attention, status and machines are independent full replacements, each held
in one latest-wins slot. Every accepted attachment receives all three current
models. Confirmation prompts and cancellation go first. Notices are transient,
bounded to 32 queued items with the oldest dropped and drops counted; they
are never replayed on reconnect. All frames stay within 16 KiB without chunks.
Machine groups are bounded by the core; `more` is the omitted count and opens
the Machines page, never a machine named by the page.

Notice IDs are random opaque values scoped to one attachment, retained for
at most 24 hours and at most 64 notices. The core rechecks the thread's
existence and visibility before answering a click with `open_thread`.
An unknown or expired notice only produces `show_window`. No click can carry
a path, request, or thread identifier into the core. Debug output excludes
notice text and machine addresses, as it excludes confirmation text.

## Run navigation

A shell whose bundled page supports run navigation may additionally offer
`shell_run_navigation`. The core accepts it only together with `shell_models`.
Only such attachments receive `open_run` with `session_id` and `environment_id`;
all other attachments keep `show_window` for run notices. Ordinary threads
continue using `open_thread`. No page request protocol feature is added.

The current person-scoped visibility read supplies the run's environment,
not the notice ID table. The native shell emits a run navigation event; the
page refreshes that environment's run list and opens the matching RunThread.
A removed run leaves the threads view open without an error toast. Reconnect
discards all notice IDs and repeats capability negotiation.
