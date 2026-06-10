#!/usr/bin/env bash
set -euo pipefail
: "${FFMPEG_PREFIX:?set FFMPEG_PREFIX to your FFmpeg shared build}"
export PKG_CONFIG_PATH="$FFMPEG_PREFIX/lib/pkgconfig${PKG_CONFIG_PATH:+:$PKG_CONFIG_PATH}"
export CGO_LDFLAGS="-Wl,--disable-new-dtags -Wl,-rpath,$FFMPEG_PREFIX/lib ${CGO_LDFLAGS:-}"
go build -o transcode-server ./cmd/transcode-server
