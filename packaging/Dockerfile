# Build on the oldest supported userspace, not the maintainer's rolling distro.
FROM golang:1.26.1-bookworm AS go
FROM ubuntu:24.04 AS builder
ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates curl file build-essential pkg-config patchelf squashfs-tools \
    libgtk-3-dev libwebkit2gtk-4.1-dev libgstreamer-plugins-base1.0-dev \
    gstreamer1.0-tools gstreamer1.0-plugins-base gstreamer1.0-plugins-good \
    gstreamer1.0-pipewire librsvg2-common pulseaudio-utils \
    && rm -rf /var/lib/apt/lists/*
COPY --from=go /usr/local/go /usr/local/go
ENV PATH="/usr/local/go/bin:${PATH}"

# Versioned tools and checksums; no mutable continuous-release downloads.
RUN curl -fL --retry 3 https://github.com/linuxdeploy/linuxdeploy/releases/download/1-alpha-20251107-1/linuxdeploy-x86_64.AppImage -o /tmp/linuxdeploy \
    && echo 'c20cd71e3a4e3b80c3483cef793cda3f4e990aca14014d23c544ca3ce1270b4d  /tmp/linuxdeploy' | sha256sum -c - \
    && chmod +x /tmp/linuxdeploy && mkdir /opt/linuxdeploy \
    && cd /opt/linuxdeploy && /tmp/linuxdeploy --appimage-extract
RUN curl -fL --retry 3 https://raw.githubusercontent.com/linuxdeploy/linuxdeploy-plugin-gtk/7a3fbc31a9e5075073ff8790f26effbac5f84453/linuxdeploy-plugin-gtk.sh -o /usr/local/bin/linuxdeploy-plugin-gtk.sh \
    && echo 'b0f4cbc684a0103a9651f0955b635eaea0096b3a66c0f5a2c2aa337960375171  /usr/local/bin/linuxdeploy-plugin-gtk.sh' | sha256sum -c - \
    && chmod +x /usr/local/bin/linuxdeploy-plugin-gtk.sh
RUN curl -fL --retry 3 https://github.com/AppImage/appimagetool/releases/download/1.9.1/appimagetool-x86_64.AppImage -o /tmp/appimagetool \
    && echo 'ed4ce84f0d9caff66f50bcca6ff6f35aae54ce8135408b3fa33abfc3cb384eb0  /tmp/appimagetool' | sha256sum -c - \
    && chmod +x /tmp/appimagetool && mkdir /opt/appimagetool \
    && cd /opt/appimagetool && /tmp/appimagetool --appimage-extract
RUN curl -fL --retry 3 https://github.com/AppImage/type2-runtime/releases/download/20251108/runtime-x86_64 -o /opt/appimage-runtime \
    && echo '2fca8b443c92510f1483a883f60061ad09b46b978b2631c807cd873a47ec260d  /opt/appimage-runtime' | sha256sum -c -
# Ubuntu's PipeWire 1.0 plugin lacks provide-clock, required by portal capture.
# Build the matching client/plugin on this older glibc; host services stay external.
RUN apt-get update && apt-get install -y --no-install-recommends meson ninja-build \
    && rm -rf /var/lib/apt/lists/* \
    && curl -fL --retry 3 https://codeload.github.com/PipeWire/pipewire/tar.gz/refs/tags/1.6.8 -o /tmp/pipewire.tar.gz \
    && echo '8181172a1d95131f6af8bbc0b98f90b2a33349b042b84c3ce57dd5d11348cc58  /tmp/pipewire.tar.gz' | sha256sum -c - \
    && tar -xf /tmp/pipewire.tar.gz -C /tmp \
    && meson setup /tmp/pipewire-build /tmp/pipewire-1.6.8 \
        --prefix=/usr --libdir=lib/x86_64-linux-gnu --buildtype=release \
        --auto-features=disabled -Dgstreamer=enabled -Dsession-managers=[] \
        -Dpipewire-jack=disabled -Dpipewire-v4l2=disabled -Ddbus=disabled \
        -Dexamples=disabled -Dtests=disabled \
    && meson compile -C /tmp/pipewire-build -j 2 \
    && rm -rf /usr/lib/x86_64-linux-gnu/pipewire-0.3 /usr/lib/x86_64-linux-gnu/spa-0.2 \
    && meson install -C /tmp/pipewire-build && ldconfig
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
COPY web ./web
COPY packaging/appimage.sh packaging/AppRun packaging/screen-share.desktop packaging/screen-share.svg ./packaging/
RUN bash packaging/appimage.sh

FROM builder AS artifact

FROM ubuntu:24.04 AS smoke
RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates xvfb xauth dbus-x11 procps libgl1-mesa-dri libegl1 libgl1 libgles2 fonts-dejavu-core \
    && rm -rf /var/lib/apt/lists/*
RUN useradd --create-home tester
COPY packaging/smoke.sh /smoke.sh
USER tester
WORKDIR /tmp
ENTRYPOINT ["bash", "/smoke.sh"]
