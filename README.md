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
  --api-keys secret1,secret2 \
  --cors-origins "http://localhost:5173,https://player.example" \
  --debug
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
    "audio_mode": "transcode",
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
GET    /v1/playback/hls/{id}/segment/{index}.ts       # MPEG-TS mode
GET    /v1/playback/hls/{id}/segment/init.mp4         # fMP4 mode init map
GET    /v1/playback/hls/{id}/segment/{index}.m4s      # fMP4 mode media fragment
DELETE /v1/playback/hls/{id}
```

`video.m3u8` is a virtual VOD timeline. Segment URL maps to:

```txt
segment index -> start time = index * segment_seconds
```

Server seeks input via libav, transcodes that segment window, caches it, and serves it. `DELETE /v1/playback/hls/{id}` cancels the session context, removes its cache directory, and releases per-segment lock bookkeeping.

### HLS fMP4/CMAF mode

Set `options.segment_type` to `"fmp4"` to emit HLS playlists with an `#EXT-X-MAP` initialization section and `.m4s` media fragments:

```json
{
  "input_path": "/media/movie.mkv",
  "prewarm_segments": 4,
  "options": {
    "width": 1280,
    "segment_seconds": 4,
    "segment_type": "fmp4",
    "audio_mode": "transcode",
    "crf": 28,
    "preset": "ultrafast"
  }
}
```

The returned media playlist uses:

```m3u8
#EXT-X-VERSION:7
#EXT-X-MAP:URI="segment/init.mp4"
#EXTINF:4.000,
segment/000000.m4s
```

Use MPEG-TS for maximum legacy compatibility. Use fMP4 when you want a modern HLS path that is closer to DASH/CMAF and can share more logic with `.m4s` DASH segment generation. Because this server generates seeked segments independently on demand, HLS fMP4 playlists also mark segment boundaries with `#EXT-X-DISCONTINUITY`; the single shared `init.mp4` map is still used for all `.m4s` fragments.

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
    "audio_mode": "transcode",
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
GET    /v1/playback/dash/{id}/manifest.mpd
GET    /v1/playback/dash/{id}/segment/init.mp4
GET    /v1/playback/dash/{id}/segment/{index}.m4s
DELETE /v1/playback/dash/{id}
```

MPD is virtual. It advertises an explicit `segment/init.mp4` initialization section and on-demand `.m4s` media fragments. `DELETE /v1/playback/dash/{id}` cancels the session context, removes its cache directory, and releases per-segment lock bookkeeping.

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

No packet-copy is used for dynamic playback audio because copy cannot sample-trim safely inside compressed audio packets.

For HLS MPEG-TS, every on-demand segment is generated independently, so the virtual playlist marks each segment boundary with `#EXT-X-DISCONTINUITY`. This avoids hard player stalls caused by AAC encoder priming/padding creating non-monotonic DTS across separately generated segments.

For HLS fMP4, the server writes a shared `init.mp4` map and `.m4s` media fragments. Segment boundaries are marked with `#EXT-X-DISCONTINUITY` because each segment is independently generated from a seek window. This is more robust for VLC/hls.js seeking than pretending separately encoded AAC/fMP4 windows are one continuous mux stream.

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

## Debugging playback

Run with `--debug` to see dynamic playback events: session creation, playlist serving,
segment generation start/end, cache hits, segment byte size, elapsed time, and errors.
For difficult HEVC/10-bit sources, keep `--max-jobs` at `4` or higher and use
`prewarm_segments` in the HLS session request so the next segments are generated
before the player reaches them. The long-source regression uses a real HLS client
to ensure playback does not hang on the first several segments.

## Profile-driven playback and library URLs

For production use, prefer a YAML config with libraries and playback profiles. A profile can define multiple variants, so one HLS master playlist or DASH MPD can expose several resolutions without creating multiple sessions.

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
  tv:
    root: "/srv/media/tv"
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

  nvidia-h264:
    container: hls
    segment_type: fmp4
    segment_seconds: 4
    audio:
      mode: transcode
      codec: aac
      bitrate: 128000
      channels: 2
    video:
      codec: h264
      encoder_name: h264_nvenc
      preset: fast
      crf: 30
      gop_size: 96
      max_b_frames: 0
    variants:
      - name: 720p
        width: 1280
        height: 720
        video_bitrate: 2800000
      - name: 1080p
        width: 1920
        height: 1080
        video_bitrate: 5000000
```

Run with config:

```bash
./transcode-server --config ./transcoder.yaml
```

Config routes:

```txt
GET  /v1/profiles
GET  /v1/profiles/{id}
GET  /v1/libraries
GET  /v1/libraries/{id}
POST /v1/admin/reload
```

`POST /v1/admin/reload` reloads the YAML profile/library section without restarting the process.

### HLS library playback URLs

Library playback URLs auto-create and reuse an internal dynamic session. The session key includes library, relative path, file size/mtime, and profile hash, so changing the media file or profile creates a fresh cache namespace.

```txt
GET /play/hls/{profile}/{library}/{path...}/master.m3u8
GET /play/hls/{profile}/{library}/{path...}/variant/{variant}/video.m3u8
GET /play/hls/{profile}/{library}/{path...}/variant/{variant}/segment/init.mp4
GET /play/hls/{profile}/{library}/{path...}/variant/{variant}/segment/{index}.m4s
GET /play/hls/{profile}/{library}/{path...}/variant/{variant}/segment/{index}.ts
```

Example:

```txt
http://localhost:8080/play/hls/web-h264/tv/House.of.the.Dragon/S02E07.mkv/master.m3u8
```

The master playlist contains every configured profile variant:

```m3u8
#EXTM3U
#EXT-X-VERSION:7
#EXT-X-INDEPENDENT-SEGMENTS
#EXT-X-STREAM-INF:BANDWIDTH=900000,RESOLUTION=640x360,CODECS="avc1.64001f,mp4a.40.2"
variant/360p/video.m3u8
#EXT-X-STREAM-INF:BANDWIDTH=2800000,RESOLUTION=1280x720,CODECS="avc1.64001f,mp4a.40.2"
variant/720p/video.m3u8
```

### DASH library playback URLs

DASH uses the same profiles and variants:

```txt
GET /play/dash/{profile}/{library}/{path...}/manifest.mpd
GET /play/dash/{profile}/{library}/{path...}/variant/{variant}/segment/init.mp4
GET /play/dash/{profile}/{library}/{path...}/variant/{variant}/segment/{index}.m4s
```

The MPD contains one `Representation` per variant and each representation has its own init segment and media fragment URLs.

### Library path safety

Library URLs never resolve arbitrary absolute paths. The server maps `{library}/{path...}` to a configured root and rejects:

- `..` traversal
- absolute paths
- paths that escape the configured root
- symlink escapes when `allow_symlinks: false`
