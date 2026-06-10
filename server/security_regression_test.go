package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"
)

func TestAllowedInputRootsRejectsOutsidePath(t *testing.T) {
	srv := New(Config{RequestTimeout: time.Minute, AllowedInputRoots: []string{t.TempDir()}})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	body, _ := json.Marshal(map[string]any{"input_path": filepath.Join("..", "testdata", "sample.mp4")})
	resp, err := http.Post(ts.URL+"/v1/probe", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected forbidden for outside input root, got %d", resp.StatusCode)
	}
}

func TestCacheRootOverridesClientCacheDir(t *testing.T) {
	serverCache := t.TempDir()
	clientCache := t.TempDir()
	srv := New(Config{RequestTimeout: 2 * time.Minute, MaxConcurrentJobs: 1, CacheRoot: serverCache})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	body := map[string]any{
		"input_path": filepath.Join("..", "testdata", "sample.mp4"),
		"cache_dir":  clientCache,
		"options": map[string]any{
			"segment_seconds": 1,
			"width":           160,
			"audio_mode":      "skip",
			"crf":             32,
			"preset":          "ultrafast",
		},
	}
	b, _ := json.Marshal(body)
	resp, err := http.Post(ts.URL+"/v1/playback/hls/sessions", "application/json", bytes.NewReader(b))
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
	resp, err = http.Get(ts.URL + "/v1/playback/hls/" + created.ID + "/segment/000000.ts")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("segment status=%d", resp.StatusCode)
	}
	serverFiles, err := filepath.Glob(filepath.Join(serverCache, "media-transcoder-hls", created.ID, "*.ts"))
	if err != nil {
		t.Fatal(err)
	}
	if len(serverFiles) != 1 {
		t.Fatalf("expected segment under server cache root, got %v", serverFiles)
	}
	clientFiles, err := filepath.Glob(filepath.Join(clientCache, "**", "*.ts"))
	if err != nil {
		t.Fatal(err)
	}
	if len(clientFiles) != 0 {
		t.Fatalf("client cache dir should be ignored when server cache root is set, got %v", clientFiles)
	}
}
