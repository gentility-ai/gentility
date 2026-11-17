#!/usr/bin/env bash
# Bind connector/phonenode — the phone's 6net node and its line to a Mac's door —
# into the xcframework the iOS app links.
#
#   phone/build-phonenode.sh
#
# Writes phone/build/Phonenode.xcframework (git-ignored, ~50 MB).
# macOS only: it needs Xcode and its command-line tools. Built from the
# connector's own module, whose tool directives pin gomobile and gobind, so the
# phone always links the connector's current overlay code. Never run it beside
# a cargo build.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
out="$root/phone/build"
tools="$out/tools"
framework="$out/Phonenode.xcframework"
version="${CONNECTOR_VERSION:-phone-$(git -C "$root" rev-parse --short HEAD)}"

mkdir -p "$tools"
cd "$root/connector"

# gomobile finds gobind on PATH, not through `go tool`; both are built at the
# versions connector/go.mod pins.
echo "==> gomobile and gobind"
GOBIN="$tools" go install golang.org/x/mobile/cmd/gomobile golang.org/x/mobile/cmd/gobind

echo "==> $framework"
rm -rf "$framework"
PATH="$tools:$PATH" gomobile bind \
  -target ios/arm64,iossimulator/arm64 \
  -iosversion 17.0 \
  -trimpath \
  -ldflags "-s -w -X github.com/gentility-ai/gentility/connector/internal/version.Connector=$version" \
  -o "$framework" \
  ./phonenode

# The app is written against these names; a bind that dropped
# one, because gobind could not export its types, is a broken build.
headers="$(find "$framework" -name 'Phonenode.objc.h' | head -1)"
if [ -z "$headers" ]; then
  echo "build-phonenode: no Phonenode.objc.h under $framework; gomobile bind wrote no header" >&2
  exit 1
fi
# -w: PhonenodeJoin must not pass on PhonenodeJoinSomething.
for symbol in PhonenodeJoin PhonenodeRejoin PhonenodeStatus PhonenodeDown \
  PhonenodeOpenDoor PhonenodeSend PhonenodeClose PhonenodeLogger PhonenodeFrameListener \
  PhonenodeDefaultRoute PhonenodePeerPath; do
  grep -qw "$symbol" "$headers" || { echo "build-phonenode: $symbol is missing from $headers" >&2; exit 1; }
done

echo "    $(du -sh "$framework" | cut -f1)  $version"
