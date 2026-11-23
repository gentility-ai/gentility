# The guest executor's local protocol

The lines a loop computer's Desk core and its job executor send each other
over `/run/gentility-exec/exec.sock`: JSON, one object a line, each with a
`type` (T-642, D4; docs/plans/2026-11-24-computer-shell-asks.md, "The local
executor protocol"). Both ends ship pinned in one guest image. Nothing here
crosses the network, and a personal build speaks none of it.

`lines.json` holds one of each line the types in `desk/guest-exec/src/lib.rs`
write, and those types read each back (`cargo test -p desk_guest_exec`). A
change to a type rewrites the file off CI and fails once; commit the result.

| From | Line | Says |
| --- | --- | --- |
| core | `hello {version}` | first, on every connection |
| core | `start {job, command, cwd?, time_limit_s}` | run `bash -c <command>`; `job` is the ask's id |
| core | `cancel {job, reason}` | SIGTERM to the job's process group, its cgroup killed 5 s later; `reason` is one of `reasons` |
| executor | `hello {version, running}` | the answer to `hello` |
| executor | `started {job}` | the job's main process is running, in its own cgroup |
| executor | `output {job, stream, offset, data}` | base64 bytes of `stdout` or `stderr`, in order, the first MiB of each |
| executor | `ended {job, decision, reason?, exit_code?, signal?, stdout_total, stderr_total}` | `exited`, or `cancelled` with the cancel's reason or `time_limit`; after the job's last output |
| executor | `refused {job, reason}` | never started: `busy`, `duplicate`, `malformed`, `cwd_missing` or `not_ready` |

`reasons` is the closed list a cancel and an `ended` may give: the design's
cancel reasons, with `evaluator_refused` for any of the ownership evaluator's
refusals (the core keeps that word itself), `lease_lapsed`, and the executor's
own `time_limit`.

Anything but `hello` first, a `hello` of another version (answered with the
executor's own first), or a line that does not read, such as a reason outside
the list, closes the connection, and a closed connection ends every job.
