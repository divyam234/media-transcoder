package server

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	transcoder "media-transcoder"
)

func testProfileConfig(t *testing.T, cache string) Config {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", "testdata"))
	if err != nil {
		t.Fatal(err)
	}
	return Config{
		RequestTimeout:    time.Minute,
		CacheRoot:         cache,
		AllowedInputRoots: []string{root},
		Libraries: map[string]LibraryConfig{
			"samples": {Root: root},
		},
		Profiles: map[string]PlaybackProfile{
			"web-h264": {
				Container:      "hls",
				SegmentType:    "fmp4",
				SegmentSeconds: 1,
				Audio:          AudioProfile{Mode: transcoder.AudioSkip},
				Video:          VideoProfile{EncoderName: "libx264", Preset: "ultrafast", CRF: 34, GOPSize: 24, MaxBFrames: 0},
				Variants: []transcoder.LadderVariant{
					{Name: "low", Width: 160, Height: 90, VideoBitrate: 200000, CRF: 34},
					{Name: "mid", Width: 320, Height: 180, VideoBitrate: 400000, CRF: 32},
				},
			},
		},
	}
}

func TestProfileAndLibraryRoutes(t *testing.T) {
	s := New(testProfileConfig(t, t.TempDir()))
	for _, path := range []string{"/v1/profiles", "/v1/profiles/web-h264", "/v1/libraries", "/v1/libraries/samples"} {
		rr := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		s.Handler().ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("%s status=%d body=%s", path, rr.Code, rr.Body.String())
		}
	}
}

func TestLibraryHLSMasterUsesProfileVariantsAndAutoSessionReuse(t *testing.T) {
	cache := t.TempDir()
	s := New(testProfileConfig(t, cache))
	path := "/play/hls/web-h264/samples/sample.mp4/master.m3u8"
	for i := 0; i < 2; i++ {
		rr := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		s.Handler().ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("master status=%d body=%s", rr.Code, rr.Body.String())
		}
		body := rr.Body.String()
		if !strings.Contains(body, "variant/low/video.m3u8") || !strings.Contains(body, "variant/mid/video.m3u8") {
			t.Fatalf("master missing variants:\n%s", body)
		}
	}
	s.dynHLS.mu.RLock()
	count := len(s.dynHLS.sessions)
	s.dynHLS.mu.RUnlock()
	if count != 1 {
		t.Fatalf("expected one reused auto-session, got %d", count)
	}
	entries, _ := os.ReadDir(filepath.Join(cache, "media-transcoder-hls"))
	if len(entries) != 1 {
		t.Fatalf("expected one cache session dir, got %d", len(entries))
	}
}

func TestLibraryHLSVariantPlaylistAndSegment(t *testing.T) {
	s := New(testProfileConfig(t, t.TempDir()))
	playlist := "/play/hls/web-h264/samples/sample.mp4/variant/low/video.m3u8"
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, playlist, nil)
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("playlist status=%d body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "#EXT-X-MAP") || !strings.Contains(rr.Body.String(), "segment/000000.m4s") {
		t.Fatalf("bad fmp4 playlist:\n%s", rr.Body.String())
	}
	seg := "/play/hls/web-h264/samples/sample.mp4/variant/low/segment/000000.m4s"
	rr = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, seg, nil)
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("segment status=%d body=%s", rr.Code, rr.Body.String())
	}
	if rr.Body.Len() == 0 {
		t.Fatal("empty segment")
	}
}

func TestLibraryDASHManifestUsesProfileVariants(t *testing.T) {
	s := New(testProfileConfig(t, t.TempDir()))
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/play/dash/web-h264/samples/sample.mp4/manifest.mpd", nil)
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("manifest status=%d body=%s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if !strings.Contains(body, `Representation id="low"`) || !strings.Contains(body, `variant/low/segment/init.mp4`) || !strings.Contains(body, `Representation id="mid"`) {
		t.Fatalf("manifest missing variants:\n%s", body)
	}
}

func TestLibraryPathTraversalRejected(t *testing.T) {
	s := New(testProfileConfig(t, t.TempDir()))
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/play/hls/web-h264/samples/../sample.mp4/master.m3u8", nil)
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest && rr.Code != http.StatusNotFound {
		t.Fatalf("expected traversal rejection, got status=%d body=%s", rr.Code, rr.Body.String())
	}
}
