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
			"samples": {VFS: root},
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
	t.Cleanup(s.Close)
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
	t.Cleanup(s.Close)
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
	t.Cleanup(s.Close)
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
	t.Cleanup(s.Close)
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
	t.Cleanup(s.Close)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/play/hls/web-h264/samples/../sample.mp4/master.m3u8", nil)
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest && rr.Code != http.StatusNotFound {
		t.Fatalf("expected traversal rejection, got status=%d body=%s", rr.Code, rr.Body.String())
	}
}

func TestPlaybackProfilePropagatesVAAPIDevice(t *testing.T) {
	profile := PlaybackProfile{
		SegmentSeconds: 4,
		Video: VideoProfile{
			EncoderName:    "h264_vaapi",
			HardwareDevice: "/dev/dri/renderD129",
			HardwareDecode: true,
		},
	}
	opts := profile.HLSOptions()
	if opts.EncoderName != "h264_vaapi" {
		t.Fatalf("encoder = %q", opts.EncoderName)
	}
	if opts.HardwareDevice != "/dev/dri/renderD129" {
		t.Fatalf("hardware device = %q", opts.HardwareDevice)
	}
	if !opts.HardwareDecode {
		t.Fatal("hardware decode was not propagated")
	}
}

func TestExampleConfigLoads(t *testing.T) {
	cfg, err := LoadPlaybackConfig(filepath.Join("..", "transcoder.example.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Libraries) < 4 {
		t.Fatalf("expected production library examples, got %d", len(cfg.Libraries))
	}
	if cfg.Libraries["cached_movies"].HTTP == nil || cfg.Libraries["cached_movies"].HTTP.BaseURL != "http://media-cache/media/" {
		t.Fatalf("HTTP library example was not loaded: %#v", cfg.Libraries["cached_movies"].HTTP)
	}
	if len(cfg.Profiles) < 3 {
		t.Fatalf("expected HLS and DASH profile examples, got %d", len(cfg.Profiles))
	}
	if len(cfg.Server.HTTPAllowedHosts) != 2 || cfg.Server.HTTPAllowedHosts[0] != "media-cache" {
		t.Fatalf("HTTP allowed hosts were not loaded: %v", cfg.Server.HTTPAllowedHosts)
	}
	if got := cfg.Profiles["hls-h264-nvenc"].Variants[3].VideoBitrate; got != 5500000 {
		t.Fatalf("human-readable example bitrate parsed as %d", got)
	}
}

func TestVariantCropAspectResolvesFromSource(t *testing.T) {
	info := transcoder.MediaInfo{Width: 3840, Height: 2160}
	requested := []transcoder.LadderVariant{{Name: "1080p", Width: 1920, CropAspect: "40:17"}}

	hls := buildHLSVariants(transcoder.HLSOptions{}, requested, info)
	if len(hls) != 1 {
		t.Fatalf("HLS variants = %d", len(hls))
	}
	if got := hls[0]; got.Height != 816 || got.Options.CropWidth != 3840 || got.Options.CropHeight != 1632 || got.Options.CropX != 0 || got.Options.CropY != 264 {
		t.Fatalf("unexpected HLS crop variant: %+v options=%+v", got, got.Options)
	}

	dash := buildDASHVariants(transcoder.DASHOptions{}, requested, info)
	if len(dash) != 1 {
		t.Fatalf("DASH variants = %d", len(dash))
	}
	if got := dash[0]; got.Height != 816 || got.Options.CropWidth != 3840 || got.Options.CropHeight != 1632 || got.Options.CropX != 0 || got.Options.CropY != 264 {
		t.Fatalf("unexpected DASH crop variant: %+v options=%+v", got, got.Options)
	}
}
