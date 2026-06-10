package transcoder

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

type AudioMode string

const (
	AudioSkip      AudioMode = "skip"
	AudioCopy      AudioMode = "copy"
	AudioTranscode AudioMode = "transcode"
)

type ToneMapOptions struct {
	Enabled   bool    `json:"enabled,omitempty"`
	Algorithm string  `json:"algorithm,omitempty"`
	Peak      float64 `json:"peak,omitempty"`
	Desat     float64 `json:"desat,omitempty"`
	Format    string  `json:"format,omitempty"`
}

type RuntimeFFmpegCapabilities struct {
	FFmpegVersion       string   `json:"ffmpeg_version"`
	LibavcodecVersion   uint     `json:"libavcodec_version"`
	LibavformatVersion  uint     `json:"libavformat_version"`
	LibavutilVersion    uint     `json:"libavutil_version"`
	HardwareDeviceTypes []string `json:"hardware_device_types"`
	VideoEncoders       []string `json:"video_encoders"`
	VideoDecoders       []string `json:"video_decoders"`
	AudioEncoders       []string `json:"audio_encoders"`
	AudioDecoders       []string `json:"audio_decoders"`
	Muxers              []string `json:"muxers"`
	Demuxers            []string `json:"demuxers"`
}

type HardwareCodecSupport struct {
	Codec                   VideoCodec               `json:"codec"`
	Hardware                HardwareAccelerationType `json:"hardware"`
	EncoderName             string                   `json:"encoder_name"`
	SupportedByPolicy       bool                     `json:"supported_by_policy"`
	EncoderAvailableInBuild bool                     `json:"encoder_available_in_build"`
	HWDeviceType            string                   `json:"hw_device_type,omitempty"`
	HWDeviceTypeInBuild     bool                     `json:"hw_device_type_in_build"`
	HostDeviceHintAvailable bool                     `json:"host_device_hint_available"`
	RunnableLikely          bool                     `json:"runnable_likely"`
	Notes                   []string                 `json:"notes,omitempty"`
}

type CapabilitySet struct {
	AudioCopy                     bool                       `json:"audio_copy"`
	AudioTranscodeAAC             bool                       `json:"audio_transcode_aac"`
	DynamicHLS                    bool                       `json:"dynamic_hls"`
	DynamicDASH                   bool                       `json:"dynamic_dash"`
	OnDemandSegments              bool                       `json:"on_demand_segments"`
	SegmentCache                  bool                       `json:"segment_cache"`
	Prewarm                       bool                       `json:"prewarm"`
	DynamicABRHLS                 bool                       `json:"dynamic_abr_hls"`
	Metrics                       bool                       `json:"metrics"`
	CooperativeCancel             bool                       `json:"cooperative_cancel"`
	InputRootPolicy               bool                       `json:"input_root_policy"`
	StaticTranscoding             bool                       `json:"static_transcoding"`
	Cancellation                  bool                       `json:"cancellation"`
	ProgressPolling               bool                       `json:"progress_polling"`
	Auth                          bool                       `json:"auth"`
	RateLimit                     bool                       `json:"rate_limit"`
	Throttling                    bool                       `json:"throttling"`
	ToneMapPlanning               bool                       `json:"tone_map_planning"`
	SupportedHardwareAccelerators []HardwareAccelerationType `json:"hardware_accelerators"`
	VideoCodecs                   []VideoCodec               `json:"video_codecs"`
	Unsupported                   []string                   `json:"unsupported"`
	Runtime                       RuntimeFFmpegCapabilities  `json:"runtime"`
	HardwareSupport               []HardwareCodecSupport     `json:"hardware_support"`
}

func Capabilities() CapabilitySet {
	runtime, err := RuntimeCapabilities()
	if err != nil {
		runtime = RuntimeFFmpegCapabilities{}
	}
	return CapabilitySet{
		AudioCopy:                     true,
		AudioTranscodeAAC:             contains(runtime.AudioEncoders, "aac") || EncoderAvailable("aac"),
		DynamicHLS:                    true,
		DynamicDASH:                   true,
		OnDemandSegments:              true,
		SegmentCache:                  true,
		Prewarm:                       true,
		DynamicABRHLS:                 true,
		Metrics:                       true,
		CooperativeCancel:             true,
		InputRootPolicy:               true,
		StaticTranscoding:             false,
		Cancellation:                  true,
		ProgressPolling:               true,
		Auth:                          true,
		RateLimit:                     true,
		Throttling:                    true,
		ToneMapPlanning:               true,
		SupportedHardwareAccelerators: SupportedHardwareAccelerators(),
		VideoCodecs:                   []VideoCodec{VideoH264, VideoHEVC, VideoAV1, VideoMJPEG},
		Unsupported:                   []string{"subtitle burn-in/extract/embed", "websocket progress"},
		Runtime:                       runtime,
		HardwareSupport:               HardwareSupportMatrix(runtime),
	}
}

