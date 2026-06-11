# AGENTS.md
## Build

Requires FFmpeg 8.1+ shared libs (`libavformat`, `libavcodec`, `libavutil`, `libavfilter`) with pkg-config files installed.

If FFmpeg dev packages are installed system-wide, just run:

```bash
make build
```

For a custom FFmpeg build:

```bash
export FFMPEG_PREFIX=/path/to/ffmpeg-8.1
make build
```

The binary `transcode-server` is written to the repo root.

## Package structure

| Path | Role |
|------|------|
| `media-transcoder` (root) | Core libav bridge via cgo — probe, transcode, segment, capabilities |
| `server/` | HTTP server (chi router), dynamic HLS/DASH session management |
| `cmd/transcode-server/` | Main binary, CLI flags via `pflag` |
| `testdata/sample.mp4` | Small test media file used by unit tests |

## Key CLI flags

```
--config       YAML config file with libraries/profiles/server defaults
--addr         Listen address (default :8080)
--request-timeout  Request timeout (default 30m)
--max-jobs     Max concurrent segment jobs (default 4)
--rate-limit   Requests/min per client (0 = unlimited)
--cache-root   Server-owned cache root (ignores client cache_dir when set)
--allow-input-root  Repeatable; restricts input_path. Empty = allow any path.
--api-keys     Comma-separated; empty = disable auth
--cors-origins Default is *; set to specific origins for credentialed requests
--debug        Debug logging
```

## Tests

```bash
go test ./... -count=1
go test -race ./... -count=1
```

Tests use `testdata/sample.mp4` via relative path `filepath.Join("..", "testdata", "sample.mp4")` from the `server/` package.

Two integration tests are skipped by default — they require `TRANSCODER_TEST_LONG_INPUT` pointing to a HEVC media file:
- `server/long_media_integration_test.go`
- `server/hls_client_integration_test.go` (also needs `ffmpeg` CLI on PATH or `TRANSCODER_FFMPEG_CLI`)

## Architecture

- Transcoding runs as direct libav calls via cgo (`ffmpeg_bridge.c`/`.h`). No `ffmpeg` or `ffprobe` subprocess is spawned.
- Media segments are generated on demand at request time (virtual manifests, real segments).
- Generated segments are cached by path. Cache is keyed by session or (for library URLs) by content hash + profile.
- Each segment is independently generated from a seek window using `start_time`/`duration` options.
- Dynamic ABR: requesting one variant's segment does not generate other variants.
- Library URLs (`/play/hls/{profile}/{library}/{path...}`) auto-create sessions keyed by content metadata + profile hash.

## Config

YAML config (via `--config`) defines:
- `libraries` — named media roots with symlink policy
- `profiles` — playback profiles with optional ABR variants

`POST /v1/admin/reload` hot-reloads the profile/library section.

## Audio

Dynamic playback always transcodes audio via `atrim` + `asetpts` filters (never packet copy), because compressed audio packets cannot be safely sample-trimmed. Three modes: `skip`, `copy`, `transcode` (defaults to `transcode` when source has audio).

## Important gotchas

- All API routes require auth when `--api-keys` is set. Auth header: `X-API-Key` or `Authorization: Bearer`.
- Static transcode endpoints (`/v1/transcode/*`, `/v1/sessions`) were removed. Only dynamic playback exists.
- Subtitle handling is not implemented. Set `skip_subtitles=true` or omit.
- No pre-commit hooks, linters, or formatters are configured.
- `Height` in video options is reserved; current filter keeps aspect via width only.
- cgo means `CGO_ENABLED=1` is required (it is by default but must not be overridden).
- The binary links FFmpeg shared libs via rpath set at build time.

## Compatibility notes

- Chi router is used for HTTP routing (not stdlib mux).
- `pflag` (not std `flag`) for CLI parsing.
- Module path is `media-transcoder` (root package import path).
- Go version is 1.26 (see `go.mod`).
