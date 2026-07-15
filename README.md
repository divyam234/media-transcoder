# media-transcoder

On-demand HLS and DASH transcoding with FFmpeg libraries, rclone VFS inputs, adaptive bitrate profiles, and segment caching.

## Requirements

- Go 1.22+
- FFmpeg 8.1 shared libraries
- NVIDIA drivers for NVENC, or `/dev/dri/renderD128` for VAAPI

## Build

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
      preset: ultrafast
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
```

Bitrates accept values such as `128kbps`, `2.8Mbps`, and plain bits per second. Rclone size options accept values such as `64MiB` and `250GiB`.

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

NVENC:

```yaml
video:
  encoder_name: h264_nvenc
  preset: ultrafast
```

VAAPI:

```yaml
video:
  encoder_name: h264_vaapi
  hardware_device: "/dev/dri/renderD128"
  hardware_decode: true
```

## Main routes

```text
GET  /healthz
GET  /v1/capabilities
GET  /v1/capabilities/hardware
GET  /v1/metrics
GET  /v1/profiles
GET  /v1/libraries
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
