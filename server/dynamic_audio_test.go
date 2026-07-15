package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	transcoder "media-transcoder"
)

func TestDynamicHLSDefaultAudioIsTimestampTrimmed(t *testing.T) {
	input := filepath.Join("..", "testdata", "avsample.mp4")
	cache := t.TempDir()
	srv := New(Config{RequestTimeout: 0, MaxConcurrentJobs: 1})
	t.Cleanup(srv.Close)
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
	defer deleteTestSession(t, ts.URL+"/v1/playback/hls/"+created.ID)

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

func TestDynamicDASHIsVideoOnlyUntilSeparateAudioRepresentationsExist(t *testing.T) {
	input := filepath.Join("..", "testdata", "avsample.mp4")
	cache := t.TempDir()
	srv := New(Config{RequestTimeout: 0, MaxConcurrentJobs: 1})
	t.Cleanup(srv.Close)
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
	defer deleteTestSession(t, ts.URL+"/v1/playback/dash/"+created.ID)

	segResp, err := http.Get(ts.URL + "/v1/playback/dash/" + created.ID + "/segment/000001.m4s")
	if err != nil {
		t.Fatal(err)
	}
	_ = segResp.Body.Close()
	if segResp.StatusCode != http.StatusOK {
		t.Fatalf("segment status=%d", segResp.StatusCode)
	}
	initPath := filepath.Join(cache, created.ID, "init.mp4")
	segPath := filepath.Join(cache, created.ID, "000001.m4s")
	joined := filepath.Join(t.TempDir(), "joined.mp4")
	initBytes, err := os.ReadFile(initPath)
	if err != nil {
		t.Fatalf("read dash init: %v", err)
	}
	segBytes, err := os.ReadFile(segPath)
	if err != nil {
		t.Fatalf("read dash segment: %v", err)
	}
	if !bytes.Contains(initBytes, []byte("moov")) || !bytes.Contains(segBytes, []byte("moof")) || !bytes.Contains(segBytes, []byte("mdat")) {
		t.Fatalf("dash init/media split invalid")
	}
	if err := os.WriteFile(joined, append(initBytes, segBytes...), 0o644); err != nil {
		t.Fatal(err)
	}
	info, err := transcoder.ProbeFile(context.Background(), joined)
	if err != nil {
		t.Fatalf("probe joined init+segment: %v", err)
	}
	if info.HasAudio || info.AudioStreams != 0 {
		t.Fatalf("dynamic DASH unexpectedly muxed audio into video representation: %+v", info)
	}
	// Segment 1 starts at 1.75s and ends near 3.5s because DASH fragments now
	// carry continuous tfdt decode times instead of resetting to zero.
	if info.Duration < 3.20 || info.Duration > 3.90 {
		t.Fatalf("dynamic dash segment timeline drift: %+v", info)
	}
}

func deleteTestSession(t *testing.T, url string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodDelete, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted && resp.StatusCode != http.StatusNotFound {
		t.Fatalf("delete session status=%d", resp.StatusCode)
	}
}
