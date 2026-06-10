package transcoder

import "testing"

func TestRuntimeCapabilitiesExposeFFmpegBuild(t *testing.T) {
	caps, err := RuntimeCapabilities()
	if err != nil {
		t.Fatalf("runtime capabilities: %v", err)
	}
	if caps.FFmpegVersion == "" || caps.LibavcodecVersion == 0 || caps.LibavformatVersion == 0 || caps.LibavutilVersion == 0 {
		t.Fatalf("missing version info: %+v", caps)
	}
	if len(caps.VideoEncoders) == 0 || len(caps.VideoDecoders) == 0 || len(caps.AudioEncoders) == 0 || len(caps.Muxers) == 0 {
		t.Fatalf("missing runtime lists: video_encoders=%d video_decoders=%d audio_encoders=%d muxers=%d", len(caps.VideoEncoders), len(caps.VideoDecoders), len(caps.AudioEncoders), len(caps.Muxers))
	}
	if !contains(caps.AudioEncoders, "aac") {
		t.Fatalf("expected AAC encoder in FFmpeg build, got audio encoders: %v", caps.AudioEncoders)
	}
}

func TestHardwareSupportMatrixReportsBuildAvailability(t *testing.T) {
	caps, err := RuntimeCapabilities()
	if err != nil {
		t.Fatalf("runtime capabilities: %v", err)
	}
	matrix := HardwareSupportMatrix(caps)
	if len(matrix) == 0 {
		t.Fatal("empty Hardware support matrix")
	}
	var softwareH264, nvencH264 bool
	for _, row := range matrix {
		if row.Hardware == HWNone && row.Codec == VideoH264 {
			softwareH264 = true
			if row.EncoderName != "libx264" {
				t.Fatalf("software h264 encoder=%q", row.EncoderName)
			}
			if !row.EncoderAvailableInBuild {
				t.Fatalf("libx264 should be available in supplied FFmpeg build: %+v", row)
			}
		}
		if row.Hardware == HWNVENC && row.Codec == VideoH264 {
			nvencH264 = true
			if row.EncoderName != "h264_nvenc" {
				t.Fatalf("nvenc h264 encoder=%q", row.EncoderName)
			}
		}
	}
	if !softwareH264 || !nvencH264 {
		t.Fatalf("missing expected rows: softwareH264=%v nvencH264=%v matrix=%+v", softwareH264, nvencH264, matrix)
	}
}

func TestCapabilitiesIncludeRuntimeAndProfileMatrix(t *testing.T) {
	c := Capabilities()
	if c.StaticTranscoding {
		t.Fatalf("server must remain dynamic-only")
	}
	if c.Runtime.FFmpegVersion == "" || len(c.Runtime.VideoEncoders) == 0 {
		t.Fatalf("missing runtime capability payload: %+v", c.Runtime)
	}
	if len(c.HardwareSupport) == 0 {
		t.Fatalf("missing Hardware support matrix")
	}
}
