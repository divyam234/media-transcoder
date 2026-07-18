# media-transcoder

On-demand HLS and DASH transcoding with FFmpeg libraries, seekable HTTP and rclone VFS inputs, adaptive bitrate profiles, and segment caching.

## Requirements

- Go 1.22+
- FFmpeg 8.1 shared libraries
- NVIDIA drivers for NVENC, or `/dev/dri/renderD128` for VAAPI

## Build

`just` uses the active `pkg-config` environment first. On Nix systems it automatically discovers FFmpeg development libraries from `/nix/store`; no store hash is hardcoded. Set `FFMPEG_DEV` or `FFMPEG_LIB` only to override detection.

```bash
just build
```

Run:

```bash
./transcode-server --config ./transcoder.example.yaml
```

## Configuration

See [`transcoder.example.yaml`](./transcoder.example.yaml).

```yaml
server:
  addr: ":8080"
  cache_root: "/var/cache/media-transcoder/segments"
  vfs_cache_root: "/var/cache/media-transcoder/vfs"
  max_jobs: 4
  request_timeout: "30m"
  # input_url is disabled unless its host is explicitly allowed.
  http_allowed_hosts:
    - "media-cache"
    - "storage.example.com"

libraries:
  movies:
    vfs: "gdrive:Movies"
    options:
      vfs_cache_mode: "full"
      vfs_cache_max_size: "250GiB"
      vfs_cache_max_age: "24h"
      vfs_read_ahead: "64MiB"
      vfs_read_chunk_size: "16MiB"
      vfs_read_chunk_size_limit: "1GiB"

profiles:
  hls-h264-nvenc:
    container: hls
    segment_type: fmp4
    segment_seconds: 4
    audio:
      mode: transcode
      codec: aac
      bitrate: 128kbps
      channels: 2
      sample_rate: 48000
    video:
      codec: h264
      encoder_name: h264_nvenc
      preset: fastest
      crf: 23
      gop_size: 96
      max_b_frames: 0
    variants:
      - name: 720p
        width: 1280
        height: 720
        video_bitrate: 2.8Mbps
      - name: 1080p
        width: 1920
        height: 1080
        video_bitrate: 5.5Mbps
```

`cache_root` stores generated HLS/DASH output. `vfs_cache_root` stores rclone VFS cache data.

Libraries support:

```yaml
# Local path
vfs: "/srv/media/movies"

# Named rclone remote
vfs: "gdrive:Movies"

# Connection string
vfs: ":s3,provider=AWS,env_auth=true:media-bucket/movies"

# Seekable HTTP range library
http:
  base_url: "http://media-cache/media/"
  headers:
    Authorization: "Bearer internal-token"
```

A library must configure exactly one source: `vfs`/`encoded_config`, or `http`. For an HTTP library, the relative media path from the playback URL is appended to `base_url`.

Bitrates accept values such as `128kbps`, `2.8Mbps`, and plain bits per second. Rclone size options accept values such as `64MiB` and `250GiB`.

## HTTP range inputs

`POST /v1/probe`, `POST /v1/playback/hls/sessions`, and `POST /v1/playback/dash/sessions` accept either the existing `input_path` or a generic HTTP source:

```json
{
  "input_url": "http://media-cache/media/objects/6fd27c73",
  "input_headers": {
    "Authorization": "Bearer internal-token"
  }
}
```

The origin must support byte ranges and return `206 Partial Content` with a valid `Content-Range`. The transcoder keeps sequential HTTP responses open and opens a new range only when libav seeks, so the input is not materialized into an rclone VFS cache.

Enable trusted source hosts in YAML with `server.http_allowed_hosts`, or repeat the CLI flag `--http-allowed-host`. Exact hosts, `host:port`, wildcard subdomains such as `*.example.com`, and `*` are supported. An empty allowlist disables `input_url`. Cross-host redirects are revalidated and do not retain authorization or cookie headers.

For repeated playback, configure the HTTP origin as a library instead of manually creating sessions:

```yaml
libraries:
  cached_movies:
    http:
      base_url: "http://media-cache/media/"
      headers:
        Authorization: "Bearer internal-token"
```

Then use the same profile URLs as any VFS library:

```text
/play/hls/{profile}/cached_movies/Action/Movie.mkv/master.m3u8
/play/dash/{profile}/cached_movies/Action/Movie.mkv/manifest.mpd
```

The server creates and reuses the dynamic profile session automatically. Existing direct `input_url`, local-path, and rclone VFS inputs remain supported.

## Web UI

Open:

```text
http://localhost:8080/
```

The server-rendered UI lists libraries, browses VFS directories, and plays media with Plyr. Browser-native formats use the range-capable `/media/{library}/{path...}` route. Other media formats use the selected HLS profile.

## Playback URLs

HLS:

```text
http://HOST:PORT/play/hls/{profile}/{library}/{relative-media-path}/master.m3u8
```

Example:

```text
http://localhost:8080/play/hls/hls-h264-nvenc/movies/Action/Movie.mkv/master.m3u8
```

VAAPI:

```text
http://localhost:8080/play/hls/hls-h264-vaapi/movies/Action/Movie.mkv/master.m3u8
```

DASH:

```text
http://localhost:8080/play/dash/dash-h264-nvenc/movies/Action/Movie.mkv/manifest.mpd
```

The media path is relative to the configured library root. URL-encode spaces and special characters.

Play with ffplay:

```bash
ffplay "http://localhost:8080/play/hls/hls-h264-nvenc/movies/Action/Movie.mkv/master.m3u8"
```

Play with VLC:

```bash
vlc "http://localhost:8080/play/hls/hls-h264-nvenc/movies/Action/Movie.mkv/master.m3u8"
```

## Hardware profiles

Use the same preset names for every encoder:

| Common preset | x264/x265 | NVENC | QSV | AMF | VAAPI / VideoToolbox / V4L2M2M / RKMPP |
| --- | --- | --- | --- | --- | --- |
| `fastest` | `ultrafast` | `p1` | `veryfast` | `speed` | not passed |
| `fast` | `veryfast` | `p2` | `faster` | `speed` | not passed |
| `balanced` | `medium` | `p4` | `medium` | `balanced` | not passed |
| `quality` | `slow` | `p6` | `slower` | `quality` | not passed |
| `best` | `veryslow` | `p7` | `veryslow` | `quality` | not passed |

Encoder-specific aliases such as `ultrafast`, `p1`, `medium`, `slow`, and `p7` are normalized to the common preset names before reaching libav.

NVENC:

```yaml
video:
  encoder_name: h264_nvenc
  preset: fastest
```

VAAPI:

```yaml
video:
  encoder_name: h264_vaapi
  hardware_device: "/dev/dri/renderD128"
  hardware_decode: true
  preset: fastest # accepted, but VAAPI receives no preset option
```

## Main routes

```text
GET  /healthz
GET  /v1/capabilities
GET  /v1/capabilities/hardware
GET  /v1/metrics
GET  /v1/profiles
GET  /v1/libraries
POST /v1/probe
POST /v1/playback/hls/sessions
POST /v1/playback/dash/sessions
POST /v1/admin/reload

GET  /play/hls/{profile}/{library}/{path...}/master.m3u8
GET  /play/hls/{profile}/{library}/{path...}/variant/{variant}/video.m3u8
GET  /play/hls/{profile}/{library}/{path...}/variant/{variant}/segment/{index}.m4s

GET  /play/dash/{profile}/{library}/{path...}/manifest.mpd
GET  /play/dash/{profile}/{library}/{path...}/variant/{variant}/segment/{index}.m4s
```

## Tests

```bash
just test
just test-race
```

## License

MIT
