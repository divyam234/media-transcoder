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

func TestDynamicHLSFMP4PlaylistAndSegment(t *testing.T) {
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
			"segment_type":    "fmp4",
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

	plResp, err := http.Get(ts.URL + created.PlaylistURL)
	if err != nil {
		t.Fatal(err)
	}
	pl := new(bytes.Buffer)
	_, _ = pl.ReadFrom(plResp.Body)
	_ = plResp.Body.Close()
	if plResp.StatusCode != http.StatusOK {
		t.Fatalf("playlist status=%d body=%s", plResp.StatusCode, pl.String())
	}
	playlist := pl.String()
	for _, want := range []string{"#EXT-X-VERSION:7", "#EXT-X-MAP:URI=\"segment/init.mp4\"", "segment/000000.m4s"} {
		if !strings.Contains(playlist, want) {
			t.Fatalf("fmp4 playlist missing %q:\n%s", want, playlist)
		}
	}
	if !strings.Contains(playlist, "#EXT-X-DISCONTINUITY") {
		t.Fatalf("fmp4 playlist should mark independently generated segments as discontinuities:\n%s", playlist)
	}
	if strings.Count(playlist, "#EXT-X-MAP:URI=\"segment/init.mp4\"") != 1 {
		t.Fatalf("fmp4 playlist should advertise one shared EXT-X-MAP:\n%s", playlist)
	}

	segResp, err := http.Get(ts.URL + "/v1/playback/hls/" + created.ID + "/segment/000001.m4s")
	if err != nil {
		t.Fatal(err)
	}
	seg := new(bytes.Buffer)
	_, _ = seg.ReadFrom(segResp.Body)
	_ = segResp.Body.Close()
	if segResp.StatusCode != http.StatusOK {
		t.Fatalf("segment status=%d body=%s", segResp.StatusCode, seg.String())
	}
	if got := segResp.Header.Get("Content-Type"); !strings.HasPrefix(got, "video/mp4") {
		t.Fatalf("segment content-type=%q", got)
	}
	if seg.Len() == 0 {
		t.Fatal("empty fmp4 media segment")
	}
	if bytes.Contains(seg.Bytes(), []byte("ftyp")) || bytes.Contains(seg.Bytes(), []byte("moov")) {
		t.Fatalf("media .m4s contains init boxes; expected moof/mdat only")
	}
	if !bytes.Contains(seg.Bytes(), []byte("moof")) || !bytes.Contains(seg.Bytes(), []byte("mdat")) {
		t.Fatalf("media .m4s missing moof/mdat boxes")
	}

	initResp, err := http.Get(ts.URL + "/v1/playback/hls/" + created.ID + "/segment/init.mp4")
	if err != nil {
		t.Fatal(err)
	}
	init := new(bytes.Buffer)
	_, _ = init.ReadFrom(initResp.Body)
	_ = initResp.Body.Close()
	if initResp.StatusCode != http.StatusOK {
		t.Fatalf("init status=%d body=%s", initResp.StatusCode, init.String())
	}
	if !bytes.Contains(init.Bytes(), []byte("ftyp")) || !bytes.Contains(init.Bytes(), []byte("moov")) {
		t.Fatalf("init segment missing ftyp/moov")
	}
	if bytes.Contains(init.Bytes(), []byte("moof")) || bytes.Contains(init.Bytes(), []byte("mdat")) {
		t.Fatalf("init segment contains media boxes")
	}
	for _, p := range []string{filepath.Join(cache, created.ID, "init.mp4"), filepath.Join(cache, created.ID, "000001.m4s")} {
		if st, err := os.Stat(p); err != nil || st.Size() == 0 {
			t.Fatalf("expected cached %s: size=%d err=%v", p, func() int64 {
				if st != nil {
					return st.Size()
				}
				return 0
			}(), err)
		}
	}
}
