set shell := ["bash", "-euo", "pipefail", "-c"]

_ffmpeg_env := '''
if ! pkg-config --exists libavformat libavcodec libavutil libavfilter; then
  if [[ -n "${FFMPEG_DEV:-}" ]]; then
    export PKG_CONFIG_PATH="$FFMPEG_DEV/lib/pkgconfig${PKG_CONFIG_PATH:+:$PKG_CONFIG_PATH}"
  elif [[ -d /nix/store ]]; then
    ffmpeg_pc="$(find /nix/store -maxdepth 4 -path '*/lib/pkgconfig/libavcodec.pc' -print 2>/dev/null | sort -V | tail -n1)"
    if [[ -n "$ffmpeg_pc" ]]; then
      export PKG_CONFIG_PATH="$(dirname "$ffmpeg_pc")${PKG_CONFIG_PATH:+:$PKG_CONFIG_PATH}"
    fi
  fi
fi
pkg-config --exists libavformat libavcodec libavutil libavfilter || {
  echo 'FFmpeg development libraries were not found. Install them or set FFMPEG_DEV.' >&2
  exit 1
}
ffmpeg_libdir="${FFMPEG_LIB:-$(pkg-config --variable=libdir libavcodec)}"
export LD_LIBRARY_PATH="$ffmpeg_libdir${LD_LIBRARY_PATH:+:$LD_LIBRARY_PATH}"
export CGO_LDFLAGS="-Wl,--disable-new-dtags -Wl,-rpath,$ffmpeg_libdir${CGO_LDFLAGS:+ $CGO_LDFLAGS}"
'''

build:
    @{{_ffmpeg_env}} go build -o transcode-server ./cmd/transcode-server

test:
    @{{_ffmpeg_env}} go test ./... -count=1

test-race:
    @{{_ffmpeg_env}} go test -race ./... -count=1

run *args:
    @{{_ffmpeg_env}} go run ./cmd/transcode-server {{args}}
