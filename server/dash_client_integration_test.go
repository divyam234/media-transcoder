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

func TestOptionalDASHClientConsumesLongHEVCWithSeparateAudio(t *testing.T) {
	input := os.Getenv("TRANSCODER_TEST_LONG_INPUT")
	if input == "" {
		t.Skip("set TRANSCODER_TEST_LONG_INPUT to run long DASH client integration test")
	}
	ffmpeg := os.Getenv("TRANSCODER_FFMPEG_CLI")
	if ffmpeg == "" {
		var err error
		ffmpeg, err = exec.LookPath("ffmpeg")
		if err != nil {
			t.Skip("ffmpeg CLI not available for DASH client simulation")
		}
	}
	srv := New(Config{RequestTimeout: 30 * time.Minute, MaxConcurrentJobs: 4, CacheRoot: t.TempDir(), AllowedInputRoots: []string{filepath.Dir(input)}})
	t.Cleanup(srv.Close)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	body, _ := json.Marshal(DynamicDASHSessionRequest{
		InputPath: input,
		Options: transcoder.DASHOptions{TranscodeOptions: transcoder.TranscodeOptions{
			Width: 640, AudioMode: transcoder.AudioTranscode, AudioCodec: "aac", AudioBitrate: 128000, AudioChannels: 2,
			CRF: 30, Preset: "ultrafast", GOPSize: 96, MaxBFrames: 0,
		}, SegmentSeconds: 4},
	})
	resp, err := http.Post(ts.URL+"/v1/playback/dash/sessions", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		payload, _ := io.ReadAll(resp.Body)
		t.Fatalf("create status=%d body=%s", resp.StatusCode, payload)
	}
	var created DynamicDASHSessionResponse
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 75*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, ffmpeg, "-hide_banner", "-loglevel", "warning", "-rw_timeout", "15000000", "-i", ts.URL+created.ManifestURL, "-t", "12", "-f", "null", "-")
	var stderr bytes.Buffer
	cmd.Stdout = io.Discard
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("DASH client failed or hung: %v\nstderr:\n%s", err, stderr.String())
	}
	bad := strings.ToLower(stderr.String())
	for _, marker := range []string{"non monotonically increasing dts", "invalid data found", "invalid nal unit", "end of file"} {
		if strings.Contains(bad, marker) {
			t.Fatalf("DASH client saw %q:\n%s", marker, stderr.String())
		}
	}
}
