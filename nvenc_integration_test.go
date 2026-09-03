package transcoder

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestNVENCHardwareDecodeCropPipeline(t *testing.T) {
	if os.Getenv("TRANSCODER_TEST_NVENC") == "" {
		t.Skip("set TRANSCODER_TEST_NVENC=1 to run the NVIDIA hardware decode integration test")
	}
	if !EncoderAvailable("h264_nvenc") {
		t.Skip("h264_nvenc is unavailable in this FFmpeg build")
	}

	crop, err := CenteredCropForAspect(MediaInfo{Width: 320, Height: 180}, "40:17")
	if err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(t.TempDir(), "nvenc.ts")
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	res, err := TranscodeSegmentFromFile(ctx, "testdata/sample.mp4", output, TranscodeOptions{
		EncoderName:    "h264_nvenc",
		HardwareDevice: "0",
		HardwareDecode: true,
		Width:          160,
		CropWidth:      crop.Width,
		CropHeight:     crop.Height,
		CropX:          crop.X,
		CropY:          crop.Y,
		CRF:            28,
		Preset:         "p1",
		GOPSize:        48,
		MaxBFrames:     0,
		AudioMode:      AudioSkip,
		Duration:       1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.OutputPath != output {
		t.Fatalf("output path = %q, want %q", res.OutputPath, output)
	}
	info, err := ProbeFile(context.Background(), output)
	if err != nil {
		t.Fatal(err)
	}
	if info.Width != 160 || info.Height != 68 {
		t.Fatalf("cropped NVENC output = %dx%d, want 160x68", info.Width, info.Height)
	}
}
