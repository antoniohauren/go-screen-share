#!/usr/bin/env bash
set -euo pipefail
# This image has neither WebKitGTK nor GStreamer installed.
"/tmp/screen share.AppImage" --appimage-extract > /dev/null
mv squashfs-root "relocated app"
cd "relocated app"
./AppRun --check-runtime
export SCREENSHARE_URL=https://share.example.com
export LIBGL_ALWAYS_SOFTWARE=1
dbus-run-session -- xvfb-run -a "/tmp/screen share.AppImage" --appimage-extract-and-run > /tmp/ui.log 2>&1 &
pid=$!
trap 'kill "$pid" 2>/dev/null || true' EXIT
sleep 15
cat /tmp/ui.log
kill -0 "$pid"
pgrep -x screen-share
pgrep -x WebKitWebProces
