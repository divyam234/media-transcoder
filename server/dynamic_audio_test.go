package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	transcoder "media-transcoder"
)

func TestDynamicHLSDefaultAudioIsTimestampTrimmed(t *testing.T) {
	input := filepath.Join("..", "testdata", "avsample.mp4")
	cache := t.TempDir()
	srv := New(Config{RequestTimeout: 0, MaxConcurrentJobs: 1})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	body := map[string]any{
		"input_path": input,
		"cache_dir":  cache,
		"options": map[string]any{
			"segment_seconds": 2.25,
			"width":           160,
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
		t.Fatalf("create status=%d", resp.StatusCode)
	}
	var created DynamicHLSSessionResponse
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}

	segResp, err := http.Get(ts.URL + "/v1/playback/hls/" + created.ID + "/segment/000001.ts")
	if err != nil {
		t.Fatal(err)
	}
	_ = segResp.Body.Close()
	if segResp.StatusCode != http.StatusOK {
		t.Fatalf("segment status=%d", segResp.StatusCode)
	}
	segPath := filepath.Join(cache, created.ID, "000001.ts")
	info, err := transcoder.ProbeFile(context.Background(), segPath)
	if err != nil {
		t.Fatalf("probe cached segment: %v", err)
	}
	if !info.HasAudio || info.AudioStreams != 1 {
		t.Fatalf("dynamic segment missing audio: %+v", info)
	}
	if info.Duration < 1.95 || info.Duration > 2.60 {
		t.Fatalf("dynamic segment duration drift: %+v", info)
	}
}

func TestDynamicDASHDefaultAudioIsTimestampTrimmed(t *testing.T) {
	input := filepath.Join("..", "testdata", "avsample.mp4")
	cache := t.TempDir()
	srv := New(Config{RequestTimeout: 0, MaxConcurrentJobs: 1})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	body := map[string]any{
		"input_path": input,
		"cache_dir":  cache,
		"options": map[string]any{
			"segment_seconds": 1.75,
			"width":           160,
			"crf":             32,
			"preset":          "ultrafast",
		},
	}
	b, _ := json.Marshal(body)
	resp, err := http.Post(ts.URL+"/v1/playback/dash/sessions", "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create status=%d", resp.StatusCode)
	}
	var created DynamicDASHSessionResponse
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}

	segResp, err := http.Get(ts.URL + "/v1/playback/dash/" + created.ID + "/segment/000001.m4s")
	if err != nil {
		t.Fatal(err)
	}
	_ = segResp.Body.Close()
	if segResp.StatusCode != http.StatusOK {
		t.Fatalf("segment status=%d", segResp.StatusCode)
	}
	segPath := filepath.Join(cache, created.ID, "000001.m4s")
	info, err := transcoder.ProbeFile(context.Background(), segPath)
	if err != nil {
		t.Fatalf("probe cached segment: %v", err)
	}
	if !info.HasAudio || info.AudioStreams != 1 {
		t.Fatalf("dynamic dash segment missing audio: %+v", info)
	}
	if info.Duration < 1.45 || info.Duration > 2.15 {
		t.Fatalf("dynamic dash segment duration drift: %+v", info)
	}
}
