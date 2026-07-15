package transcoder

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestProbeAndProgressiveFromFile(t *testing.T) {
	info, err := ProbeFile(context.Background(), "testdata/sample.mp4")
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if info.Width <= 0 || info.Height <= 0 || info.FPS <= 0 {
		t.Fatalf("bad info: %+v", info)
	}

	out := filepath.Join(t.TempDir(), "progressive.mp4")
	res, err := TranscodeProgressiveFromFile(context.Background(), "testdata/sample.mp4", out, TranscodeOptions{Width: 160, FPS: 24, CRF: 30, Preset: "ultrafast"})
	if err != nil {
		t.Fatalf("transcode: %v", err)
	}
	if res.OutputPath != out {
		t.Fatalf("out path=%q", res.OutputPath)
	}
	st, err := os.Stat(out)
	if err != nil {
		t.Fatalf("missing output: %v", err)
	}
	if st.Size() == 0 {
		t.Fatal("empty output")
	}
}

func TestHLSFromReadSeeker(t *testing.T) {
	data, err := os.ReadFile("testdata/sample.mp4")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	playlist := filepath.Join(dir, "index.m3u8")
	res, err := TranscodeHLSFromReadSeeker(context.Background(), "sample.mp4", bytes.NewReader(data), playlist, HLSOptions{TranscodeOptions: TranscodeOptions{Width: 160, FPS: 24, CRF: 30, Preset: "ultrafast"}, SegmentSeconds: 2})
	if err != nil {
		t.Fatalf("hls: %v", err)
	}
	if res.PlaylistPath != playlist {
		t.Fatalf("playlist=%q", res.PlaylistPath)
	}
	if _, err := os.Stat(playlist); err != nil {
		t.Fatalf("missing playlist: %v", err)
	}
	matches, _ := filepath.Glob(filepath.Join(dir, "index_*.ts"))
	if len(matches) == 0 {
		t.Fatal("expected hls segments")
	}
}

func TestProfilePlanAndDirectTranscode(t *testing.T) {
	out := filepath.Join(t.TempDir(), "profile.mp4")
	profile := Profile{Mode: ModeProgressive, InputPath: "testdata/sample.mp4", OutputPath: out, VideoCodec: VideoH264, Width: 160, FPS: 24, CRF: 30, Preset: "ultrafast", SkipAudio: true, SkipSubtitles: true}
	plan, err := BuildPlan(profile)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if plan.EncoderName != "libx264" {
		t.Fatalf("encoder=%q", plan.EncoderName)
	}
	res, err := TranscodeProfiledDirect(context.Background(), profile)
	if err != nil {
		t.Fatalf("direct: %v", err)
	}
	if res.OutputPath != out {
		t.Fatalf("output=%q", res.OutputPath)
	}
	if _, err := os.Stat(out); err != nil {
		t.Fatalf("missing output: %v", err)
	}
}

func TestHardwareEncoderMatrix(t *testing.T) {
	cases := []struct {
		hw    HardwareAccelerationType
		codec VideoCodec
		want  string
	}{
		{HWNone, VideoH264, "libx264"}, {HWNVENC, VideoH264, "h264_nvenc"}, {HWQSV, VideoHEVC, "hevc_qsv"}, {HWVAAPI, VideoAV1, "av1_vaapi"}, {HWAMF, VideoH264, "h264_amf"}, {HWV4L2M2M, VideoH264, "h264_v4l2m2m"}, {HWVideoToolbox, VideoHEVC, "hevc_videotoolbox"}, {HWRKMPP, VideoHEVC, "hevc_rkmpp"},
	}
	for _, tc := range cases {
		got := VideoEncoder(tc.codec, tc.hw, tc.hw != HWNone)
		if got != tc.want {
			t.Fatalf("VideoEncoder(%s,%s)=%s want %s", tc.codec, tc.hw, got, tc.want)
		}
	}
}

func TestBuildPlanPropagatesVAAPIDevice(t *testing.T) {
	plan, err := BuildPlan(Profile{
		Mode:                     ModeProgressive,
		InputPath:                "input.mkv",
		OutputPath:               "output.mp4",
		VideoCodec:               VideoH264,
		HardwareAccelerationType: HWVAAPI,
		EnableHardwareEncoding:   true,
		EnableHardwareDecoding:   true,
		VaapiDevice:              "/dev/dri/renderD129",
		AudioMode:                AudioSkip,
		SkipSubtitles:            true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Progressive.HardwareDevice != "/dev/dri/renderD129" {
		t.Fatalf("hardware device = %q", plan.Progressive.HardwareDevice)
	}
	if !plan.Progressive.HardwareDecode {
		t.Fatal("hardware decode was not propagated")
	}
}
