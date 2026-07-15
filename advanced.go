package transcoder

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

type DASHOptions struct {
	TranscodeOptions
	SegmentSeconds float64 `json:"segment_seconds,omitempty"`
}

type DASHResult struct {
	MPDPath string    `json:"mpd_path"`
	Info    MediaInfo `json:"info"`
}

func (o *DASHOptions) ApplyDefaults() {
	o.TranscodeOptions.ApplyDefaults()
	if o.SegmentSeconds <= 0 {
		o.SegmentSeconds = 4
	}
}

func TranscodeDASH(ctx context.Context, src Source, mpdPath string, opts DASHOptions) (*DASHResult, error) {
	if err := checkContext(ctx); err != nil {
		return nil, err
	}
	if mpdPath == "" {
		return nil, fmt.Errorf("mpd path is required")
	}
	opts.ApplyDefaults()
	prepared, err := prepareSource(ctx, src)
	if err != nil {
		return nil, err
	}
	if prepared.cleanup != nil {
		defer prepared.cleanup()
	}
	if err := os.MkdirAll(filepath.Dir(mpdPath), 0o755); err != nil {
		return nil, err
	}
	info, err := Probe(prepared.path)
	if err != nil {
		return nil, err
	}
	if err := transcodeDASHVideo(ctx, prepared.path, mpdPath, opts); err != nil {
		_ = os.Remove(mpdPath)
		return nil, err
	}
	return &DASHResult{MPDPath: mpdPath, Info: info}, nil
}
func TranscodeDASHFromFile(ctx context.Context, path, mpdPath string, opts DASHOptions) (*DASHResult, error) {
	return TranscodeDASH(ctx, FromFile(path), mpdPath, opts)
}

type LadderVariant struct {
	Name         string  `json:"name" yaml:"name"`
	Width        int     `json:"width" yaml:"width"`
	Height       int     `json:"height,omitempty" yaml:"height,omitempty"`
	VideoBitrate Bitrate `json:"video_bitrate,omitempty" yaml:"video_bitrate,omitempty"`
	AudioBitrate Bitrate `json:"audio_bitrate,omitempty" yaml:"audio_bitrate,omitempty"`
	FPS          float64 `json:"fps,omitempty" yaml:"fps,omitempty"`
	CRF          int     `json:"crf,omitempty" yaml:"crf,omitempty"`
}

type ABRHLSOptions struct {
	Base     HLSOptions      `json:"base"`
	Variants []LadderVariant `json:"variants"`
}

type ABRVariantResult struct {
	Variant      LadderVariant `json:"variant"`
	PlaylistPath string        `json:"playlist_path"`
}

type ABRHLSResult struct {
	MasterPlaylist string             `json:"master_playlist"`
	Variants       []ABRVariantResult `json:"variants"`
	Info           MediaInfo          `json:"info"`
}

func DefaultLadder() []LadderVariant {
	return []LadderVariant{{Name: "360p", Width: 640, Height: 360, VideoBitrate: 900000, AudioBitrate: 96000, CRF: 28}, {Name: "480p", Width: 854, Height: 480, VideoBitrate: 1400000, AudioBitrate: 128000, CRF: 27}, {Name: "720p", Width: 1280, Height: 720, VideoBitrate: 2800000, AudioBitrate: 128000, CRF: 26}}
}

func TranscodeABRHLSFromFile(ctx context.Context, inputPath, masterPlaylist string, opts ABRHLSOptions) (*ABRHLSResult, error) {
	if len(opts.Variants) == 0 {
		opts.Variants = DefaultLadder()
	}
	if masterPlaylist == "" {
		return nil, fmt.Errorf("master playlist path is required")
	}
	if err := os.MkdirAll(filepath.Dir(masterPlaylist), 0o755); err != nil {
		return nil, err
	}
	info, err := ProbeFile(ctx, inputPath)
	if err != nil {
		return nil, err
	}
	results := make([]ABRVariantResult, 0, len(opts.Variants))
	for _, variant := range opts.Variants {
		name := variant.Name
		if name == "" {
			name = fmt.Sprintf("%dp", variant.Height)
		}
		playlist := filepath.Join(filepath.Dir(masterPlaylist), name, "index.m3u8")
		hls := opts.Base
		hls.Width = variant.Width
		if variant.FPS > 0 {
			hls.FPS = variant.FPS
		}
		if variant.CRF > 0 {
			hls.CRF = variant.CRF
		}
		if variant.AudioBitrate > 0 {
			hls.AudioBitrate = int(variant.AudioBitrate)
		}
		if hls.AudioMode == "" {
			hls.AudioMode = AudioCopy
		}
		if _, err := TranscodeHLSFromFile(ctx, inputPath, playlist, hls); err != nil {
			return nil, fmt.Errorf("variant %s: %w", name, err)
		}
		variant.Name = name
		results = append(results, ABRVariantResult{Variant: variant, PlaylistPath: playlist})
	}
	if err := writeMasterPlaylist(masterPlaylist, results); err != nil {
		return nil, err
	}
	return &ABRHLSResult{MasterPlaylist: masterPlaylist, Variants: results, Info: info}, nil
}

func writeMasterPlaylist(path string, variants []ABRVariantResult) error {
	sort.SliceStable(variants, func(i, j int) bool { return variants[i].Variant.Width < variants[j].Variant.Width })
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	fmt.Fprintln(f, "#EXTM3U")
	fmt.Fprintln(f, "#EXT-X-VERSION:3")
	for _, v := range variants {
		bw := v.Variant.VideoBitrate + v.Variant.AudioBitrate
		if bw <= 0 {
			bw = Bitrate(v.Variant.Width * 2200)
		}
		res := ""
		if v.Variant.Width > 0 && v.Variant.Height > 0 {
			res = fmt.Sprintf(",RESOLUTION=%dx%d", v.Variant.Width, v.Variant.Height)
		}
		fmt.Fprintf(f, "#EXT-X-STREAM-INF:BANDWIDTH=%d%s\n", bw, res)
		rel, err := filepath.Rel(filepath.Dir(path), v.PlaylistPath)
		if err != nil {
			rel = v.PlaylistPath
		}
		fmt.Fprintln(f, filepath.ToSlash(rel))
	}
	return nil
}

type ClientCapabilities struct {
	MaxWidth        int                      `json:"max_width,omitempty"`
	MaxHeight       int                      `json:"max_height,omitempty"`
	MaxBitrate      int                      `json:"max_bitrate,omitempty"`
	DirectPlayVideo []VideoCodec             `json:"direct_play_video,omitempty"`
	AudioCodecs     []string                 `json:"audio_codecs,omitempty"`
	Hardware        HardwareAccelerationType `json:"hardware,omitempty"`
}

func BuildDeviceProfile(input MediaInfo, caps ClientCapabilities, outPath string) Profile {
	width := input.Width
	if caps.MaxWidth > 0 && width > caps.MaxWidth {
		width = caps.MaxWidth
	}
	fps := input.FPS
	if fps <= 0 {
		fps = 30
	}
	codec := VideoH264
	if len(caps.DirectPlayVideo) > 0 {
		codec = caps.DirectPlayVideo[0]
	}
	return Profile{Mode: ModeProgressive, OutputPath: outPath, VideoCodec: codec, Width: width, FPS: fps, HardwareAccelerationType: caps.Hardware, EnableHardwareEncoding: caps.Hardware != "" && caps.Hardware != HWNone, AudioMode: AudioCopy, SkipSubtitles: true, FastStart: true}
}
