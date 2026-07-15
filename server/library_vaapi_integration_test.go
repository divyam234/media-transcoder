package server

import (
	"bytes"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	transcoder "media-transcoder"
)

func TestOptionalLibraryHLSVAAPIZeroCopyFlow(t *testing.T) {
	input := os.Getenv("TRANSCODER_TEST_VAAPI_INPUT")
	if input == "" {
		input = "/home/bhunter/Downloads/Lucky 2026 S01E01 1080p 10bit WEBRip 6CH x265 HEVC-PSA.mkv"
	}
	device := os.Getenv("TRANSCODER_TEST_VAAPI_DEVICE")
	if device == "" {
		device = "/dev/dri/renderD128"
	}
	if _, err := os.Stat(input); err != nil {
		t.Skipf("VAAPI input unavailable: %v", err)
	}
	if _, err := os.Stat(device); err != nil {
		t.Skipf("VAAPI device unavailable: %v", err)
	}
	if !transcoder.EncoderAvailable("h264_vaapi") {
		t.Skip("h264_vaapi unavailable in this FFmpeg build")
	}

	root := filepath.Dir(input)
	name := filepath.Base(input)
	cfg := Config{
		RequestTimeout:    2 * time.Minute,
		MaxConcurrentJobs: 2,
		CacheRoot:         t.TempDir(),
		AllowedInputRoots: []string{root},
		Libraries: map[string]LibraryConfig{
			"media": {VFS: root},
		},
		Profiles: map[string]PlaybackProfile{
			"vaapi": {
				Container:      "hls",
				SegmentType:    "fmp4",
				SegmentSeconds: 4,
				Audio: AudioProfile{
					Mode:     transcoder.AudioTranscode,
					Codec:    "aac",
					Bitrate:  128000,
					Channels: 2,
				},
				Video: VideoProfile{
					EncoderName:    "h264_vaapi",
					HardwareDevice: device,
					HardwareDecode: true,
					CRF:            24,
					GOPSize:        96,
					MaxBFrames:     0,
				},
				Variants: []transcoder.LadderVariant{{Name: "720p", Width: 1280, VideoBitrate: 2800000, CRF: 24}},
			},
		},
	}
	s := New(cfg)
	t.Cleanup(s.Close)
	base := "/play/hls/vaapi/media/" + url.PathEscape(name) + "/variant/720p"

	playlist := request(t, s, base+"/video.m3u8")
	if playlist.Code != http.StatusOK {
		t.Fatalf("playlist status=%d body=%s", playlist.Code, playlist.Body.String())
	}
	init := request(t, s, base+"/segment/init.mp4")
	if init.Code != http.StatusOK || !bytes.Contains(init.Body.Bytes(), []byte("moov")) {
		t.Fatalf("init status=%d bytes=%d", init.Code, init.Body.Len())
	}
	seg0 := request(t, s, base+"/segment/000000.m4s")
	if seg0.Code != http.StatusOK || !bytes.Contains(seg0.Body.Bytes(), []byte("moof")) {
		t.Fatalf("segment 0 status=%d bytes=%d", seg0.Code, seg0.Body.Len())
	}
	seg15 := request(t, s, base+"/segment/000015.m4s")
	if seg15.Code != http.StatusOK || !bytes.Contains(seg15.Body.Bytes(), []byte("moof")) {
		t.Fatalf("seek segment status=%d bytes=%d body=%s", seg15.Code, seg15.Body.Len(), seg15.Body.String())
	}
	again := request(t, s, base+"/segment/000015.m4s")
	if again.Code != http.StatusOK || !bytes.Equal(seg15.Body.Bytes(), again.Body.Bytes()) {
		t.Fatal("cached VAAPI segment differs")
	}
	if s.metrics.segmentCacheHits.Load() == 0 {
		t.Fatal("VAAPI library playback did not record a cache hit")
	}
}
