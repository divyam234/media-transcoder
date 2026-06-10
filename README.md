# media-transcoder

Dynamic Go media playback server built on FFmpeg shared libraries through cgo.

## Runtime contract

The implementation calls FFmpeg libraries directly:

- `libavformat`
- `libavcodec`
- `libavutil`
- `libavfilter`

## Architecture

1. Playback session stores input metadata, profile, and cache location
2. HLS/DASH manifests are virtual — returned immediately
3. Media segments generated on demand at request time
4. Seeking requests the segment at the target timeline position
5. Generated segments are cached and reused

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
  --cache-root /var/cache/media-transcoder \
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

Capabilities report:

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

### HLS URLs

```txt
GET /v1/playback/hls/{id}/master.m3u8
GET /v1/playback/hls/{id}/video.m3u8
GET    /v1/playback/hls/{id}/segment/{index}.ts
DELETE /v1/playback/hls/{id}
```

`video.m3u8` is a virtual VOD timeline. Segment URL maps to:

```txt
segment index -> start time = index * segment_seconds
```

Server seeks input via libav, transcodes that segment window, caches it, and serves it. `DELETE /v1/playback/hls/{id}` cancels the session context, removes its cache directory, and releases per-segment lock bookkeeping.

### Dynamic ABR HLS

Multiple HLS variants without generating media at creation time. Master playlist points to variant playlists; only the requested variant segment is generated.

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

### Production safety

Server flags:

```txt
--cache-root /var/cache/media-transcoder
--allow-input-root /srv/media
--allow-input-root /mnt/library
```

With `--cache-root`, client-provided `cache_dir` is ignored — all segment cache files stay under the server-owned root. With `--allow-input-root`, `input_path` outside those roots is rejected with `403`.

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

MPD is virtual. Segments generated on demand as fragmented MP4 windows. `DELETE /v1/playback/dash/{id}` cancels the session context, removes its cache directory, and releases per-segment lock bookkeeping.

## Removed static endpoints

```txt
POST /v1/transcode/progressive
POST /v1/transcode/hls
POST /v1/transcode/dash
POST /v1/transcode/abr-hls
POST /v1/transcode/profile
POST /v1/sessions
GET  /v1/sessions
```

Internal segment-generation primitives remain in the library, but the HTTP service is dynamic playback only.

## Audio

Timestamp-trimmed audio for arbitrary HLS/DASH segment requests.

- `audio_mode: "skip"` — no audio
- `audio_mode: "transcode"` — decode, trim to segment window with `atrim`, reset timestamps with `asetpts`, resample/format for AAC, mux into segment
- omitted defaults to `"transcode"` when source has audio, otherwise `"skip"`

Output audio starts at timestamp zero. Duration stays bounded to requested segment duration. No packet-copy — copy cannot sample-trim safely inside audio packets. `audio_mode: "copy"` accepted in low-level API for compatibility only.

## Hardware acceleration capabilities

Hardware acceleration profiles:

- `none`
- `amf`
- `qsv`
- `nvenc`
- `v4l2m2m`
- `vaapi`
- `videotoolbox`
- `rkmpp`

Hardware execution requires host GPU/device support and an FFmpeg build exposing that encoder. Unsupported branches are rejected.

## Not implemented

- subtitles: burn-in, extract, embed
- WebSockets
- static transcode-to-output server endpoints

## Tests

```bash
go test ./... -count=1
go test -race ./... -count=1
```

## Runtime capability router

Runtime-discovered FFmpeg/libav capability routes. Enumerates the linked FFmpeg shared libraries directly through cgo.

```txt
GET /v1/capabilities
GET /v1/capabilities/runtime
GET /v1/capabilities/codecs
GET /v1/capabilities/hardware
```

`/v1/capabilities/runtime` returns FFmpeg build information:

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

- `encoder_available_in_build` — linked FFmpeg build contains that encoder
- `hw_device_type_in_build` — FFmpeg exposes that hardware device API (`cuda`, `vaapi`, `qsv`, etc.)
- `host_device_hint_available` — best-effort host check (`/dev/dri/renderD128`, `/dev/nvidia0`, etc.)
- `runnable_likely` — true only when build support and host hints both look usable
