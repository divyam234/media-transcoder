package transcoder

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// TranscodeOptions controls one progressive video transcode. This is the core
// direct-libav pipeline: demux, decode, filter, encode, mux. No ffmpeg process is spawned.
type TranscodeOptions struct {
	EncoderName string  `json:"encoder_name,omitempty"`
	Width       int     `json:"width,omitempty"`
	Height      int     `json:"height,omitempty"`
	FPS         float64 `json:"fps,omitempty"`
	CRF         int     `json:"crf,omitempty"`
	Preset      string  `json:"preset,omitempty"`
	GOPSize     int     `json:"gop_size,omitempty"`
	MaxBFrames  int     `json:"max_b_frames,omitempty"`
	FastStart   bool    `json:"fast_start,omitempty"`

	// AudioMode controls audio handling. Supported runtime modes are "skip" and
	// "copy". "transcode" is planned at API level and currently falls back to AAC
	// copy only when the input is already AAC-compatible.
	AudioMode     AudioMode `json:"audio_mode,omitempty"`
	AudioStream   int       `json:"audio_stream,omitempty"`
	AudioCodec    string    `json:"audio_codec,omitempty"`
	AudioBitrate  int       `json:"audio_bitrate,omitempty"`
	AudioChannels int       `json:"audio_channels,omitempty"`

	// Color/HDR planning knobs. The direct software bridge accepts them as stable
	// API fields; hardware tone-map execution is guarded by BuildPlan capability
	// checks until device-specific contexts are available.
	ToneMap      ToneMapOptions `json:"tone_map"`
	VideoBitrate int            `json:"video_bitrate,omitempty"`

	// StartTime and Duration are used by dynamic/on-demand playback paths.
	// They seek the source and emit only the requested media window, with
	// output timestamps reset to zero by the direct libav bridge.
	StartTime float64 `json:"start_time,omitempty"`
	Duration  float64 `json:"duration,omitempty"`

	// TimestampOffset shifts encoded packet timestamps in the output. Dynamic HLS
	// uses this so independently generated MPEG-TS segments form one continuous
	// playlist timeline instead of resetting PTS/DTS to zero at every segment.
	TimestampOffset float64 `json:"timestamp_offset,omitempty"`
}

// HLSOptions controls HLS VOD/event output.
type HLSOptions struct {
	TranscodeOptions
	SegmentSeconds float64 `json:"segment_seconds,omitempty"`
	SegmentPattern string  `json:"segment_pattern,omitempty"`
	SegmentType    string  `json:"segment_type,omitempty"`
	PlaylistType   string  `json:"playlist_type,omitempty"`
	ListSize       int     `json:"list_size,omitempty"`
	Live           bool    `json:"live,omitempty"`
}

type TranscodeResult struct {
	OutputPath string    `json:"output_path"`
	Info       MediaInfo `json:"info"`
}

type HLSResult struct {
	PlaylistPath   string    `json:"playlist_path"`
	SegmentPattern string    `json:"segment_pattern"`
	Info           MediaInfo `json:"info"`
}

func (o *TranscodeOptions) ApplyDefaults() {
	if o.AudioStream < 0 {
		o.AudioStream = 0
	}
	if o.AudioMode == "" {
		o.AudioMode = AudioSkip
	}
	if o.CRF <= 0 {
		o.CRF = 28
	}
	if o.Preset == "" {
		o.Preset = "ultrafast"
	}
	if o.GOPSize <= 0 {
		o.GOPSize = 48
	}
}

func (o *HLSOptions) ApplyDefaults(playlistPath string) {
	o.TranscodeOptions.ApplyDefaults()
	if o.SegmentSeconds <= 0 {
		o.SegmentSeconds = 4
	}
	if o.SegmentType == "" {
		o.SegmentType = "mpegts"
	}
	if o.PlaylistType == "" {
		if o.Live {
			o.PlaylistType = "event"
		} else {
			o.PlaylistType = "vod"
		}
	}
	if o.ListSize < 0 {
		o.ListSize = 0
	}
	if o.SegmentPattern == "" {
		ext := ".ts"
		if o.SegmentType == "fmp4" {
			ext = ".m4s"
		}
		dir := filepath.Dir(playlistPath)
		base := trimExt(filepath.Base(playlistPath))
		o.SegmentPattern = filepath.Join(dir, base+"_%05d"+ext)
	}
}

