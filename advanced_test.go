package transcoder

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCapabilitiesExposeServerFeatures(t *testing.T) {
	c := Capabilities()
	if !c.AudioCopy || !c.DynamicHLS || !c.DynamicDASH || !c.OnDemandSegments || !c.SegmentCache || !c.Prewarm || !c.Auth || !c.RateLimit || !c.Throttling || c.StaticTranscoding {
		t.Fatalf("missing capabilities: %+v", c)
	}
	if len(c.Unsupported) != 2 {
		t.Fatalf("unexpected unsupported list: %+v", c.Unsupported)
	}
}

func TestTranscodeDASHFromFile(t *testing.T) {
	dir := t.TempDir()
	mpd := filepath.Join(dir, "manifest.mpd")
	res, err := TranscodeDASHFromFile(context.Background(), "testdata/sample.mp4", mpd, DASHOptions{TranscodeOptions: TranscodeOptions{Width: 160, FPS: 24, CRF: 32, Preset: "ultrafast"}, SegmentSeconds: 2})
	if err != nil {
		t.Fatalf("dash: %v", err)
	}
	if res.MPDPath != mpd {
		t.Fatalf("mpd=%q", res.MPDPath)
	}
	b, err := os.ReadFile(mpd)
	if err != nil {
		t.Fatalf("missing mpd: %v", err)
	}
	if !strings.Contains(string(b), "MPD") {
		n := len(b)
		if n > 128 {
			n = 128
		}
		t.Fatalf("not an MPD: %s", b[:n])
	}
}

func TestABRHLSWritesMasterPlaylist(t *testing.T) {
	dir := t.TempDir()
	master := filepath.Join(dir, "master.m3u8")
	res, err := TranscodeABRHLSFromFile(context.Background(), "testdata/sample.mp4", master, ABRHLSOptions{Base: HLSOptions{TranscodeOptions: TranscodeOptions{FPS: 24, CRF: 32, Preset: "ultrafast"}, SegmentSeconds: 2}, Variants: []LadderVariant{{Name: "tiny", Width: 160, Height: 90, VideoBitrate: 150000}}})
	if err != nil {
		t.Fatalf("abr hls: %v", err)
	}
	if len(res.Variants) != 1 {
		t.Fatalf("variants=%d", len(res.Variants))
	}
	b, err := os.ReadFile(master)
	if err != nil {
		t.Fatalf("missing master: %v", err)
	}
	if !strings.Contains(string(b), "#EXT-X-STREAM-INF") || !strings.Contains(string(b), "tiny/index.m3u8") {
		t.Fatalf("bad master playlist:\n%s", b)
	}
}

func TestBuildDeviceProfile(t *testing.T) {
	p := BuildDeviceProfile(MediaInfo{Width: 1920, Height: 1080, FPS: 24}, ClientCapabilities{MaxWidth: 1280, Hardware: HWNVENC, DirectPlayVideo: []VideoCodec{VideoH264}}, "out.mp4")
	p.InputPath = "testdata/sample.mp4"
	p.SkipSubtitles = true
	plan, err := BuildPlan(p)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if plan.Progressive.Width != 1280 || plan.EncoderName != "h264_nvenc" || !plan.UsesHardware {
		t.Fatalf("bad plan: %+v", plan)
	}
}

func TestOptionalAudioCopyFromInputWithAudio(t *testing.T) {
	input := os.Getenv("TRANSCODER_TEST_AUDIO_INPUT")
	if input == "" {
		t.Skip("set TRANSCODER_TEST_AUDIO_INPUT to run audio-copy integration test")
	}
	info, err := ProbeFile(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if !info.HasAudio {
		t.Skip("input has no audio")
	}
	out := filepath.Join(t.TempDir(), "audio-copy.mp4")
	_, err = TranscodeProgressiveFromFile(context.Background(), input, out, TranscodeOptions{Width: 320, FPS: 24, CRF: 32, Preset: "ultrafast", AudioMode: AudioCopy})
	if err != nil {
		t.Fatalf("transcode audio copy: %v", err)
	}
	outInfo, err := ProbeFile(context.Background(), out)
	if err != nil {
		t.Fatal(err)
	}
	if !outInfo.HasAudio || outInfo.AudioStreams == 0 {
		t.Fatalf("expected output audio stream, got %+v", outInfo)
	}
}

func TestCenteredCropForAspect(t *testing.T) {
	tests := []struct {
		name string
		info MediaInfo
		want CropRect
	}{
		{name: "2160p", info: MediaInfo{Width: 3840, Height: 2160}, want: CropRect{Width: 3840, Height: 1632, X: 0, Y: 264}},
		{name: "1080p", info: MediaInfo{Width: 1920, Height: 1080}, want: CropRect{Width: 1920, Height: 816, X: 0, Y: 132}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := CenteredCropForAspect(tt.info, "40:17")
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Fatalf("crop = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestParseCropAspectRejectsInvalidValue(t *testing.T) {
	if _, err := ParseCropAspect("2.35"); err == nil {
		t.Fatal("expected invalid crop aspect to fail")
	}
}
