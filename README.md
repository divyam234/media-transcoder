# go-media-transcoder

Dynamic Go media playback server built on FFmpeg shared libraries through cgo.

This project is intentionally separate from the thumbnail/sprite/previewer project. It does not contain sprite, preview slicing, VTT, or pHash code.

## Runtime contract

Runtime code does **not** spawn `ffmpeg` or `ffprobe`.

The implementation calls FFmpeg libraries directly:

- `libavformat`
- `libavcodec`
- `libavutil`
- `libavfilter`

`os/exec` is forbidden in runtime code and covered by a test.

## Important architecture

This server is now a **dynamic playback origin**, not a static transcoder.

It does **not** expose endpoints that transcode an entire file to a fixed output path before playback. Instead:

1. A playback session stores input metadata, profile, and cache location.
2. HLS playlists and DASH manifests are virtual and returned immediately.
3. Individual media segments are generated only when the browser/player requests them.
4. Seeking works by requesting the segment at the target timeline position.
5. Generated segments are cached and reused.

## Build

```bash
export FFMPEG_PREFIX=/path/to/ffmpeg-8.1
export PKG_CONFIG_PATH="$FFMPEG_PREFIX/lib/pkgconfig"
export CGO_LDFLAGS="-Wl,--disable-new-dtags -Wl,-rpath,$FFMPEG_PREFIX/lib"

./build.sh
```

## Run server

```bash
./transcode-server \
  --addr :8080 \
  --request-timeout 30m \
  --max-jobs 2 \
  --rate-limit 120 \
  --cache-root /var/cache/go-media-transcoder \
  --allow-input-root /srv/media \
  --api-keys secret1,secret2
```

Auth is disabled when `--api-keys` is empty. When enabled, send either:

```http
X-API-Key: secret1
```

or:

```http
Authorization: Bearer secret1
```

## HTTP API

### Health and capabilities

```txt
GET /healthz
GET /v1/capabilities
GET /v1/metrics
```

Capabilities explicitly report:

```json
{
  "dynamic_hls": true,
  "dynamic_dash": true,
  "on_demand_segments": true,
  "segment_cache": true,
  "dynamic_abr_hls": true,
  "metrics": true,
  "cooperative_cancel": true,
  "input_root_policy": true,
  "static_transcoding": false
}
```

### Probe

```txt
POST /v1/probe
```

```json
{ "input_path": "/media/movie.mkv" }
```

Returns duration, video dimensions, FPS, and audio-stream presence/count.

## Dynamic HLS playback

### Create HLS playback session

```txt
POST /v1/playback/hls/sessions
```

```json
{
  "input_path": "/media/movie.mkv",
  "cache_dir": "/tmp/transcode-cache",
  "prewarm_segments": 2,
  "options": {
    "width": 1280,
    "fps": 24,
    "segment_seconds": 4,
    "audio_mode": "skip",
    "crf": 28,
    "preset": "ultrafast"
  }
}
```

Response:

```json
{
  "id": "...",
  "master_url": "/v1/playback/hls/{id}/master.m3u8",
  "playlist_url": "/v1/playback/hls/{id}/video.m3u8",
  "duration": 1800,
  "segment_time": 4,
  "segment_count": 450
}
```

No media segments are generated during session creation.

### HLS URLs

```txt
GET /v1/playback/hls/{id}/master.m3u8
GET /v1/playback/hls/{id}/video.m3u8
GET    /v1/playback/hls/{id}/segment/{index}.ts
DELETE /v1/playback/hls/{id}
```

`video.m3u8` is a virtual VOD timeline. When the player seeks, it requests a segment URL. The server maps:

```txt
segment index -> start time = index * segment_seconds
```

Then it seeks the input with direct libav, transcodes only that segment window, caches it, and serves it. `DELETE /v1/playback/hls/{id}` cancels the session context, removes its cache directory, and releases per-segment lock bookkeeping.

### Dynamic ABR HLS

A session may expose multiple HLS variants without generating any media at creation time.
The master playlist points to variant playlists, and only the requested variant segment is generated.

```json
{
  "input_path": "/media/movie.mkv",
  "prewarm_segments": 2,
  "options": {
    "segment_seconds": 4,
    "audio_mode": "skip",
    "preset": "ultrafast"
  },
  "variants": [
    {
      "name": "360p",
      "width": 640,
      "height": 360,
      "video_bitrate": 900000,
      "crf": 30
    },
    {
      "name": "720p",
      "width": 1280,
      "height": 720,
      "video_bitrate": 2800000,
      "crf": 28
    }
  ]
}
```

Variant URLs:

```txt
GET /v1/playback/hls/{id}/variant/360p/video.m3u8
GET /v1/playback/hls/{id}/variant/360p/segment/{index}.ts
GET /v1/playback/hls/{id}/variant/720p/video.m3u8
GET /v1/playback/hls/{id}/variant/720p/segment/{index}.ts
```

Requesting a `720p` segment does not generate the `360p` segment. Each variant has its own cache namespace.

### Production safety knobs

Server flags:

```txt
--cache-root /var/cache/go-media-transcoder
--allow-input-root /srv/media
--allow-input-root /mnt/library
```

When `--cache-root` is set, client-provided `cache_dir` is ignored and all dynamic segment cache files stay under the server-owned root. When one or more `--allow-input-root` values are set, `input_path` outside those roots is rejected with `403`.

### Metrics

```txt
GET /v1/metrics
```