func trimExt(name string) string {
	ext := filepath.Ext(name)
	if ext == "" {
		return name
	}
	return name[:len(name)-len(ext)]
}

func ProbeSource(ctx context.Context, src Source) (MediaInfo, error) {
	if err := checkContext(ctx); err != nil {
		return MediaInfo{}, err
	}
	prepared, err := prepareSource(ctx, src)
	if err != nil {
		return MediaInfo{}, err
	}
	if prepared.cleanup != nil {
		defer prepared.cleanup()
	}
	return Probe(prepared.path)
}

func ProbeFile(ctx context.Context, path string) (MediaInfo, error) {
	return ProbeSource(ctx, FromFile(path))
}
func ProbeReadSeeker(ctx context.Context, name string, r io.ReadSeeker) (MediaInfo, error) {
	return ProbeSource(ctx, FromReadSeeker(name, r))
}

func TranscodeSegment(ctx context.Context, src Source, outputPath string, opts TranscodeOptions) (*TranscodeResult, error) {
	if err := checkContext(ctx); err != nil {
		return nil, err
	}
	if outputPath == "" {
		return nil, fmt.Errorf("output path is required")
	}
	if opts.Duration <= 0 {
		return nil, fmt.Errorf("segment duration must be > 0")
	}
	opts.ApplyDefaults()
	if err := validateAudioMode(opts.AudioMode); err != nil {
		return nil, err
	}
	prepared, err := prepareSource(ctx, src)
	if err != nil {
		return nil, err
	}
	if prepared.cleanup != nil {
		defer prepared.cleanup()
	}
	if err := os.MkdirAll(filepath.Dir(outputPath), 0o755); err != nil {
		return nil, err
	}
	info, err := Probe(prepared.path)
	if err != nil {
		return nil, err
	}
	if err := transcodeSegmentVideo(ctx, prepared.path, outputPath, opts); err != nil {
		_ = os.Remove(outputPath)
		return nil, err
	}
	return &TranscodeResult{OutputPath: outputPath, Info: info}, nil
}

func TranscodeSegmentFromFile(ctx context.Context, path, outputPath string, opts TranscodeOptions) (*TranscodeResult, error) {
	return TranscodeSegment(ctx, FromFile(path), outputPath, opts)
}

func TranscodeSegmentFromReadSeeker(ctx context.Context, name string, r io.ReadSeeker, outputPath string, opts TranscodeOptions) (*TranscodeResult, error) {
	return TranscodeSegment(ctx, FromReadSeeker(name, r), outputPath, opts)
}

// TranscodeFMP4Segment emits a single on-demand fragmented MP4 media window.
// It is intended for dynamic DASH playback origins, not static full-media output.
func TranscodeFMP4Segment(ctx context.Context, src Source, outputPath string, opts TranscodeOptions) (*TranscodeResult, error) {
	if err := checkContext(ctx); err != nil {
		return nil, err
	}
	if outputPath == "" {
		return nil, fmt.Errorf("output path is required")
	}
	if opts.Duration <= 0 {
		return nil, fmt.Errorf("segment duration must be > 0")
	}
	opts.ApplyDefaults()
	if err := validateAudioMode(opts.AudioMode); err != nil {
		return nil, err
	}
	prepared, err := prepareSource(ctx, src)
	if err != nil {
		return nil, err
	}
	if prepared.cleanup != nil {
		defer prepared.cleanup()
	}
	if err := os.MkdirAll(filepath.Dir(outputPath), 0o755); err != nil {
		return nil, err
	}
	info, err := Probe(prepared.path)
	if err != nil {
		return nil, err
	}
	if err := transcodeFMP4SegmentVideo(ctx, prepared.path, outputPath, opts); err != nil {
		_ = os.Remove(outputPath)
		return nil, err
	}
	return &TranscodeResult{OutputPath: outputPath, Info: info}, nil
}

