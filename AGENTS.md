# AGENTS.md

## Build

Requires Go 1.26, CGO, and FFmpeg 8.1+ development libraries for:

- `libavformat`
- `libavcodec`
- `libavutil`
- `libavfilter`

Use the repository recipes. They prefer the active `pkg-config` environment and, on Nix systems, discover FFmpeg development outputs from `/nix/store` without hardcoded store hashes. `FFMPEG_DEV` and `FFMPEG_LIB` remain optional overrides.

```bash
just build
```

The binary is written to `./transcode-server`.

## Validation

Run the narrowest relevant test first, then the full checks before committing:

```bash
just test
just test-race
just build
git diff --check
```

Remove the generated binary after validation when it is not part of the change:

```bash
rm -f transcode-server
```

Optional integration tests may require external media, FFmpeg CLI, or hardware devices. Do not claim they passed unless they were explicitly run.

## Repository layout

| Path | Purpose |
| --- | --- |
| root package | Direct libav/cgo transcoding, probing, AVIO callbacks, shared types |
| `ffmpeg_bridge.c` | libav implementation and custom AVIO integration |
| `server/` | HTTP API, HLS/DASH sessions, library routing, rclone VFS integration |
| `cmd/transcode-server/` | CLI entry point |
| `third_party/rclone/` | Pinned rclone fork submodule |
| `transcoder.example.yaml` | Valid production-oriented configuration example |
| `testdata/sample.mp4` | Small media fixture used by tests |

## Architecture

- Do not spawn `ffmpeg` or `ffprobe` for normal transcoding. The server calls libav directly through cgo.
- Library media is opened through rclone `fs` + VFS, including local paths.
- rclone VFS streams into libav through a custom `AVIOContext`; do not reintroduce full-file materialization.
- Every demuxer open gets an independent VFS handle. Never share seek position between concurrent jobs.
- HLS and DASH manifests are generated dynamically. Media segments are generated on demand and cached.
- Session cleanup must wait for active workers before deleting AVIO registrations or shutting down VFS instances.

## Configuration

The YAML file contains `server`, `libraries`, and `profiles`.

Libraries use:

```yaml
libraries:
  movies:
    vfs: "gdrive:Movies"
    options:
      vfs_cache_mode: "full"
      vfs_cache_max_size: "250GiB"
      vfs_read_ahead: "64MiB"
```

`vfs` may be a local path, named rclone remote, or connection string. Secret-bearing configurations may use `encoded_config`.

Keep caches separate:

```yaml
server:
  cache_root: "/var/cache/media-transcoder/segments"
  vfs_cache_root: "/var/cache/media-transcoder/vfs"
```

- `cache_root`: generated HLS/DASH manifests and segments
- `vfs_cache_root`: global rclone VFS cache directory

Bitrates accept numeric bits per second or readable values such as `128kbps`, `2.8Mbps`, and `1Gbit/s`. Rclone size and duration options use rclone-native values such as `64MiB`, `250GiB`, and `24h`.

Keep `transcoder.example.yaml` loadable. `TestExampleConfigLoads` protects it.

## Web UI

The root route serves a Go-template library browser. Keep it build-step-free: Tailwind browser CDN, Plyr, and hls.js are loaded from CDN.

```text
GET /
GET /ui/library/{library}/{relative-directory...}
GET /ui/player/{library}/{relative-media-path...}
GET /media/{library}/{relative-media-path...}
```

`/media` must preserve HTTP range support through the rclone VFS handle. Unsupported browser formats should use configured HLS profiles rather than raw delivery.

## Playback routes

```text
GET /play/hls/{profile}/{library}/{relative-path}/master.m3u8
GET /play/dash/{profile}/{library}/{relative-path}/manifest.mpd
```

The media path is relative to the configured library VFS root. URL-escape spaces and reserved characters.

## Hardware profiles

The checked-in example uses NVENC and VAAPI only.

- NVENC encoder: `h264_nvenc`
- VAAPI encoder: `h264_vaapi`
- VAAPI device default example: `/dev/dri/renderD128`
- Common preset used by the example: `fastest` (mapped to NVENC `p1`)
- The bridge does not pass a preset option to VAAPI

Do not add software profiles to the production example unless explicitly requested.

## CLI flags

Important flags:

```text
--config
--addr
--request-timeout
--max-jobs
--rate-limit
--cache-root
--vfs-cache-root
--allow-input-root
--api-keys
--cors-origins
--cors-credentials
--debug
```

`--cache-root` and `--vfs-cache-root` are independent.

## Editing rules

- Preserve unrelated user changes.
- Keep changes local and simple.
- Update tests when changing configuration parsing, playback URLs, VFS lifecycle, or AVIO behavior.
- Update `README.md` and `transcoder.example.yaml` when public configuration or routes change.
- Check Git status and final diff before committing.
- Do not commit, push, amend, rebase, or alter remotes unless explicitly requested.