Returns counters for sessions, generated segments, cache hits, and segment errors.

## Dynamic DASH playback

### Create DASH playback session

```txt
POST /v1/playback/dash/sessions
```

```json
{
  "input_path": "/media/movie.mkv",
  "cache_dir": "/tmp/transcode-cache",
  "prewarm_segments": 2,
  "options": {
    "width": 1280,
    "fps": 24,
    "segment_seconds": 4,
    "audio_mode": "skip",
    "crf": 28,
    "preset": "ultrafast"
  }
}
```

Response:

```json
{
  "id": "...",
  "manifest_url": "/v1/playback/dash/{id}/manifest.mpd",
  "duration": 1800,
  "segment_time": 4,
  "segment_count": 450
}
```

### DASH URLs

```txt
GET /v1/playback/dash/{id}/manifest.mpd
GET    /v1/playback/dash/{id}/segment/{index}.m4s
DELETE /v1/playback/dash/{id}
```

The MPD is virtual. Segments are generated on demand as fragmented MP4 windows through direct libav. `DELETE /v1/playback/dash/{id}` cancels the session context, removes its cache directory, and releases per-segment lock bookkeeping.

## Removed static server endpoints

These are intentionally **not exposed** by the server anymore:

```txt
POST /v1/transcode/progressive
POST /v1/transcode/hls
POST /v1/transcode/dash
POST /v1/transcode/abr-hls
POST /v1/transcode/profile
POST /v1/sessions
GET  /v1/sessions
```

The library still contains low-level direct-libav primitives used internally for segment generation, but the HTTP service is now dynamic playback only.

## Audio

Dynamic playback now supports timestamp-trimmed audio for arbitrary HLS/DASH segment requests.

Supported modes:

- `audio_mode: "skip"` disables audio.
- `audio_mode: "transcode"` decodes the selected input audio stream, trims it to the requested segment window with `atrim`, resets timestamps with `asetpts`, resamples/formats for AAC, and muxes it into the generated HLS/DASH segment.
- omitted `audio_mode` now defaults to `"transcode"` for dynamic playback sessions when the source has audio, otherwise `"skip"`.

Important production behavior:

- arbitrary segment starts such as 17.37s are supported;
- output segment audio starts at timestamp zero;
- output duration stays bounded to the requested segment duration;
- no packet-copy shortcut is used for arbitrary dynamic segments, because copy cannot sample-trim safely inside audio packets.

The low-level API still accepts `audio_mode: "copy"` for compatibility, but dynamic playback uses transcoded AAC for timestamp-correct segments.

## Hardware acceleration capabilities

The planner still exposes hardware acceleration profiles:

- `none`
- `amf`
- `qsv`
- `nvenc`
- `v4l2m2m`
- `vaapi`
- `videotoolbox`
- `rkmpp`

Actual hardware execution requires host GPU/device support and an FFmpeg build exposing that encoder. Unsupported branches are rejected; the server never falls back to spawning `ffmpeg`.

## Explicitly not implemented

Per current scope:

- subtitles: burn-in, extract, embed
- WebSockets
- static transcode-to-output server endpoints

## Tests

```bash
go test ./... -count=1
go test -race ./... -count=1
```

The schema documents only the dynamic playback service surface: health, capabilities, probe, device planning, on-demand HLS, and on-demand DASH. Static transcode-to-output routes are intentionally absent.

## Runtime capability router

The server exposes runtime-discovered FFmpeg/libav capability routes. These do **not** call `ffmpeg` or `ffprobe`; they enumerate the linked FFmpeg shared libraries directly through cgo.

```txt
GET /v1/capabilities
GET /v1/capabilities/runtime
GET /v1/capabilities/codecs
GET /v1/capabilities/hardware
```

`/v1/capabilities/runtime` returns the actual FFmpeg build information:

```json
{
  "ffmpeg_version": "8.1",
  "hardware_device_types": [
    "cuda",
    "drm",
    "opencl",
    "qsv",
    "vaapi",
    "vdpau",
    "vulkan"
  ],
  "video_encoders": ["libx264", "libx265", "h264_nvenc"],
  "audio_encoders": ["aac", "libopus"]
}
```

`/v1/capabilities/hardware` maps hardware acceleration profiles to the runtime FFmpeg build:

```json
{
  "hardware_accelerators": [
    "none",
    "amf",
    "qsv",
    "nvenc",
    "v4l2m2m",
    "vaapi",
    "videotoolbox",
    "rkmpp"
  ],
  "matrix": [
    {
      "codec": "h264",
      "hardware": "nvenc",
      "encoder_name": "h264_nvenc",
      "supported_by_policy": true,
      "encoder_available_in_build": true,
      "hw_device_type": "cuda",
      "hw_device_type_in_build": true,
      "host_device_hint_available": false,
      "runnable_likely": false,
      "notes": [
        "host hardware/device hint was not found; build support alone may still not run"
      ]
    }
  ]
}
```

Important distinction:

- `encoder_available_in_build` means the linked FFmpeg build contains that encoder.
- `hw_device_type_in_build` means FFmpeg exposes that hardware device API, such as `cuda`, `vaapi`, or `qsv`.
- `host_device_hint_available` is a best-effort host check, such as `/dev/dri/renderD128` or `/dev/nvidia0`.
- `runnable_likely` is true only when build support and host hints both look usable.

This avoids lying about hardware acceleration: a build can contain `h264_nvenc` but still fail at runtime on a host without NVIDIA hardware.
