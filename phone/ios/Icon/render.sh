#!/bin/sh
# Render the iPhone icon's three appearances from the SVGs beside this script
# into App/Assets.xcassets/AppIcon.appiconset. One 1024 px PNG per
# appearance; iOS cuts every other size from it. Light and dark are flattened
# to RGB — the App Store rejects a marketing icon with an alpha channel —
# while tinted keeps its transparency, which is the point of it. Needs
# rsvg-convert and magick; commit what it writes.
set -eu

here="$(cd "$(dirname "$0")" && pwd)"
set="$here/../App/Assets.xcassets/AppIcon.appiconset"
for tool in rsvg-convert magick; do
  command -v "$tool" >/dev/null || { echo "$tool is not installed" >&2; exit 1; }
done

opaque() { rsvg-convert -w 1024 -h 1024 "$here/$1.svg" | magick - -alpha off -define png:exclude-chunks=date,time "$set/$2"; }
opaque light AppIcon.png
opaque dark  AppIcon-Dark.png
rsvg-convert -w 1024 -h 1024 "$here/tinted.svg" | magick - -define png:exclude-chunks=date,time "$set/AppIcon-Tinted.png"
