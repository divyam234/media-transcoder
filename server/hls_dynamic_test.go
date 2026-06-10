package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDynamicHLSIsOnDemandAndSeekableBySegment(t *testing.T) {
	input := filepath.Join("..", "testdata", "sample.mp4")
	cache := t.TempDir()
	srv := New(Config{RequestTimeout: 0, MaxConcurrentJobs: 1})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	body := map[string]any{
		"input_path": input,
		"cache_dir":  cache,
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
		t.Fatalf("create status = %d", resp.StatusCode)
	}
	var created DynamicHLSSessionResponse
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}
	if created.ID == "" || created.SegmentCount < 1 {
		t.Fatalf("bad created response: %+v", created)
	}

	entries, err := os.ReadDir(filepath.Join(cache, created.ID))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("dynamic session pre-generated files: %d", len(entries))
	}

	plResp, err := http.Get(ts.URL + created.PlaylistURL)
	if err != nil {
		t.Fatal(err)
	}
	plBytes := new(bytes.Buffer)
	_, _ = plBytes.ReadFrom(plResp.Body)
	_ = plResp.Body.Close()
	if plResp.StatusCode != http.StatusOK {
		t.Fatalf("playlist status=%d body=%s", plResp.StatusCode, plBytes.String())
	}
	if !strings.Contains(plBytes.String(), "segment/000000.ts") || !strings.Contains(plBytes.String(), "#EXT-X-ENDLIST") {
		t.Fatalf("playlist is not a seekable VOD timeline:\n%s", plBytes.String())
	}
	entries, _ = os.ReadDir(filepath.Join(cache, created.ID))
	if len(entries) != 0 {
		t.Fatalf("playlist request generated segments: %d", len(entries))
	}

	segResp, err := http.Get(ts.URL + "/v1/playback/hls/" + created.ID + "/segment/000000.ts")
	if err != nil {
		t.Fatal(err)
	}
	seg := new(bytes.Buffer)
	_, _ = seg.ReadFrom(segResp.Body)
	_ = segResp.Body.Close()
	if segResp.StatusCode != http.StatusOK {
		t.Fatalf("segment status=%d body=%s", segResp.StatusCode, seg.String())
	}
	if seg.Len() == 0 {
		t.Fatal("empty segment")
	}
	if _, err := os.Stat(filepath.Join(cache, created.ID, "000000.ts")); err != nil {
		t.Fatalf("segment not cached: %v", err)
	}
}
