package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	transcoder "media-transcoder"
)

func testUIServer(t *testing.T) *Server {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "Movies"), 0o755); err != nil {
		t.Fatal(err)
	}
	sample, err := os.ReadFile(filepath.Join("..", "testdata", "sample.mp4"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "Movies", "sample.mp4"), sample, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "Movies", "movie.mkv"), sample, 0o644); err != nil {
		t.Fatal(err)
	}

	s := New(Config{
		RequestTimeout: time.Minute,
		Libraries: map[string]LibraryConfig{
			"media": {VFS: root},
		},
		Profiles: map[string]PlaybackProfile{
			"dash-h264-vaapi": {
				Container: "dash",
				Audio:     AudioProfile{Mode: transcoder.AudioTranscode, Codec: "aac", Bitrate: 128000, Channels: 2},
				Video:     VideoProfile{EncoderName: "h264_vaapi", GOPSize: 96},
			},
			"hls-h264-nvenc": {
				Container: "hls",
				Audio:     AudioProfile{Mode: transcoder.AudioTranscode, Codec: "aac", Bitrate: 128000, Channels: 2},
				Video:     VideoProfile{EncoderName: "h264_nvenc", GOPSize: 96},
			},
		},
	})
	t.Cleanup(s.Close)
	return s
}

func TestUIListsLibrariesAndFiles(t *testing.T) {
	s := testUIServer(t)

	for _, tc := range []struct {
		path string
		want string
	}{
		{path: "/", want: "media"},
		{path: "/ui/library/media/", want: "Movies/"},
		{path: "/ui/library/media/Movies/", want: "sample.mp4"},
	} {
		req := httptest.NewRequest(http.MethodGet, tc.path, nil)
		res := httptest.NewRecorder()
		s.Handler().ServeHTTP(res, req)
		if res.Code != http.StatusOK {
			t.Fatalf("GET %s: status=%d body=%s", tc.path, res.Code, res.Body.String())
		}
		if !strings.Contains(res.Body.String(), tc.want) {
			t.Fatalf("GET %s: missing %q", tc.path, tc.want)
		}
	}
}

func TestUIPlayerUsesStableLayoutAndAllPlaybackModes(t *testing.T) {
	s := testUIServer(t)

	tests := []struct {
		path string
		want string
	}{
		{path: "/ui/player/media/Movies/sample.mp4", want: "/media/media/Movies/sample.mp4"},
		{path: "/ui/player/media/Movies/movie.mkv?profile=hls-h264-nvenc", want: "/play/hls/hls-h264-nvenc/media/Movies/movie.mkv/master.m3u8"},
		{path: "/ui/player/media/Movies/movie.mkv?profile=dash:dash-h264-vaapi", want: "/play/dash/dash-h264-vaapi/media/Movies/movie.mkv/manifest.mpd"},
	}
	for _, tc := range tests {
		req := httptest.NewRequest(http.MethodGet, tc.path, nil)
		res := httptest.NewRecorder()
		s.Handler().ServeHTTP(res, req)
		if res.Code != http.StatusOK {
			t.Fatalf("GET %s: status=%d body=%s", tc.path, res.Code, res.Body.String())
		}
		body := res.Body.String()
		for _, want := range []string{
			tc.want,
			"Direct",
			"hls:hls-h264-nvenc",
			"dash:dash-h264-vaapi",
			"aspect-video",
			"https://cdn.dashjs.org/latest/dash.all.min.js",
			"Loading media…",
		} {
			if !strings.Contains(body, want) {
				t.Fatalf("GET %s: missing %q", tc.path, want)
			}
		}
	}
}

func TestRawLibraryMediaSupportsRanges(t *testing.T) {
	s := testUIServer(t)
	req := httptest.NewRequest(http.MethodGet, "/media/media/Movies/sample.mp4", nil)
	req.Header.Set("Range", "bytes=0-15")
	res := httptest.NewRecorder()
	s.Handler().ServeHTTP(res, req)

	if res.Code != http.StatusPartialContent {
		t.Fatalf("status=%d body=%s", res.Code, res.Body.String())
	}
	body, err := io.ReadAll(res.Result().Body)
	if err != nil {
		t.Fatal(err)
	}
	if len(body) != 16 {
		t.Fatalf("range length=%d want 16", len(body))
	}
	if got := res.Header().Get("Accept-Ranges"); got != "bytes" {
		t.Fatalf("Accept-Ranges=%q", got)
	}
}
