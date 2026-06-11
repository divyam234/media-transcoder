# media-transcoder

Dynamic media playback server that transcodes on demand via FFmpeg shared libraries (`libavformat`, `libavcodec`, `libavutil`, `libavfilter`) through cgo.

## Features

- Dynamic HLS (MPEG-TS and fMP4/CMAF) and DASH playback
- On-demand segment generation with caching and reuse
- ABR HLS: multiple variant playlists from a single session
- Audio modes: skip, transcode (AAC, resampled to 48 kHz stereo)
- Hardware acceleration: none, amf, qsv, nvenc, v4l2m2m, vaapi, videotoolbox, rkmpp
- Profile-driven YAML config: define libraries (media roots) and playback profiles (codec, resolution, variants)
- Library playback URLs: `GET /play/hls/{profile}/{library}/{path...}/master.m3u8`
- Capability discovery: runtime FFmpeg version, available encoders, hardware device types
- Input probing: duration, dimensions, FPS, audio presence
- API key auth (optional), CORS, rate limiting, input root restriction
- Metric counters: sessions, segments generated, cache hits, errors

## Installation

Requires Go 1.22+ and a FFmpeg 8.1 shared library build.

```bash
export FFMPEG_PREFIX=/path/to/ffmpeg-8.1
export PKG_CONFIG_PATH="$FFMPEG_PREFIX/lib/pkgconfig"
export CGO_LDFLAGS="-Wl,--disable-new-dtags -Wl,-rpath,$FFMPEG_PREFIX/lib"

./build.sh
```

## Docker

```bash
# Run with media directory and config
docker run -d --name transcode-server \
  -p 8080:8080 \
  -v /path/to/media:/srv/media \
  -v /path/to/config.yaml:/etc/transcoder.yaml:ro \
  ghcr.io/divyam234/media-transcoder:latest \
  --config /etc/transcoder.yaml --allow-input-root /srv/media

# Minimal run with inline flags
docker run -d --name transcode-server \
  -p 8080:8080 \
  -v /path/to/media:/srv/media \
  ghcr.io/divyam234/media-transcoder:latest \
  --addr :8080 --allow-input-root /srv/media
```

## Quick Start

```bash
# Run with defaults (no auth, no cache root, single input root)
./transcode-server --addr :8080 --allow-input-root /srv/media

# Or with a YAML config file (see Configuration section)
./transcode-server --config ./transcoder.yaml
```

## Configuration

### YAML config file

See `transcoder.example.yaml` for a full example. The config defines server settings, libraries (named media roots), and profiles (codec/resolution presets with optional ABR variants).

```yaml
server:
  addr: ":8080"
  cache_root: "/var/cache/media-transcoder"
  debug: true
  max_jobs: 4
  allow_input_roots:
    - "/srv/media"

libraries:
  movies:
    root: "/srv/media/movies"
    allow_symlinks: false

profiles:
  web-h264:
    container: hls
    segment_type: fmp4
    segment_seconds: 4
    audio:
      mode: transcode
      codec: aac
      bitrate: 128000
      channels: 2
      sample_rate: 48000
    video:
      codec: h264
      encoder_name: libx264
      preset: veryfast
      crf: 28
      gop_size: 96
      max_b_frames: 0
    variants:
      - name: 360p
        width: 640
        height: 360
        video_bitrate: 900000
        crf: 30
      - name: 720p
        width: 1280
        height: 720
        video_bitrate: 2800000
        crf: 28
```

`POST /v1/admin/reload` reloads the profile/library section without restarting.

### CLI flags

| Flag                 | Default | Description                                          |
| -------------------- | ------- | ---------------------------------------------------- |
| `--config`           | `""`    | Path to YAML config file                             |
| `--addr`             | `:8080` | Listen address                                       |
| `--request-timeout`  | `5m`    | Request timeout                                      |
| `--max-jobs`         | `4`     | Max concurrent segment jobs                          |
| `--rate-limit`       | `0`     | Rate limit per minute (0 = unlimited)                |
| `--cache-root`       | `""`    | Server-owned cache root (ignores client `cache_dir`) |
| `--allow-input-root` | `""`    | Allowed input path roots (repeatable)                |
| `--api-keys`         | `""`    | Comma-separated API keys (empty = auth disabled)     |
| `--cors-origins`     | `""`    | Allowed CORS origins                                 |
| `--debug`            | `false` | Enable debug logging                                 |

### Auth

When `--api-keys` is set, requests must include:

```
X-API-Key: <key>
Authorization: Bearer <key>
```

## Usage

### Create an HLS session