func RuntimeCapabilities() (RuntimeFFmpegCapabilities, error) {
	caps, err := runtimeFFmpegCapabilities()
	if err != nil {
		return caps, err
	}
	sort.Strings(caps.HardwareDeviceTypes)
	sort.Strings(caps.VideoEncoders)
	sort.Strings(caps.VideoDecoders)
	sort.Strings(caps.AudioEncoders)
	sort.Strings(caps.AudioDecoders)
	sort.Strings(caps.Muxers)
	sort.Strings(caps.Demuxers)
	return caps, nil
}

func SupportedHardwareAccelerators() []HardwareAccelerationType {
	return []HardwareAccelerationType{HWNone, HWAMF, HWQSV, HWNVENC, HWV4L2M2M, HWVAAPI, HWVideoToolbox, HWRKMPP}
}

func HardwareSupportMatrix(runtime RuntimeFFmpegCapabilities) []HardwareCodecSupport {
	if len(runtime.VideoEncoders) == 0 && len(runtime.HardwareDeviceTypes) == 0 {
		runtime, _ = RuntimeCapabilities()
	}
	codecs := []VideoCodec{VideoH264, VideoHEVC, VideoAV1, VideoMJPEG}
	hws := SupportedHardwareAccelerators()
	out := make([]HardwareCodecSupport, 0, len(codecs)*len(hws))
	for _, hw := range hws {
		for _, codec := range codecs {
			enc := VideoEncoder(codec, hw, hw != HWNone)
			if enc == "" {
				continue
			}
			deviceType := hardwareDeviceType(hw)
			encoderOK := contains(runtime.VideoEncoders, enc) || EncoderAvailable(enc)
			deviceTypeOK := deviceType == "" || contains(runtime.HardwareDeviceTypes, deviceType)
			hostHint := hostDeviceHintAvailable(hw)
			item := HardwareCodecSupport{Codec: codec, Hardware: hw, EncoderName: enc, SupportedByPolicy: supportedByPolicy(hw, codec), EncoderAvailableInBuild: encoderOK, HWDeviceType: deviceType, HWDeviceTypeInBuild: deviceTypeOK, HostDeviceHintAvailable: hostHint}
			if hw == HWNone {
				item.RunnableLikely = encoderOK
			} else {
				item.RunnableLikely = encoderOK && deviceTypeOK && hostHint
			}
			if !item.SupportedByPolicy {
				item.Notes = append(item.Notes, "not part of this server's hardware acceleration policy matrix")
			}
			if !encoderOK {
				item.Notes = append(item.Notes, "encoder is not present in this FFmpeg build")
			}
			if hw != HWNone && !deviceTypeOK {
				item.Notes = append(item.Notes, "FFmpeg build does not expose the required HW device type")
			}
			if hw != HWNone && !hostHint {
				item.Notes = append(item.Notes, "host hardware/device hint was not found; build support alone may still not run")
			}
			out = append(out, item)
		}
	}
	return out
}

func supportedByPolicy(hw HardwareAccelerationType, codec VideoCodec) bool {
	switch hw {
	case HWNone:
		return codec == VideoH264 || codec == VideoHEVC || codec == VideoAV1 || codec == VideoMJPEG
	case HWNVENC, HWQSV, HWVAAPI, HWAMF:
		return codec == VideoH264 || codec == VideoHEVC || codec == VideoAV1
	case HWV4L2M2M, HWVideoToolbox, HWRKMPP:
		return codec == VideoH264 || codec == VideoHEVC
	default:
		return false
	}
}

func hardwareDeviceType(hw HardwareAccelerationType) string {
	switch hw {
	case HWNVENC:
		return "cuda"
	case HWQSV:
		return "qsv"
	case HWVAAPI:
		return "vaapi"
	case HWAMF:
		return "d3d11va"
	case HWV4L2M2M:
		return "drm"
	case HWVideoToolbox:
		return "videotoolbox"
	case HWRKMPP:
		return "drm"
	default:
		return ""
	}
}

func hostDeviceHintAvailable(hw HardwareAccelerationType) bool {
	switch hw {
	case "", HWNone:
		return true
	case HWNVENC:
		return fileExists("/dev/nvidia0") || fileExists("/proc/driver/nvidia/version")
	case HWVAAPI, HWQSV:
		return fileExists("/dev/dri/renderD128") || globExists("/dev/dri/renderD*")
	case HWV4L2M2M:
		return globExists("/dev/video*")
	case HWRKMPP:
		return fileExists("/dev/mpp_service") || globExists("/dev/rga*") || globExists("/dev/dri/renderD*")
	case HWVideoToolbox:
		return false // only meaningful on Apple platforms; Linux sandbox cannot expose it.
	case HWAMF:
		return false // normally Windows/AMD path; Linux sandbox cannot prove it.
	default:
		return false
	}
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func globExists(pattern string) bool {
	matches, err := filepath.Glob(pattern)
	return err == nil && len(matches) > 0
}

func contains[T comparable](items []T, needle T) bool {
	for _, item := range items {
		if item == needle {
			return true
		}
	}
	return false
}

func validateAudioMode(mode AudioMode) error {
	switch mode {
	case "", AudioSkip, AudioCopy, AudioTranscode:
		return nil
	default:
		return fmt.Errorf("unsupported audio_mode %q", mode)
	}
}
