#!/bin/zsh
set -euo pipefail
cd "${0:A:h}/../.."
FENGXIN_OUTPUT_DIR="${FENGXIN_OUTPUT_DIR:-$PWD/dist/Fengxin-Mac}"
FENGXIN_BUILD_DIR="${FENGXIN_BUILD_DIR:-$PWD/.build}"
FENGXIN_OUTPUT_DIR="${FENGXIN_OUTPUT_DIR:A}"
FENGXIN_BUILD_DIR="${FENGXIN_BUILD_DIR:A}"
FENGXIN_APP="$FENGXIN_OUTPUT_DIR/蜂信.app"
FENGXIN_CACHE="$FENGXIN_BUILD_DIR/swift-cache"
FENGXIN_ICONS="$FENGXIN_BUILD_DIR/AppIcon.iconset"
if [[ ! -x "$FENGXIN_OUTPUT_DIR/fengxin-core" || ! -x "$FENGXIN_OUTPUT_DIR/runtime/bin/python3" ]]; then
  print -u2 'Run mac/build.command first.'
  exit 1
fi
mkdir -p "$FENGXIN_APP/Contents/MacOS" "$FENGXIN_APP/Contents/Resources" "$FENGXIN_CACHE" "$FENGXIN_ICONS"
# Use a canonical absolute module cache to avoid Swift cache aliases.
env -u TMPDIR xcrun swiftc -swift-version 5 -parse-as-library -module-cache-path "$FENGXIN_CACHE" \
  -framework SwiftUI -framework AppKit -framework AVFoundation \
  mac/app/FengxinCore.swift mac/app/FengxinMac.swift -o "$FENGXIN_APP/Contents/MacOS/FengxinMac"
env -u TMPDIR xcrun swift -module-cache-path "$FENGXIN_CACHE" mac/app/Icon.swift "$FENGXIN_BUILD_DIR/AppIcon.png"
for FENGXIN_SIZE in 16 32 128 256 512; do
  sips -z "$FENGXIN_SIZE" "$FENGXIN_SIZE" "$FENGXIN_BUILD_DIR/AppIcon.png" --out "$FENGXIN_ICONS/icon_${FENGXIN_SIZE}x${FENGXIN_SIZE}.png" >/dev/null
  FENGXIN_DOUBLE=$(( FENGXIN_SIZE * 2 ))
  sips -z "$FENGXIN_DOUBLE" "$FENGXIN_DOUBLE" "$FENGXIN_BUILD_DIR/AppIcon.png" --out "$FENGXIN_ICONS/icon_${FENGXIN_SIZE}x${FENGXIN_SIZE}@2x.png" >/dev/null
done
iconutil -c icns "$FENGXIN_ICONS" -o "$FENGXIN_APP/Contents/Resources/AppIcon.icns"
cp mac/app/Info.plist "$FENGXIN_APP/Contents/Info.plist"
codesign --force --sign - --identifier com.zjx.fengxin.mac "$FENGXIN_APP"
codesign --verify --strict "$FENGXIN_APP"
print "Built $FENGXIN_APP"
