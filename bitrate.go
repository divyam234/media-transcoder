package transcoder

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"unicode"

	"gopkg.in/yaml.v3"
)

// Bitrate stores bits per second while accepting human-readable config values
// such as 128k, 2.8M, 128kbps, and 2.8Mbps.
type Bitrate int

func ParseBitrate(value string) (Bitrate, error) {
	s := strings.TrimSpace(value)
	if s == "" {
		return 0, nil
	}
	compact := strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) || r == '_' {
			return -1
		}
		return unicode.ToLower(r)
	}, s)

	multiplier := float64(1)
	suffixes := []struct {
		suffix string
		mult   float64
	}{
		{"gbit/s", 1e9}, {"gbps", 1e9}, {"g", 1e9},
		{"mbit/s", 1e6}, {"mbps", 1e6}, {"m", 1e6},
		{"kbit/s", 1e3}, {"kbps", 1e3}, {"k", 1e3},
		{"bit/s", 1}, {"bps", 1},
	}
	for _, candidate := range suffixes {
		if strings.HasSuffix(compact, candidate.suffix) {
			compact = strings.TrimSuffix(compact, candidate.suffix)
			multiplier = candidate.mult
			break
		}
	}
	if compact == "" {
		return 0, fmt.Errorf("invalid bitrate %q", value)
	}
	number, err := strconv.ParseFloat(compact, 64)
	if err != nil || number < 0 {
		return 0, fmt.Errorf("invalid bitrate %q", value)
	}
	bits := number * multiplier
	if bits > float64(math.MaxInt) {
		return 0, fmt.Errorf("bitrate %q overflows int", value)
	}
	return Bitrate(math.Round(bits)), nil
}

func (b *Bitrate) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.ScalarNode {
		return fmt.Errorf("bitrate must be a scalar")
	}
	parsed, err := ParseBitrate(node.Value)
	if err != nil {
		return err
	}
	*b = parsed
	return nil
}

func (b *Bitrate) UnmarshalJSON(data []byte) error {
	var text string
	if len(data) > 0 && data[0] == '"' {
		if err := json.Unmarshal(data, &text); err != nil {
			return err
		}
	} else {
		text = string(data)
	}
	parsed, err := ParseBitrate(text)
	if err != nil {
		return err
	}
	*b = parsed
	return nil
}

func (b Bitrate) MarshalJSON() ([]byte, error) { return json.Marshal(int(b)) }
