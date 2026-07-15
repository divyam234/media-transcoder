package transcoder

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"strings"
)

type HardwareAccelerationType string

const (
	HWNone         HardwareAccelerationType = "none"
	HWAMF          HardwareAccelerationType = "amf"
	HWQSV          HardwareAccelerationType = "qsv"
	HWNVENC        HardwareAccelerationType = "nvenc"
	HWV4L2M2M      HardwareAccelerationType = "v4l2m2m"
	HWVAAPI        HardwareAccelerationType = "vaapi"
	HWVideoToolbox HardwareAccelerationType = "videotoolbox"
	HWRKMPP        HardwareAccelerationType = "rkmpp"
)

type TranscodeMode string

const (
	ModeProgressive TranscodeMode = "progressive"
	ModeHLS         TranscodeMode = "hls"
)

type VideoCodec string

const (
	VideoH264  VideoCodec = "h264"
	VideoHEVC  VideoCodec = "hevc"
	VideoAV1   VideoCodec = "av1"
	VideoMJPEG VideoCodec = "mjpeg"
)

// Profile is a direct-libav transcode profile. Unsupported paths
// are rejected rather than executing ffmpeg as a child process.
type Profile struct {
	Mode       TranscodeMode `json:"mode"`
	InputPath  string        `json:"input_path"`
	OutputPath string        `json:"output_path"`

	VideoCodec VideoCodec `json:"video_codec"`
	Width      int        `json:"width,omitempty"`
	Height     int        `json:"height,omitempty"` // reserved; current direct filter keeps aspect via width
	FPS        float64    `json:"fps,omitempty"`
	CRF        int        `json:"crf,omitempty"`
	Preset     string     `json:"preset,omitempty"`
	GOPSize    int        `json:"gop_size,omitempty"`
	MaxBFrames int        `json:"max_b_frames,omitempty"`
	Threads    int        `json:"threads,omitempty"` // reserved; libav encoder thread_count currently auto

	HardwareAccelerationType HardwareAccelerationType `json:"hardware_acceleration_type"`
	EnableHardwareEncoding   bool                     `json:"enable_hardware_encoding,omitempty"`
	EnableHardwareDecoding   bool                     `json:"enable_hardware_decoding,omitempty"` // reserved
	VaapiDevice              string                   `json:"vaapi_device,omitempty"`
	QsvDevice                string                   `json:"qsv_device,omitempty"`
	HardwareDeviceIndex      int                      `json:"hardware_device_index,omitempty"`

	AudioMode     AudioMode `json:"audio_mode,omitempty"`
	SkipAudio     bool      `json:"skip_audio,omitempty"`
	AudioCodec    string    `json:"audio_codec,omitempty"`
	AudioBitrate  int       `json:"audio_bitrate,omitempty"`
	AudioChannels int       `json:"audio_channels,omitempty"`
	CopySubtitles bool      `json:"copy_subtitles,omitempty"`
	SkipSubtitles bool      `json:"skip_subtitles,omitempty"`

	SegmentSeconds float64 `json:"segment_seconds,omitempty"`
	SegmentPattern string  `json:"segment_pattern,omitempty"`
	PlaylistType   string  `json:"playlist_type,omitempty"`
	ListSize       int     `json:"list_size,omitempty"`
	SegmentType    string  `json:"segment_type,omitempty"`
	FastStart      bool    `json:"fast_start,omitempty"`
}

type Plan struct {
	Mode         TranscodeMode            `json:"mode"`
	InputPath    string                   `json:"input_path"`
	OutputPath   string                   `json:"output_path"`
	EncoderName  string                   `json:"encoder_name"`
	VideoCodec   VideoCodec               `json:"video_codec"`
	Hardware     HardwareAccelerationType `json:"hardware"`
	UsesHardware bool                     `json:"uses_hardware"`
	Progressive  TranscodeOptions         `json:"progressive"`
	HLS          HLSOptions               `json:"hls,omitempty"`
	Warnings     []string                 `json:"warnings,omitempty"`
}

