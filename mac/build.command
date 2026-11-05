#!/bin/zsh
set -euo pipefail
cd "${0:A:h}/.."
FENGXIN_BUILD_DIR="${FENGXIN_BUILD_DIR:-$PWD/.build}"
FENGXIN_OUTPUT_DIR="${FENGXIN_OUTPUT_DIR:-$PWD/dist/Fengxin-Mac}"
FENGXIN_BUILD_DIR="${FENGXIN_BUILD_DIR:A}"
FENGXIN_OUTPUT_DIR="${FENGXIN_OUTPUT_DIR:A}"
FENGXIN_GO_BINARY="${FENGXIN_GO_BINARY:-go}"
FENGXIN_PYTHON_BINARY="${FENGXIN_PYTHON_BINARY:-python3}"
mkdir -p "$FENGXIN_BUILD_DIR/tmp" "$FENGXIN_BUILD_DIR/pip-cache" \
  "$FENGXIN_OUTPUT_DIR/data" "$FENGXIN_OUTPUT_DIR/logs" \
  "${FENGXIN_OUTPUT_DIR:h}/EC20-Mac/data"
export GOCACHE="${GOCACHE:-$FENGXIN_BUILD_DIR/go-cache}"
export GOMODCACHE="${GOMODCACHE:-$FENGXIN_BUILD_DIR/go-mod}"
export GOPATH="${GOPATH:-$FENGXIN_BUILD_DIR/go-path}"
export GOTMPDIR="$FENGXIN_BUILD_DIR/tmp" TMPDIR="$FENGXIN_BUILD_DIR/tmp"
export GOTOOLCHAIN=local PIP_CACHE_DIR="$FENGXIN_BUILD_DIR/pip-cache"
"$FENGXIN_GO_BINARY" build -trimpath -o "$FENGXIN_OUTPUT_DIR/fengxin-core" ./cmd/vohive-sip
if [[ ! -x "$FENGXIN_OUTPUT_DIR/runtime/bin/python3" ]]; then
  "$FENGXIN_PYTHON_BINARY" -m venv "$FENGXIN_OUTPUT_DIR/runtime"
fi
"$FENGXIN_OUTPUT_DIR/runtime/bin/python3" -m pip install --disable-pip-version-check -r mac/requirements.txt
cp mac/vohive_mac_helper.py mac/desktop_audio.py mac/launcher.py mac/device_supervisor.py "$FENGXIN_OUTPUT_DIR/"
cp LICENSE NOTICE.md "$FENGXIN_OUTPUT_DIR/"
cp mac/README.md "$FENGXIN_OUTPUT_DIR/README.md"
"$FENGXIN_OUTPUT_DIR/runtime/bin/python3" -c 'import usb.core, libusb_package, sounddevice'
print "Core and local Python environment built in $FENGXIN_OUTPUT_DIR"
print "Next: mac/app/build-app.command"