func TranscodeFMP4SegmentFromFile(ctx context.Context, path, outputPath string, opts TranscodeOptions) (*TranscodeResult, error) {
	return TranscodeFMP4Segment(ctx, FromFile(path), outputPath, opts)
}

func TranscodeFMP4SegmentFromReadSeeker(ctx context.Context, name string, r io.ReadSeeker, outputPath string, opts TranscodeOptions) (*TranscodeResult, error) {
	return TranscodeFMP4Segment(ctx, FromReadSeeker(name, r), outputPath, opts)
}

func TranscodeProgressive(ctx context.Context, src Source, outputPath string, opts TranscodeOptions) (*TranscodeResult, error) {
	if err := checkContext(ctx); err != nil {
		return nil, err
	}
	if outputPath == "" {
		return nil, fmt.Errorf("output path is required")
	}
	opts.ApplyDefaults()
	if err := validateAudioMode(opts.AudioMode); err != nil {
		return nil, err
	}
	if !opts.FastStart {
		opts.FastStart = true
	}
	prepared, err := prepareSource(ctx, src)
	if err != nil {
		return nil, err
	}
	if prepared.cleanup != nil {
		defer prepared.cleanup()
	}
	if err := os.MkdirAll(filepath.Dir(outputPath), 0o755); err != nil {
		return nil, err
	}
	info, err := Probe(prepared.path)
	if err != nil {
		return nil, err
	}
	if err := transcodeVideo(ctx, prepared.path, outputPath, opts); err != nil {
		_ = os.Remove(outputPath)
		return nil, err
	}
	return &TranscodeResult{OutputPath: outputPath, Info: info}, nil
}

func TranscodeProgressiveFromFile(ctx context.Context, path, outputPath string, opts TranscodeOptions) (*TranscodeResult, error) {
	return TranscodeProgressive(ctx, FromFile(path), outputPath, opts)
}

func TranscodeProgressiveFromReadSeeker(ctx context.Context, name string, r io.ReadSeeker, outputPath string, opts TranscodeOptions) (*TranscodeResult, error) {
	return TranscodeProgressive(ctx, FromReadSeeker(name, r), outputPath, opts)
}

func TranscodeHLS(ctx context.Context, src Source, playlistPath string, opts HLSOptions) (*HLSResult, error) {
	if err := checkContext(ctx); err != nil {
		return nil, err
	}
	if playlistPath == "" {
		return nil, fmt.Errorf("playlist path is required")
	}
	opts.ApplyDefaults(playlistPath)
	if err := validateAudioMode(opts.AudioMode); err != nil {
		return nil, err
	}
	prepared, err := prepareSource(ctx, src)
	if err != nil {
		return nil, err
	}
	if prepared.cleanup != nil {
		defer prepared.cleanup()
	}
	if err := os.MkdirAll(filepath.Dir(playlistPath), 0o755); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(opts.SegmentPattern), 0o755); err != nil {
		return nil, err
	}
	info, err := Probe(prepared.path)
	if err != nil {
		return nil, err
	}
	if err := transcodeHLSVideo(ctx, prepared.path, playlistPath, opts.SegmentPattern, opts); err != nil {
		_ = os.Remove(playlistPath)
		return nil, err
	}
	return &HLSResult{PlaylistPath: playlistPath, SegmentPattern: opts.SegmentPattern, Info: info}, nil
}

func TranscodeHLSFromFile(ctx context.Context, path, playlistPath string, opts HLSOptions) (*HLSResult, error) {
	return TranscodeHLS(ctx, FromFile(path), playlistPath, opts)
}

func TranscodeHLSFromReadSeeker(ctx context.Context, name string, r io.ReadSeeker, playlistPath string, opts HLSOptions) (*HLSResult, error) {
	return TranscodeHLS(ctx, FromReadSeeker(name, r), playlistPath, opts)
}
