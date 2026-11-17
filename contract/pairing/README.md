# Browser admission

`admission.json` records the opt-in loopback carrier's type-tagged frames,
before normal request traffic. Rust checks outgoing serialization and incoming
decoding in `pairing::renewal::tests`; the browser transport tests consume the
offer, ready and refusal records and compare its acceptance with the recording.
Renewal offers name the presented field they matched (`secret` or `pending`).
First-link offers omit it. The browser refuses a missing selector on renewal
or a selector naming a field it did not present; the unpresented-pending
record is a negative reader case, not a frame the core emits for that request.

All identifiers and secrets here are inert fixture labels. Recovery case names
describe the store state in the corresponding behavioral tests; the JSON alone
does not establish recovery, transactionality or revocation.

The existing protocol minor remains frozen. Legacy `/ws?token=` remains until
a separately reviewed protocol-major-2 removal. Changes here require the
Architecture Manager's sign-off at the pull request head.
