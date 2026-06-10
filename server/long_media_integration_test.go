package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	transcoder "media-transcoder"
)

func TestOptionalLongHEVCHLSDoesNotStallFirstSegments(t *testing.T) {
	input := os.Getenv("TRANSCODER_TEST_LONG_INPUT")
	if input == "" {
		t.Skip("set TRANSCODER_TEST_LONG_INPUT to run long HEVC dynamic HLS integration test")
	}
	cache := t.TempDir()
	srv := New(Config{RequestTimeout: 30 * time.Minute, MaxConcurrentJobs: 4, CacheRoot: cache, AllowedInputRoots: []string{filepath.Dir(input)}})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	body, _ := json.Marshal(DynamicHLSSessionRequest{
		InputPath:       input,
		PrewarmSegments: 3,
		Options: transcoder.HLSOptions{TranscodeOptions: transcoder.TranscodeOptions{
			Width: 640, AudioMode: transcoder.AudioTranscode, CRF: 30, Preset: "ultrafast", GOPSize: 96,
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
	if resp, err = http.Get(ts.URL + created.PlaylistURL); err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("playlist status=%d", resp.StatusCode)
	}

	deadline := time.Now().Add(30 * time.Second)
	for i := 0; i < 4; i++ {
		segURL := ts.URL + "/v1/playback/hls/" + created.ID + "/segment/" + sprintfSegment(i)
		start := time.Now()
		resp, err := http.Get(segURL)
		if err != nil {
			t.Fatal(err)
		}
		n, _ := io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("segment %d status=%d", i, resp.StatusCode)
		}
		if n == 0 {
			t.Fatalf("segment %d was empty", i)
		}
		t.Logf("segment %d bytes=%d elapsed=%s", i, n, time.Since(start))
		if time.Now().After(deadline) {
			t.Fatalf("first segments took too long; likely playback stall")
		}
	}
}

func sprintfSegment(i int) string { return fmt.Sprintf("%06d.ts", i) }
