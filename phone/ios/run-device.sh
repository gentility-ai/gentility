#!/bin/zsh
# Build DeskPhone for a plugged-in iPhone, install it and launch it.
#
#   phone/ios/run-device.sh <device-id>
# Find a device with: xcrun devicectl list devices
#
# The phone must be unlocked for the launch step; devicectl can install to a
# locked phone but cannot bring an app to the front on one.
set -euo pipefail

DEV="${1:?Usage: run-device.sh <device-id>}"
here="${0:A:h}"
dd="$here/../build/dd-device"
app="$dd/Build/Products/Debug-iphoneos/DeskPhone.app"

# The project is generated, and git-ignored: a checkout older than its
# files would build without the ones added since.
(cd "$here" && xcodegen generate --quiet)

xcodebuild -project "$here/DeskPhone.xcodeproj" -scheme DeskPhone -configuration Debug \
  -destination "id=$DEV" -derivedDataPath "$dd" -allowProvisioningUpdates build

# devicectl drops the connection now and then mid-install; one retry is
# enough in practice.
install() { xcrun devicectl device install app --device "$DEV" "$app"; }
if ! out="$(install 2>&1)"; then
  print -r -- "$out"
  if [[ "$out" == *"connection"* ]]; then
    echo "==> install retry"
    install
  else
    exit 1
  fi
else
  print -r -- "$out"
fi

xcrun devicectl device process launch --device "$DEV" --terminate-existing ai.gentility.desk.phone