type ProfileTranscodeResult struct {
	Plan       Plan      `json:"plan"`
	OutputPath string    `json:"output_path"`
	Info       MediaInfo `json:"info"`
}

func (p *Profile) applyDefaults() {
	if p.Mode == "" {
		p.Mode = ModeProgressive
	}
	if p.VideoCodec == "" {
		p.VideoCodec = VideoH264
	}
	if p.HardwareAccelerationType == "" {
		p.HardwareAccelerationType = HWNone
	}
	if p.CRF <= 0 {
		p.CRF = 28
	}
	if p.Preset == "" {
		p.Preset = "veryfast"
	}
	if p.GOPSize <= 0 {
		p.GOPSize = 48
	}
	if p.VaapiDevice == "" {
		p.VaapiDevice = "/dev/dri/renderD128"
	}
	if p.SegmentSeconds <= 0 {
		p.SegmentSeconds = 4
	}
	if p.PlaylistType == "" {
		p.PlaylistType = "vod"
	}
	if p.AudioMode == "" {
		if p.SkipAudio {
			p.AudioMode = AudioSkip
		} else {
			p.AudioMode = AudioCopy
		}
	}
	if p.SegmentType == "" {
		p.SegmentType = "mpegts"
	}
}

func VideoEncoder(codec VideoCodec, hw HardwareAccelerationType, enableHW bool) string {
	if !enableHW || hw == "" || hw == HWNone {
		switch codec {
		case VideoHEVC:
			return "libx265"
		case VideoAV1:
			return "libsvtav1"
		case VideoMJPEG:
			return "mjpeg"
		default:
			return "libx264"
		}
	}
	switch hw {
	case HWNVENC:
		switch codec {
		case VideoHEVC:
			return "hevc_nvenc"
		case VideoAV1:
			return "av1_nvenc"
		default:
			return "h264_nvenc"
		}
	case HWQSV:
		switch codec {
		case VideoHEVC:
			return "hevc_qsv"
		case VideoAV1:
			return "av1_qsv"
		default:
			return "h264_qsv"
		}
	case HWVAAPI:
		switch codec {
		case VideoHEVC:
			return "hevc_vaapi"
		case VideoAV1:
			return "av1_vaapi"
		default:
			return "h264_vaapi"
		}
	case HWAMF:
		switch codec {
		case VideoHEVC:
			return "hevc_amf"
		case VideoAV1:
			return "av1_amf"
		default:
			return "h264_amf"
		}
	case HWV4L2M2M:
		switch codec {
		case VideoHEVC:
			return "hevc_v4l2m2m"
		default:
			return "h264_v4l2m2m"
		}
	case HWVideoToolbox:
		switch codec {
		case VideoHEVC:
			return "hevc_videotoolbox"
		default:
			return "h264_videotoolbox"
		}
	case HWRKMPP:
		switch codec {
		case VideoHEVC:
			return "hevc_rkmpp"
		default:
			return "h264_rkmpp"
		}
	default:
		return "libx264"
	}
}

func directPresetForEncoder(encoder, preset string) string {
	if preset == "" {
		preset = "veryfast"
	}
	if strings.Contains(encoder, "_vaapi") || strings.Contains(encoder, "_amf") || strings.Contains(encoder, "_videotoolbox") {
		return preset
	}
	return preset
}

