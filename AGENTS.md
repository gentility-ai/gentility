# AGENTS.md

Guidance for agents working in this repository. It is public: everything you
commit, and every commit's message and author, can be read by anyone.

## Cares

- Clean, maintainable, readable code: parsimonious, no unnecessary frills or
  abstractions; reject complexity until it is strictly required.
- Architecture as simple as it can be, but no simpler.
- Security, reproducibility, legibility. People run this code on their own
  machines because they can read it.
- Idiomatic Go and Swift, within reason. Well named, well laid out.

## What is here

- `connector/`: the Connector, a Go module
  (`github.com/gentility-ai/gentility/connector`). `cmd/connector` is the
  binary; `phonenode/` is the phone's network node and its line to a Mac's
  door, bound for iOS by `phone/build-phonenode.sh`; `cmd/clientcontract`
  writes `contract/client/`.
- `phone/`: the iPhone app. `phone/ios/project.yml` is the XcodeGen project;
  `Kit/` is its testable core and `KitTests/` its tests.
- `contract/`: the wire contract, one copy, read in place by everything here.
- `bin/`: the checks and tools named below.

## The contract

- `contract/` outside `contract/client/` is the server's. It arrives only by
  the publishing pull request, from a `contract-publish/` branch, and that
  pull request changes nothing else. `bin/publisher-scope` enforces both in
  `checks.yml`. To change one of those files, change its source in the
  server's repository.
- `contract/client/` is this repository's: what the Connector's command line
  and status file promise their readers. It is generated: after a change to
  what it describes, run `go run ./cmd/clientcontract --out <empty dir>` in
  `connector/` and copy the result over `contract/client/`. CI regenerates it
  and fails on any difference.
- Tests read the contract through `connector/internal/contracttest` and the
  phone's build step, never a copy. `GENTILITY_CONTRACT_DIR` may point them at
  a local build during joint development; CI refuses it.

## Rules

- **No secret, ever.** No credential, key or token in the tree. Workflows
  build and test; nothing here signs, notarizes or deploys. A pull request
  here must have nothing to steal.
- **No workflow uses `pull_request_target` or `workflow_run`, and none is
  given a secret.** Every job a pull request starts has `contents: read`.
  Only two jobs hold a write token, and neither runs on a pull request:
  `contract-tag.yml` on a push to `main`, and `release-connector.yml` on a
  release tag. `checks.yml` refuses the two triggers and any `secrets.`
  reference.
- **Nothing private.** `bin/contract-check` runs the general rules over the
  tree in CI: no email address or host outside its allow-list, no home
  directory but `/Users/someone`, no private address, device or signing team
  outside the reviewed exceptions in `bin/contract-check-allow.json`, no link
  that leaves the tree. Use invented names in tests and examples.
- **Gentility's own clones also run its private deny check** before every
  push and merge (`bin/private-check`). In each clone on Gentility's
  machines, once:

  ```sh
  git config core.hooksPath .githooks
  git config gentility.denyCheck <gentility-core checkout>/bin/client-deny-check
  ```

  On a machine that runs Desk, `bin/merge-pr` and the hook refuse until the
  check is configured. Outside Gentility, set
  `git config gentility.denyCheck none`.
- **Comments cite what a reader can open:** a file here, a contract file, a
  public document. Otherwise, say the thing itself.
- **Commits are signed, and authored with a GitHub `noreply` address,** never
  a personal mail address.
- **Errors are handled, not hidden.** No panics for expected failures in Go,
  no force unwraps on input in Swift.
- **Releases never move.** A `connector/v*` or `contract-v*` tag is never
  moved or deleted; a bad release is followed by a higher one.

## Commands

```sh
cd connector && go test ./... && go vet ./...   # the Connector
test -z "$(gofmt -l connector)"                  # formatting
bin/phone-readers && bin/phone-unknown-words     # the phone's readers, any OS
bin/contract-check --source --exclude contract . # nothing private
contract/tools/verify contract                   # the contract whole
bin/connector-build linux/amd64 <version> <commit> out  # a release build
```

The phone's `KitTests` need macOS and a simulator; see `phone/README.md`.
`connector/internal/tsnode`'s interface test needs root
(`go test -tags interface_root`). The enrolment integration test
(`-tags overlay_integration`) and the live tool-schema test (`-tags livemcp`)
need Gentility's server running in development, and do not run here.

## Pull requests

- One change per pull request, each from `main`; never stacked.
- Branches and titles describe the change, never the tool that made it.
- Land with `bin/merge-pr <number>`, not `gh pr merge`. It merges once every
  check and workflow on the head has passed and the sign-offs the change
  needs are on that head.
- **Architecture sign-off** is needed for what this repository publishes and
  for the checks that guard everything else, so that no pull request weakens
  a check it would fail: `contract/client/`, `connector/cmd/clientcontract/`,
  `connector/phonenode/` (the phone's API), the status file
  (`connector/internal/status/`, `connector/internal/state/status.go`),
  `.github/`, `.githooks/`, `bin/merge-pr`, `bin/sign-off`,
  `bin/publisher-scope`, `bin/private-check`, `bin/contract-check` and
  `bin/contract-check-allow.json`.
  **UI sign-off**, with screenshots in the pull request, for `phone/ios/App/`.
  The reviewer records it with `bin/sign-off`; the author never signs for the
  reviewer. Any new commit needs it again.
- Push only to your own branches, and never force-push another's.
