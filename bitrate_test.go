package transcoder

import (
	"encoding/json"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestParseBitrate(t *testing.T) {
	tests := map[string]Bitrate{
		"128000":   128000,
		"128k":     128000,
		"128 kbps": 128000,
		"2.8M":     2800000,
		"2.8Mbps":  2800000,
		"1Gbit/s":  1000000000,
	}
	for input, want := range tests {
		got, err := ParseBitrate(input)
		if err != nil {
			t.Fatalf("ParseBitrate(%q): %v", input, err)
		}
		if got != want {
			t.Fatalf("ParseBitrate(%q)=%d want %d", input, got, want)
		}
	}
}

func TestBitrateYAMLAndJSON(t *testing.T) {
	var yamlValue struct {
		Bitrate Bitrate `yaml:"bitrate"`
	}
	if err := yaml.Unmarshal([]byte("bitrate: 2.8Mbps\n"), &yamlValue); err != nil {
		t.Fatal(err)
	}
	if yamlValue.Bitrate != 2800000 {
		t.Fatalf("yaml bitrate=%d", yamlValue.Bitrate)
	}

	var jsonValue struct {
		Bitrate Bitrate `json:"bitrate"`
	}
	if err := json.Unmarshal([]byte(`{"bitrate":"128kbps"}`), &jsonValue); err != nil {
		t.Fatal(err)
	}
	if jsonValue.Bitrate != 128000 {
		t.Fatalf("json bitrate=%d", jsonValue.Bitrate)
	}
}