```bash
curl -X POST http://localhost:8080/v1/playback/hls/sessions \
  -H 'Content-Type: application/json' \
  -d '{
    "input_path": "/srv/media/movie.mkv",
    "prewarm_segments": 2,
    "options": {
      "width": 1280,
      "fps": 24,
      "segment_seconds": 4,
      "segment_type": "fmp4",
      "audio_mode": "transcode",
      "crf": 28,
      "preset": "ultrafast"
    }
  }'
```

Response includes `master_url` and `playlist_url`.

### Library playback URL (requires config)

```
http://localhost:8080/play/hls/web-h264/tv/House.of.the.Dragon/S02E07.mkv/master.m3u8
```

This auto-creates an internal session keyed by library + relative path + file metadata + profile hash.

## Development

```bash
# Build
./build.sh

# Run locally (debug mode)
./transcode-server --addr :8080 --allow-input-root /tmp/media --debug

# Tests
go test ./... -count=1
go test -race ./... -count=1
```

## Architecture

1. Playback session stores input metadata, profile, and cache location
2. HLS/DASH manifests are virtual — returned immediately, no transcoding
3. Media segments are generated on demand at request time
4. Seeking requests the segment at the target timeline position
5. Generated segments are cached and reused

Each on-demand segment is independently generated from a seek window. For HLS, every segment boundary is marked with `#EXT-X-DISCONTINUITY` because separately encoded AAC frames may produce non-monotonic DTS.

Audio uses `atrim` + `asetpts` filters (not packet copy) because compressed audio packets cannot be safely sample-trimmed.

## API / CLI Reference

### CLI

`transcode-server` is the only binary. Flags documented in [Configuration](#configuration).

### Endpoints

```
# Health
GET  /healthz

# Capabilities
GET  /v1/capabilities
GET  /v1/capabilities/runtime
GET  /v1/capabilities/codecs
GET  /v1/capabilities/hardware

# Probe
POST /v1/probe                          {"input_path": "/media/file.mkv"}

# Metrics
GET  /v1/metrics

# HLS
POST /v1/playback/hls/sessions          create session
GET  /v1/playback/hls/{id}/master.m3u8
GET  /v1/playback/hls/{id}/video.m3u8
GET  /v1/playback/hls/{id}/segment/{index}.ts
GET  /v1/playback/hls/{id}/segment/init.mp4
GET  /v1/playback/hls/{id}/segment/{index}.m4s
DELETE /v1/playback/hls/{id}

# DASH
POST /v1/playback/dash/sessions         create session
GET  /v1/playback/dash/{id}/manifest.mpd
GET  /v1/playback/dash/{id}/segment/init.mp4
GET  /v1/playback/dash/{id}/segment/{index}.m4s
DELETE /v1/playback/dash/{id}

# Config-driven library URLs
GET  /play/hls/{profile}/{library}/{path...}/master.m3u8
GET  /play/hls/{profile}/{library}/{path...}/variant/{variant}/video.m3u8
GET  /play/hls/{profile}/{library}/{path...}/variant/{variant}/segment/{index}.ts
GET  /play/hls/{profile}/{library}/{path...}/variant/{variant}/segment/{index}.m4s
GET  /play/hls/{profile}/{library}/{path...}/variant/{variant}/segment/init.mp4
GET  /play/dash/{profile}/{library}/{path...}/manifest.mpd
GET  /play/dash/{profile}/{library}/{path...}/variant/{variant}/segment/init.mp4
GET  /play/dash/{profile}/{library}/{path...}/variant/{variant}/segment/{index}.m4s

# Config management
GET  /v1/profiles
GET  /v1/profiles/{id}
GET  /v1/libraries
GET  /v1/libraries/{id}
POST /v1/admin/reload
```

### Library path safety

Library URLs map `{library}/{path...}` to a configured root. The server rejects:

- `..` traversal
- Absolute paths
- Paths escaping the configured root
- Symlink escapes when `allow_symlinks: false`

## Testing

```bash
go test ./... -count=1
go test -race ./... -count=1
```

Tests cover session creation, manifest generation, segment serving, cache behavior, config profile loading, and library URL routing.

## Troubleshooting

**Segments fail for HEVC/10-bit sources** — Keep `--max-jobs` at 4 or higher and use `prewarm_segments` in the session request so segments are generated before the player requests them.

**Authentication errors** — Omit `--api-keys` to disable auth, or pass the key via `X-API-Key` or `Authorization: Bearer` header.

**"input_path outside allowed roots"** — Add the directory to `--allow-input-root` or `server.allow_input_roots` in config.

**Hardware encoder not used** — Check `/v1/capabilities/hardware` for `runnable_likely` field. The FFmpeg build may support the encoder but the host device may be missing.

## Roadmap / TODO

- Subtitles: burn-in, extract, embed
- WebSocket support
- Static transcode-to-output server endpoints

## License

MIT
