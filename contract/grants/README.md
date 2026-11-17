# Grants a thread passed on

Golden cases for T-584: a coordinator passes its person's manage_threads grant
over one thread on to a thread it manages. Each file is one world, recorded by
`test/gentility/desk/grant_delegation_test.exs` ("the golden cases") from the
server's own writers, and read by every reader of a passed-on grant:

- the server's `Gentility.Desk.Grants.authority/4`, which the recording test
  asserts;
- the server's fold of the facts, `Gentility.Sessions.Event.grant_decision/1`
  through `Grants.decisions/1`, which the test asserts reproduces `grants`;
- the core's replica (`replica_granted`), from the core half on.

A file holds:

- `asked`: the person, the grantee thread and the target thread;
- `authority`: `{"kind": "delegated", "by_session_id": …}` or `{"kind": "none"}`;
- `now`: the instant the answer holds at;
- `grants`: the grant feed entries as the server sends them to a core that says
  `delegated_grants`;
- `rows`: the three threads' feed rows, cut to what the rule reads;
- `facts`: every `thread_management_grant` fact on the three threads, with the
  instant the fold reads (`at`).

A passed-on line stands only while its own decision allows and has not expired;
the grant it rests on (`basis_scope`, `basis_granted_at`) is the person's own,
still in force, with that `granted_at`; and its delegator's `last_closed_at`
and `last_archived_at` are each null or before the line's `granted_at`.

Ids are numbered by first appearance. Instants keep their order and equality
but all fall within one second at microsecond precision, so a reader must
compare `basis_granted_at` with the basis's `granted_at` byte for byte, as the
server renders both: a person's decision made again in the same second already
differs (`basis_granted_again`).