func BuildPlan(profile Profile) (Plan, error) {
	profile.applyDefaults()
	if profile.InputPath == "" {
		return Plan{}, fmt.Errorf("input path is required")
	}
	if profile.OutputPath == "" {
		return Plan{}, fmt.Errorf("output path is required")
	}
	if err := validateAudioMode(profile.AudioMode); err != nil {
		return Plan{}, err
	}
	if profile.CopySubtitles || !profile.SkipSubtitles {
		return Plan{}, fmt.Errorf("subtitle handling is not implemented in the direct bridge yet; set skip_subtitles=true")
	}
	encoder := VideoEncoder(profile.VideoCodec, profile.HardwareAccelerationType, profile.EnableHardwareEncoding)
	base := TranscodeOptions{EncoderName: encoder, HardwareDevice: profile.VaapiDevice, HardwareDecode: profile.EnableHardwareDecoding, Width: profile.Width, Height: profile.Height, FPS: profile.FPS, CRF: profile.CRF, Preset: directPresetForEncoder(encoder, profile.Preset), GOPSize: profile.GOPSize, MaxBFrames: profile.MaxBFrames, FastStart: profile.FastStart || profile.Mode == ModeProgressive, AudioMode: profile.AudioMode, AudioCodec: profile.AudioCodec, AudioBitrate: profile.AudioBitrate, AudioChannels: profile.AudioChannels}
	plan := Plan{Mode: profile.Mode, InputPath: profile.InputPath, OutputPath: profile.OutputPath, EncoderName: encoder, VideoCodec: profile.VideoCodec, Hardware: profile.HardwareAccelerationType, UsesHardware: profile.EnableHardwareEncoding && profile.HardwareAccelerationType != HWNone, Progressive: base}
	if plan.UsesHardware {
		plan.Warnings = append(plan.Warnings, "hardware accelerator maps to a libavcodec hardware encoder; execution requires matching host GPU/device and FFmpeg build support")
	}
	switch profile.Mode {
	case ModeProgressive:
		return plan, nil
	case ModeHLS:
		segPattern := profile.SegmentPattern
		if segPattern == "" {
			ext := ".ts"
			if profile.SegmentType == "fmp4" {
				ext = ".m4s"
			}
			dir := filepath.Dir(profile.OutputPath)
			baseName := strings.TrimSuffix(filepath.Base(profile.OutputPath), filepath.Ext(profile.OutputPath))
			segPattern = filepath.Join(dir, baseName+"_%05d"+ext)
		}
		plan.HLS = HLSOptions{TranscodeOptions: base, SegmentSeconds: profile.SegmentSeconds, SegmentPattern: segPattern, SegmentType: profile.SegmentType, PlaylistType: profile.PlaylistType, ListSize: profile.ListSize}
		plan.HLS.FastStart = false
		return plan, nil
	default:
		return Plan{}, fmt.Errorf("unsupported mode %q", profile.Mode)
	}
}

func TranscodeProfiledDirect(ctx context.Context, profile Profile) (*ProfileTranscodeResult, error) {
	plan, err := BuildPlan(profile)
	if err != nil {
		return nil, err
	}
	var info MediaInfo
	switch plan.Mode {
	case ModeProgressive:
		res, err := TranscodeProgressiveFromFile(ctx, plan.InputPath, plan.OutputPath, plan.Progressive)
		if err != nil {
			return nil, err
		}
		info = res.Info
	case ModeHLS:
		res, err := TranscodeHLSFromFile(ctx, plan.InputPath, plan.OutputPath, plan.HLS)
		if err != nil {
			return nil, err
		}
		info = res.Info
	default:
		return nil, fmt.Errorf("unsupported mode %q", plan.Mode)
	}
	return &ProfileTranscodeResult{Plan: plan, OutputPath: plan.OutputPath, Info: info}, nil
}

func TranscodeProfiledDirectFromReadSeeker(ctx context.Context, name string, r io.ReadSeeker, profile Profile) (*ProfileTranscodeResult, error) {
	if profile.InputPath == "" {
		profile.InputPath = name
	}
	plan, err := BuildPlan(profile)
	if err != nil {
		return nil, err
	}
	var info MediaInfo
	switch plan.Mode {
	case ModeProgressive:
		res, err := TranscodeProgressiveFromReadSeeker(ctx, name, r, plan.OutputPath, plan.Progressive)
		if err != nil {
			return nil, err
		}
		info = res.Info
	case ModeHLS:
		res, err := TranscodeHLSFromReadSeeker(ctx, name, r, plan.OutputPath, plan.HLS)
		if err != nil {
			return nil, err
		}
		info = res.Info
	}
	return &ProfileTranscodeResult{Plan: plan, OutputPath: plan.OutputPath, Info: info}, nil
}
