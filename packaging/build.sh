#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."

target=${1:?Usage: bash packaging/build.sh windows|linux OUTPUT_DIRECTORY}
output=${2:?Output directory required}
mkdir -p "$output"
output=$(realpath "$output")

case "$target" in
    windows)
        CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -trimpath \
            -tags production -ldflags='-s -w -H windowsgui' \
            -o "$output/screen-share-windows-amd64.exe" ./cmd/sharer
        ;;
    linux)
        docker build --platform linux/amd64 --target artifact \
            -t screen-share-packaging -f packaging/Dockerfile .
        temporary=$(mktemp "$output/.AppImage.XXXXXX")
        trap 'rm -f "$temporary"' EXIT
        docker run --rm --platform linux/amd64 --entrypoint cat \
            screen-share-packaging /out/screen-share-linux-x86_64.AppImage > "$temporary"
        chmod 755 "$temporary"
        mv "$temporary" "$output/screen-share-linux-x86_64.AppImage"
        ;;
    *) printf 'Unknown target: %s\n' "$target" >&2; exit 1 ;;
esac
