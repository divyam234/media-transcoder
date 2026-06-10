package transcoder

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"testing"
)

func TestTimestampTrimmedAudioInArbitrarySegment(t *testing.T) {
	input := filepath.Join("testdata", "avsample.mp4")
	if _, err := os.Stat(input); err != nil {
		t.Fatalf("missing audio fixture: %v", err)
	}
	out := filepath.Join(t.TempDir(), "seg.ts")
	_, err := TranscodeSegmentFromFile(context.Background(), input, out, TranscodeOptions{
		Width:        160,
		FPS:          24,
		CRF:          30,
		Preset:       "ultrafast",
		GOPSize:      48,
		StartTime:    1.37,
		Duration:     2.25,
		AudioMode:    AudioTranscode,
		AudioCodec:   "aac",
		AudioBitrate: 96000,
	})
	if err != nil {
		t.Fatalf("transcode arbitrary audio segment: %v", err)
	}
	info, err := ProbeFile(context.Background(), out)
	if err != nil {
		t.Fatalf("probe segment: %v", err)
	}
	if !info.HasAudio || info.AudioStreams != 1 {
		t.Fatalf("segment missing audio: %+v", info)
	}
	if math.Abs(info.Duration-2.25) > 0.30 {
		t.Fatalf("segment duration drift too high: got %.3fs want ~2.25s", info.Duration)
	}
}

func TestTimestampTrimmedAudioInArbitraryFMP4Segment(t *testing.T) {
	input := filepath.Join("testdata", "avsample.mp4")
	if _, err := os.Stat(input); err != nil {
		t.Fatalf("missing audio fixture: %v", err)
	}
	out := filepath.Join(t.TempDir(), "seg.m4s")
	_, err := TranscodeFMP4SegmentFromFile(context.Background(), input, out, TranscodeOptions{
		Width:        160,
		FPS:          24,
		CRF:          30,
		Preset:       "ultrafast",
		GOPSize:      48,
		StartTime:    2.40,
		Duration:     1.75,
		AudioMode:    AudioTranscode,
		AudioCodec:   "aac",
		AudioBitrate: 96000,
	})
	if err != nil {
		t.Fatalf("transcode arbitrary fmp4 audio segment: %v", err)
	}
	info, err := ProbeFile(context.Background(), out)
	if err != nil {
		t.Fatalf("probe fmp4 segment: %v", err)
	}
	if !info.HasAudio || info.AudioStreams != 1 {
		t.Fatalf("fmp4 segment missing audio: %+v", info)
	}
	if math.Abs(info.Duration-1.75) > 0.35 {
		t.Fatalf("fmp4 segment duration drift too high: got %.3fs want ~1.75s", info.Duration)
	}
}
