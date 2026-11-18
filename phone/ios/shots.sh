#!/usr/bin/env bash
# Showcase screens as contact sheets, for a pull request's before and after.
#
#   phone/ios/shots.sh <simulator-id> <out-dir> [screen ...]
#
# Builds the Debug app for the simulator, draws each screen in dark and in
# light (DESK_PHONE_SHOWCASE), and lays them four to a sheet with ImageMagick's
# `montage`. With no screens named it draws the conversation set.
set -euo pipefail

sim="${1:?simulator id}"; out="${2:?output directory}"; shift 2
screens=("$@")
if [ ${#screens[@]} -eq 0 ]; then
  screens=(conversation idle tools tools-running approval question-tall queued strip-scrolled transcript-markdown transcript-images attach-tray ended)
fi

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$here"
xcodegen generate >/dev/null
xcodebuild build -quiet -project DeskPhone.xcodeproj -scheme DeskPhone -configuration Debug \
  -destination "platform=iOS Simulator,id=$sim" -derivedDataPath ../build/dd-sim \
  CODE_SIGN_IDENTITY=- DEVELOPMENT_TEAM=
xcrun simctl boot "$sim" 2>/dev/null || true
xcrun simctl install "$sim" ../build/dd-sim/Build/Products/Debug-iphonesimulator/DeskPhone.app

mkdir -p "$out"
for look in dark light; do
  xcrun simctl ui "$sim" appearance "$look"
  for screen in "${screens[@]}"; do
    # `simctl launch` can outlive the launch it made; give it three seconds.
    SIMCTL_CHILD_DESK_PHONE_SHOWCASE="$screen" xcrun simctl launch --terminate-running-process "$sim" ai.gentility.desk.phone >/dev/null 2>&1 &
    launch=$!
    sleep 3
    kill "$launch" 2>/dev/null || true
    xcrun simctl io "$sim" screenshot "$out/$screen-$look.png" >/dev/null 2>&1
  done
  files=()
  for screen in "${screens[@]}"; do files+=("$out/$screen-$look.png"); done
  montage "${files[@]}" -resize 480x -tile 4x -geometry +6+6 -background '#444' "$out/sheet-$look.png"
done
ls "$out"/sheet-*.png
