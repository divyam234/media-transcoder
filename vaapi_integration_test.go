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

func TestVAAPICropAspectPipeline(t *testing.T) {
	if os.Getenv("TRANSCODER_TEST_VAAPI_CROP") == "" {
		t.Skip("set TRANSCODER_TEST_VAAPI_CROP=1 to run the VAAPI crop integration test")
	}
	device := "/dev/dri/renderD128"
	if _, err := os.Stat(device); err != nil {
		t.Skipf("VAAPI device unavailable: %v", err)
	}
	if !EncoderAvailable("h264_vaapi") {
		t.Skip("h264_vaapi is unavailable in this FFmpeg build")
	}
	input := "testdata/sample.mp4"
	info, err := ProbeFile(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	crop, err := CenteredCropForAspect(info, "40:17")
	if err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(t.TempDir(), "vaapi-crop.ts")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err = TranscodeSegmentFromFile(ctx, input, output, TranscodeOptions{
		EncoderName:    "h264_vaapi",
		HardwareDevice: device,
		HardwareDecode: true,
		Width:          160,
		CropWidth:      crop.Width,
		CropHeight:     crop.Height,
		CropX:          crop.X,
		CropY:          crop.Y,
		CRF:            28,
		GOPSize:        48,
		AudioMode:      AudioSkip,
		Duration:       1,
	})
	if err != nil {
		t.Fatal(err)
	}
	outInfo, err := ProbeFile(context.Background(), output)
	if err != nil {
		t.Fatal(err)
	}
	if outInfo.Width != 160 || outInfo.Height != 68 {
		t.Fatalf("cropped VAAPI output = %dx%d, want 160x68", outInfo.Width, outInfo.Height)
	}
}
