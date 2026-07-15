set shell := ["bash", "-euo", "pipefail", "-c"]

ffmpeg_dev := env_var_or_default("FFMPEG_DEV", "/nix/store/i670hqnvklnjbjiicnm712nrcf6nnn0h-ffmpeg-8.1.2-dev")
ffmpeg_lib := env_var_or_default("FFMPEG_LIB", "/nix/store/9zydfg5hfbqg8ixiy6r3q875wjr3szyy-ffmpeg-8.1.2-lib")

_export := "export PKG_CONFIG_PATH='" + ffmpeg_dev + "/lib/pkgconfig'${PKG_CONFIG_PATH:+:$PKG_CONFIG_PATH}; export LD_LIBRARY_PATH='" + ffmpeg_lib + "/lib'${LD_LIBRARY_PATH:+:$LD_LIBRARY_PATH}; export CGO_LDFLAGS='-Wl,--disable-new-dtags -Wl,-rpath," + ffmpeg_lib + "/lib'${CGO_LDFLAGS:+ $CGO_LDFLAGS};"

build:
    {{_export}} go build -o transcode-server ./cmd/transcode-server

test:
    {{_export}} go test ./... -count=1

test-race:
    {{_export}} go test -race ./... -count=1

run *args:
    {{_export}} go run ./cmd/transcode-server {{args}}

# Run a real VAAPI segment transcode. Override INPUT, DEVICE, or OUTPUT as needed.
vaapi-smoke INPUT="/home/bhunter/Downloads/Lucky 2026 S01E01 1080p 10bit WEBRip 6CH x265 HEVC-PSA.mkv" DEVICE="/dev/dri/renderD128" OUTPUT="/tmp/media-transcoder-vaapi.ts":
    {{_export}} INPUT='{{INPUT}}' DEVICE='{{DEVICE}}' OUTPUT='{{OUTPUT}}' go test -run TestVAAPIRealInput -count=1 -v .
