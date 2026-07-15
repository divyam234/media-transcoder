package transcoder

import "strings"

// Preset is a codec-agnostic speed/quality preference.
type Preset string

const (
	PresetFastest  Preset = "fastest"
	PresetFast     Preset = "fast"
	PresetBalanced Preset = "balanced"
	PresetQuality  Preset = "quality"
	PresetBest     Preset = "best"
)

// PresetForEncoder maps a common preset to the native value expected by an encoder.
// Unsupported encoder families return an empty string so no invalid option is sent.
func PresetForEncoder(encoder, preset string) string {
	family := encoderFamily(encoder)
	common := normalizeCommonPreset(preset)

	switch family {
	case "x26x":
		return map[string]string{
			"fastest":  "ultrafast",
			"fast":     "veryfast",
			"balanced": "medium",
			"quality":  "slow",
			"best":     "veryslow",
		}[common]
	case "nvenc":
		return map[string]string{
			"fastest":  "p1",
			"fast":     "p2",
			"balanced": "p4",
			"quality":  "p6",
			"best":     "p7",
		}[common]
	case "qsv":
		return map[string]string{
			"fastest":  "veryfast",
			"fast":     "faster",
			"balanced": "medium",
			"quality":  "slower",
			"best":     "veryslow",
		}[common]
	case "amf":
		return map[string]string{
			"fastest":  "speed",
			"fast":     "speed",
			"balanced": "balanced",
			"quality":  "quality",
			"best":     "quality",
		}[common]
	default:
		return ""
	}
}

func normalizeCommonPreset(preset string) string {
	switch strings.ToLower(strings.TrimSpace(preset)) {
	case "", "fastest", "ultrafast", "superfast", "p1", "veryfast":
		return "fastest"
	case "fast", "faster", "p2", "p3", "speed":
		return "fast"
	case "balanced", "medium", "p4", "p5":
		return "balanced"
	case "quality", "slow", "slower", "p6":
		return "quality"
	case "best", "veryslow", "placebo", "p7":
		return "best"
	default:
		return "balanced"
	}
}

func encoderFamily(encoder string) string {
	name := strings.ToLower(strings.TrimSpace(encoder))
	switch {
	case name == "", name == "libx264", name == "libx265":
		return "x26x"
	case strings.Contains(name, "_nvenc"):
		return "nvenc"
	case strings.Contains(name, "_qsv"):
		return "qsv"
	case strings.Contains(name, "_amf"):
		return "amf"
	default:
		return ""
	}
}
