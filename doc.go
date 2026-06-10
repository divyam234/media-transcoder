// Package transcoder is a direct-libav media playback/transcoding library.
//
// The HTTP server built on this package is a dynamic playback origin: it serves
// virtual HLS/DASH manifests and generates requested segments on demand without
// spawning ffmpeg or ffprobe at runtime.
package transcoder
