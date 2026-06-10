package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	transcoder "media-transcoder"
)

func TestOptionalHLSClientConsumesLongHEVCWithoutHardStuck(t *testing.T) {
	input := os.Getenv("TRANSCODER_TEST_LONG_INPUT")
	if input == "" {
		t.Skip("set TRANSCODER_TEST_LONG_INPUT to run long HLS client integration test")
	}
	ffmpeg := os.Getenv("TRANSCODER_FFMPEG_CLI")
	if ffmpeg == "" {
		var err error
		ffmpeg, err = exec.LookPath("ffmpeg")
		if err != nil {
			t.Skip("ffmpeg CLI not available for HLS client simulation")
		}
	}

	cache := t.TempDir()
	srv := New(Config{RequestTimeout: 30 * time.Minute, MaxConcurrentJobs: 4, CacheRoot: cache, AllowedInputRoots: []string{filepath.Dir(input)}})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	body, _ := json.Marshal(DynamicHLSSessionRequest{
		InputPath:       input,
		PrewarmSegments: 4,
		Options: transcoder.HLSOptions{TranscodeOptions: transcoder.TranscodeOptions{
			Width: 640, AudioMode: transcoder.AudioTranscode, CRF: 30, Preset: "ultrafast", GOPSize: 96, MaxBFrames: 0,
		}, SegmentSeconds: 4},
	})
	resp, err := http.Post(ts.URL+"/v1/playback/hls/sessions", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		payload, _ := io.ReadAll(resp.Body)
		t.Fatalf("create status=%d body=%s", resp.StatusCode, string(payload))
	}
	var created DynamicHLSSessionResponse
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 75*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, ffmpeg,
		"-hide_banner", "-loglevel", "warning",
		"-rw_timeout", "15000000",
		"-i", ts.URL+created.PlaylistURL,
		"-t", "24",
		"-f", "null", "-",
	)
	var stderr bytes.Buffer
	cmd.Stdout = io.Discard
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("HLS client failed or hung: %v\nstderr:\n%s", err, stderr.String())
	}
	bad := strings.ToLower(stderr.String())
	if strings.Contains(bad, "non monotonically increasing dts") || strings.Contains(bad, "invalid data found") || strings.Contains(bad, "end of file") {
		t.Fatalf("HLS client saw fatal timestamp/read errors:\n%s", stderr.String())
	}
}
