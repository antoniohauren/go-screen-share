#!/usr/bin/env bash
set -euo pipefail
appdir=/tmp/ScreenShare.AppDir
lib=/usr/lib/x86_64-linux-gnu
mkdir -p "$appdir/usr/bin" "$appdir/usr/lib/gstreamer-1.0" "$appdir/usr/share" /out
go build -trimpath -tags production,webkit2_41 -ldflags='-s -w' \
    -o "$appdir/usr/bin/screen-share" ./cmd/sharer

# Plugins and helpers are loaded dynamically, so ELF dependency scanning alone
# cannot discover them. Keep the exact capture/encoding pipeline plus test sources.
for plugin in app coreelements videoconvertscale videorate vpx rtp audioconvert \
    audioresample opus pulseaudio pipewire rawparse videotestsrc audiotestsrc; do
    cp "$lib/gstreamer-1.0/libgst$plugin.so" "$appdir/usr/lib/gstreamer-1.0/"
done
cp /usr/bin/gst-inspect-1.0 /usr/bin/gst-launch-1.0 "$appdir/usr/bin/"
cp /usr/bin/pactl /usr/bin/parec "$appdir/usr/bin/"
mkdir -p "$appdir/usr/lib/x86_64-linux-gnu"
cp -a "$lib/webkit2gtk-4.1" "$appdir/usr/lib/x86_64-linux-gnu/"
cp -a "$lib/pipewire-0.3" "$lib/spa-0.2" "$appdir/usr/lib/"
cp -a /usr/share/pipewire "$appdir/usr/share/"
mkdir -p "$appdir/etc" "$appdir/usr/lib/gio"
cp -Lr /etc/fonts "$appdir/etc/"
cp -a "$lib/gio/modules" "$appdir/usr/lib/gio/"
cp "$lib/gstreamer1.0/gstreamer-1.0/gst-plugin-scanner" "$appdir/usr/bin/"
# linuxdeploy normally assumes these desktop libraries are host-installed.
# Bundle them explicitly; only libc and the host graphics/driver stack stay external.
libraries=()
for library in libpipewire-0.3.so.0 libharfbuzz.so.0 libfreetype.so.6 \
    libfontconfig.so.1 libexpat.so.1 libfribidi.so.0 libgpg-error.so.0 \
    libcom_err.so.2 libgmp.so.10 libasound.so.2; do
    libraries+=(--library "$lib/$library")
done
DEPLOY_GTK_VERSION=3 /opt/linuxdeploy/squashfs-root/AppRun \
    --appdir "$appdir" --executable "$appdir/usr/bin/screen-share" \
    --executable "$appdir/usr/bin/pactl" --executable "$appdir/usr/bin/parec" \
    --deploy-deps-only "$appdir/usr/lib" \
    "${libraries[@]}" \
    --desktop-file packaging/screen-share.desktop --icon-file packaging/screen-share.svg \
    --plugin gtk --custom-apprun packaging/AppRun
# Own the launcher: linuxdeploy's wrapper forces X11 and sources hooks twice.
cp packaging/AppRun "$appdir/AppRun"
rm -f "$appdir/AppRun.wrapped"
chmod +x "$appdir/AppRun"
# Release WebKit ignores WEBKIT_EXEC_PATH. Like Tauri's GTK AppImage packaging,
# relocate its compiled-in helper/bundle prefix with an equal-length replacement.
# AppRun sets cwd to usr so these paths work at any mount/extraction location.
sed -i 's|/usr/lib/x86_64-linux-gnu/webkit2gtk-4\.1|././/lib/x86_64-linux-gnu/webkit2gtk-4.1|g' \
    "$appdir/usr/lib/libwebkit2gtk-4.1.so.0"
ARCH=x86_64 /opt/appimagetool/squashfs-root/AppRun \
    --runtime-file /opt/appimage-runtime "$appdir" /out/screen-share-linux-x86_64.AppImage
