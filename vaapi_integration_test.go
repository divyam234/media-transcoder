package transcoder

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestVAAPIRealInput(t *testing.T) {
	input := os.Getenv("INPUT")
	if input == "" {
		t.Skip("set INPUT to a real media file to run the VAAPI integration test")
	}
	device := os.Getenv("DEVICE")
	if device == "" {
		device = "/dev/dri/renderD128"
	}
	output := os.Getenv("OUTPUT")
	if output == "" {
		output = filepath.Join(t.TempDir(), "vaapi.ts")
	}
	if _, err := os.Stat(input); err != nil {
		t.Fatalf("input: %v", err)
	}
	if _, err := os.Stat(device); err != nil {
		t.Fatalf("VAAPI device: %v", err)
	}
	if !EncoderAvailable("h264_vaapi") {
		t.Skip("h264_vaapi is unavailable in this FFmpeg build")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	res, err := TranscodeSegmentFromFile(ctx, input, output, TranscodeOptions{
		EncoderName:    "h264_vaapi",
		HardwareDevice: device,
		HardwareDecode: true,
		Width:          1280,
		CRF:            24,
		GOPSize:        48,
		MaxBFrames:     0,
		AudioMode:      AudioTranscode,
		AudioCodec:     "aac",
		AudioBitrate:   128000,
		AudioChannels:  2,
		StartTime:      60,
		Duration:       4,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.OutputPath != output {
		t.Fatalf("output path = %q, want %q", res.OutputPath, output)
	}
	st, err := os.Stat(output)
	if err != nil {
		t.Fatal(err)
	}
	if st.Size() == 0 {
		t.Fatal("VAAPI output is empty")
	}
}
