package server

import (
	"bytes"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	transcoder "media-transcoder"
)

func TestDynamicDASHIsOnDemandAndSeekableBySegment(t *testing.T) {
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
	resp, err := http.Post(ts.URL+"/v1/playback/dash/sessions", "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create status = %d", resp.StatusCode)
	}
	var created DynamicDASHSessionResponse
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}
	defer deleteTestSession(t, ts.URL+"/v1/playback/dash/"+created.ID)
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

	mpdResp, err := http.Get(ts.URL + created.ManifestURL)
	if err != nil {
		t.Fatal(err)
	}
	mpdBytes := new(bytes.Buffer)
	_, _ = mpdBytes.ReadFrom(mpdResp.Body)
	_ = mpdResp.Body.Close()
	if mpdResp.StatusCode != http.StatusOK {
		t.Fatalf("manifest status=%d body=%s", mpdResp.StatusCode, mpdBytes.String())
	}
	if !strings.Contains(mpdBytes.String(), `media="segment/$Number%06d$.m4s"`) || !strings.Contains(mpdBytes.String(), "<SegmentTemplate") || !strings.Contains(mpdBytes.String(), `initialization="segment/init.mp4"`) {
		t.Fatalf("manifest is not a seekable dynamic timeline with init segment:\n%s", mpdBytes.String())
	}

	initResp, err := http.Get(ts.URL + "/v1/playback/dash/" + created.ID + "/segment/init.mp4")
	if err != nil {
		t.Fatal(err)
	}
	init := new(bytes.Buffer)
	_, _ = init.ReadFrom(initResp.Body)
	_ = initResp.Body.Close()
	if initResp.StatusCode != http.StatusOK {
		t.Fatalf("init status=%d body=%s", initResp.StatusCode, init.String())
	}
	if !bytes.Contains(init.Bytes(), []byte("ftyp")) || !bytes.Contains(init.Bytes(), []byte("moov")) || bytes.Contains(init.Bytes(), []byte("moof")) {
		t.Fatalf("invalid dash init segment")
	}

	segResp, err := http.Get(ts.URL + "/v1/playback/dash/" + created.ID + "/segment/000000.m4s")
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
	if _, err := os.Stat(filepath.Join(cache, created.ID, "000000.m4s")); err != nil {
		t.Fatalf("segment not cached: %v", err)
	}
}

func TestDASHSegmentTimelineIncludesFinalPartialSegment(t *testing.T) {
	sess := &DynamicDASHSession{
		Options: transcoder.DASHOptions{SegmentSeconds: 4},
		Info:    transcoder.MediaInfo{Duration: 10.25, Width: 640, Height: 360, FPS: 24},
	}
	mpd := buildDynamicDASHMPD(sess)
	if !strings.Contains(mpd, `<S t="0" d="4000" r="1"/>`) {
		t.Fatalf("missing repeated full segments in MPD:\n%s", mpd)
	}
	if !strings.Contains(mpd, `<S d="2250"/>`) {
		t.Fatalf("missing final partial segment in MPD:\n%s", mpd)
	}
}

func TestDASHSegmentWindowClampsFinalSegment(t *testing.T) {
	start, duration, ok := dashSegmentWindow(10.25, 4, 2)
	if !ok || math.Abs(start-8) > 0.001 || math.Abs(duration-2.25) > 0.001 {
		t.Fatalf("window=(%.3f, %.3f, %v)", start, duration, ok)
	}
	if _, _, ok := dashSegmentWindow(10.25, 4, 3); ok {
		t.Fatal("out-of-range segment unexpectedly valid")
	}
}
