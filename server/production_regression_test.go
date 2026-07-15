package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDynamicHLSABRVariantsAreVirtualAndGeneratedOnDemand(t *testing.T) {
	cache := t.TempDir()
	srv := New(Config{RequestTimeout: 2 * time.Minute, MaxConcurrentJobs: 2})
	t.Cleanup(srv.Close)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	body := map[string]any{
		"input_path": filepath.Join("..", "testdata", "sample.mp4"),
		"cache_dir":  cache,
		"options": map[string]any{
			"segment_seconds": 1,
			"audio_mode":      "skip",
			"crf":             32,
			"preset":          "ultrafast",
		},
		"variants": []map[string]any{
			{"name": "low", "width": 120, "height": 68, "video_bitrate": 300000, "crf": 34},
			{"name": "high", "width": 160, "height": 90, "video_bitrate": 600000, "crf": 32},
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
	if got := len(created.Variants); got != 2 {
		t.Fatalf("expected two variants, got %d: %+v", got, created.Variants)
	}
	if entries, err := os.ReadDir(filepath.Join(cache, created.ID)); err != nil || len(entries) != 0 {
		t.Fatalf("session creation should not generate media entries=%v err=%v", entries, err)
	}

	masterResp, err := http.Get(ts.URL + created.MasterURL)
	if err != nil {
		t.Fatal(err)
	}
	master, _ := io.ReadAll(masterResp.Body)
	_ = masterResp.Body.Close()
	if masterResp.StatusCode != http.StatusOK {
		t.Fatalf("master status=%d body=%s", masterResp.StatusCode, string(master))
	}
	masterText := string(master)
	if !strings.Contains(masterText, "variant/low/video.m3u8") || !strings.Contains(masterText, "variant/high/video.m3u8") {
		t.Fatalf("master missing variant playlists:\n%s", masterText)
	}
	if !strings.Contains(masterText, `FRAME-RATE=`) || !strings.Contains(masterText, `CODECS="`) {
		t.Fatalf("master missing probed stream metadata:\n%s", masterText)
	}
	sess, ok := srv.dynHLS.Get(created.ID)
	if !ok || len(sess.Variants) != 2 {
		t.Fatalf("missing HLS session variants after master probe")
	}
	for _, variant := range sess.Variants {
		if variant.VideoCodec.CodecString == "" {
			t.Fatalf("variant %s missing probed video codec", variant.Name)
		}
		if !strings.Contains(masterText, variant.VideoCodec.CodecString) {
			t.Fatalf("master missing exact codec %q for %s:\n%s", variant.VideoCodec.CodecString, variant.Name, masterText)
		}
	}
	for _, variant := range []string{"low", "high"} {
		dir := filepath.Join(cache, created.ID, variant)
		entries, err := os.ReadDir(dir)
		if err != nil || len(entries) == 0 {
			t.Fatalf("master should prime codec metadata for %s: entries=%v err=%v", variant, entries, err)
		}
	}

	plResp, err := http.Get(ts.URL + "/v1/playback/hls/" + created.ID + "/variant/high/video.m3u8")
	if err != nil {
		t.Fatal(err)
	}
	pl, _ := io.ReadAll(plResp.Body)
	_ = plResp.Body.Close()
	if plResp.StatusCode != http.StatusOK || !strings.Contains(string(pl), "segment/000000.ts") {
		t.Fatalf("variant playlist status=%d body=%s", plResp.StatusCode, string(pl))
	}

	segResp, err := http.Get(ts.URL + "/v1/playback/hls/" + created.ID + "/variant/high/segment/000000.ts")
	if err != nil {
		t.Fatal(err)
	}
	seg, _ := io.ReadAll(segResp.Body)
	_ = segResp.Body.Close()
	if segResp.StatusCode != http.StatusOK || len(seg) == 0 {
		t.Fatalf("segment status=%d bytes=%d", segResp.StatusCode, len(seg))
	}
	if _, err := os.Stat(filepath.Join(cache, created.ID, "high", "000000.ts")); err != nil {
		t.Fatalf("variant segment not cached in variant directory: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cache, created.ID, "low", "000000.ts")); err != nil {
		t.Fatalf("master codec probing should have primed low variant: %v", err)
	}
}

func TestMetricsExposeDynamicPlaybackCounters(t *testing.T) {
	cache := t.TempDir()
	srv := New(Config{RequestTimeout: 2 * time.Minute, MaxConcurrentJobs: 2})
	t.Cleanup(srv.Close)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	created := createTestHLSSession(t, ts, cache)

	for i := 0; i < 2; i++ {
		resp, err := http.Get(ts.URL + "/v1/playback/hls/" + created.ID + "/segment/000000.ts")
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("segment status=%d", resp.StatusCode)
		}
	}
	resp, err := http.Get(ts.URL + "/v1/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var snap MetricsSnapshot
	if err := json.NewDecoder(resp.Body).Decode(&snap); err != nil {
		t.Fatal(err)
	}
	if snap.HLSSessions < 1 || snap.SegmentsGenerated < 1 || snap.SegmentCacheHits < 1 {
		t.Fatalf("unexpected metrics snapshot: %+v", snap)
	}
}
