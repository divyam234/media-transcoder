package transcoder

import "testing"

func TestPresetForEncoder(t *testing.T) {
	tests := []struct {
		encoder string
		preset  string
		want    string
	}{
		{"libx264", "fastest", "ultrafast"},
		{"libx265", "quality", "slow"},
		{"h264_nvenc", "fastest", "p1"},
		{"hevc_nvenc", "ultrafast", "p1"},
		{"h264_nvenc", "balanced", "p4"},
		{"h264_nvenc", "best", "p7"},
		{"h264_qsv", "fast", "faster"},
		{"hevc_qsv", "best", "veryslow"},
		{"h264_amf", "fastest", "speed"},
		{"h264_amf", "quality", "quality"},
		{"h264_vaapi", "fastest", ""},
		{"h264_videotoolbox", "quality", ""},
	}
	for _, tc := range tests {
		if got := PresetForEncoder(tc.encoder, tc.preset); got != tc.want {
			t.Fatalf("PresetForEncoder(%q, %q)=%q want %q", tc.encoder, tc.preset, got, tc.want)
		}
	}
}
