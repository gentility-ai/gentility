# Gentility clients

The software Gentility runs on a person's own machines, in the open so it can
be read before it is trusted.

- **`connector/`**: the Connector, one Go binary. It joins a machine to its
  organisation's private network (6net, built on a fork of Tailscale), serves
  what the organisation grants through it, and is the command line for tasks.
  The same binary runs Gentility's network gateway. See
  [connector/README.md](connector/README.md).
- **`phone/`**: the iPhone app. It reaches a person's Desk on their Mac over
  6net, through `Phonenode`, a framework bound from `connector/phonenode/`.
  See [phone/README.md](phone/README.md).
- **`contract/`**: the published wire contract between Gentility's server,
  the Mac's Desk, the Connector and the phone. Every reader here reads this
  one copy in place. See [contract/README.md](contract/README.md).

## Building and testing

The Connector needs the Go version `connector/go.mod` names:

```sh
cd connector
go test ./...
go build ./cmd/connector
```

The phone needs macOS, Xcode and XcodeGen:

```sh
phone/build-phonenode.sh
cd phone/ios && xcodegen generate
```

then the commands in [phone/README.md](phone/README.md).

`bin/phone-readers` and `bin/phone-unknown-words` check the phone's readers
against the contract, and run on Linux too.

## Releases

A Connector release is a tag, `connector/v<major>.<minor>.<patch>`. Its
GitHub release carries the Linux (`amd64`) and macOS (`arm64`) binaries,
unsigned, with `SHA256SUMS` and `client-contract.tgz`. A release's bytes can
be rebuilt from its commit:

```sh
bin/connector-build linux/amd64 <version> <commit> out
sha256sum out/connector-linux-amd64
```

and compared with `SHA256SUMS`. Every pull request checks that two builds of
one commit give the same bytes.

A published contract is tagged `contract-v<n>`.

## Contributing

Issues are welcome. Pull requests are open to collaborators for now.

## Licence

[Apache License 2.0](LICENSE).
