package server

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func request(t *testing.T, s *Server, path string) *httptest.ResponseRecorder {
	t.Helper()
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	s.Handler().ServeHTTP(rr, req)
	return rr
}

func TestLibraryHLSConcurrentMasterCreatesOneSession(t *testing.T) {
	cache := t.TempDir()
	s := New(testProfileConfig(t, cache))
	t.Cleanup(s.Close)
	const path = "/play/hls/web-h264/samples/sample.mp4/master.m3u8"

	const workers = 24
	start := make(chan struct{})
	errCh := make(chan string, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			rr := request(t, s, path)
			if rr.Code != http.StatusOK {
				errCh <- rr.Body.String()
			}
		}()
	}
	close(start)
	wg.Wait()
	close(errCh)
	for body := range errCh {
		t.Fatalf("concurrent master failed: %s", body)
	}

	s.dynHLS.mu.RLock()
	count := len(s.dynHLS.sessions)
	s.dynHLS.mu.RUnlock()
	if count != 1 {
		t.Fatalf("sessions = %d, want 1", count)
	}
	entries, err := os.ReadDir(filepath.Join(cache, "media-transcoder-hls"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("cache directories = %d, want 1", len(entries))
	}
}

func TestLibraryDASHConcurrentManifestCreatesOneSession(t *testing.T) {
	cache := t.TempDir()
	s := New(testProfileConfig(t, cache))
	t.Cleanup(s.Close)
	const path = "/play/dash/web-h264/samples/sample.mp4/manifest.mpd"

	const workers = 24
	start := make(chan struct{})
	errCh := make(chan string, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			rr := request(t, s, path)
			if rr.Code != http.StatusOK {
				errCh <- rr.Body.String()
			}
		}()
	}
	close(start)
	wg.Wait()
	close(errCh)
	for body := range errCh {
		t.Fatalf("concurrent manifest failed: %s", body)
	}

	s.dynDASH.mu.RLock()
	count := len(s.dynDASH.sessions)
	s.dynDASH.mu.RUnlock()
	if count != 1 {
		t.Fatalf("sessions = %d, want 1", count)
	}
}

func TestLibraryHLSCompleteFMP4FlowAndCacheReuse(t *testing.T) {
	cache := t.TempDir()
	s := New(testProfileConfig(t, cache))
	t.Cleanup(s.Close)
	base := "/play/hls/web-h264/samples/sample.mp4/variant/low"

	for _, path := range []string{
		"/play/hls/web-h264/samples/sample.mp4/master.m3u8",
		base + "/video.m3u8",
	} {
		rr := request(t, s, path)
		if rr.Code != http.StatusOK {
			t.Fatalf("%s status=%d body=%s", path, rr.Code, rr.Body.String())
		}
	}

	init := request(t, s, base+"/segment/init.mp4")
	if init.Code != http.StatusOK || !bytes.Contains(init.Body.Bytes(), []byte("ftyp")) || !bytes.Contains(init.Body.Bytes(), []byte("moov")) {
		t.Fatalf("invalid init segment: status=%d bytes=%d", init.Code, init.Body.Len())
	}
	first := request(t, s, base+"/segment/000000.m4s")
	if first.Code != http.StatusOK || !bytes.Contains(first.Body.Bytes(), []byte("moof")) || !bytes.Contains(first.Body.Bytes(), []byte("mdat")) {
		t.Fatalf("invalid media segment: status=%d bytes=%d", first.Code, first.Body.Len())
	}
	second := request(t, s, base+"/segment/000000.m4s")
	if second.Code != http.StatusOK || !bytes.Equal(first.Body.Bytes(), second.Body.Bytes()) {
		t.Fatal("cached HLS segment differs from first response")
	}

	root := filepath.Join(cache, "media-transcoder-hls")
	var midFiles []string
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err == nil && strings.Contains(filepath.ToSlash(path), "/mid/") && !d.IsDir() {
			midFiles = append(midFiles, path)
		}
		return nil
	})
	if len(midFiles) == 0 {
		t.Fatal("master playlist should prime the mid variant to extract exact codec metadata")
	}
}

func TestLibraryDASHCompleteFlowAndCacheReuse(t *testing.T) {
	cache := t.TempDir()
	s := New(testProfileConfig(t, cache))
	t.Cleanup(s.Close)
	base := "/play/dash/web-h264/samples/sample.mp4/variant/low/segment"

	manifest := request(t, s, "/play/dash/web-h264/samples/sample.mp4/manifest.mpd")
	if manifest.Code != http.StatusOK {
		t.Fatalf("manifest status=%d body=%s", manifest.Code, manifest.Body.String())
	}
	init := request(t, s, base+"/init.mp4")
	if init.Code != http.StatusOK || !bytes.Contains(init.Body.Bytes(), []byte("ftyp")) || !bytes.Contains(init.Body.Bytes(), []byte("moov")) {
		t.Fatalf("invalid DASH init: status=%d bytes=%d", init.Code, init.Body.Len())
	}
	first := request(t, s, base+"/000000.m4s")
	if first.Code != http.StatusOK || !bytes.Contains(first.Body.Bytes(), []byte("moof")) || !bytes.Contains(first.Body.Bytes(), []byte("mdat")) {
		t.Fatalf("invalid DASH media: status=%d bytes=%d", first.Code, first.Body.Len())
	}
	second := request(t, s, base+"/000000.m4s")
	if second.Code != http.StatusOK || !bytes.Equal(first.Body.Bytes(), second.Body.Bytes()) {
		t.Fatal("cached DASH segment differs from first response")
	}
}

func TestLibrarySessionInvalidatesWhenSourceChanges(t *testing.T) {
	root := t.TempDir()
	input, err := os.ReadFile(filepath.Join("..", "testdata", "sample.mp4"))
	if err != nil {
		t.Fatal(err)
	}
	media := filepath.Join(root, "sample.mp4")
	if err := os.WriteFile(media, input, 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := testProfileConfig(t, t.TempDir())
	cfg.AllowedInputRoots = []string{root}
	cfg.Libraries = map[string]LibraryConfig{"samples": {VFS: root}}
	s := New(cfg)
	t.Cleanup(s.Close)

	path := "/play/hls/web-h264/samples/sample.mp4/master.m3u8"
	if rr := request(t, s, path); rr.Code != http.StatusOK {
		t.Fatalf("first request: %d %s", rr.Code, rr.Body.String())
	}
	s.dynHLS.mu.RLock()
	var firstID string
	for id := range s.dynHLS.sessions {
		firstID = id
	}
	s.dynHLS.mu.RUnlock()

	if err := os.WriteFile(media, append(input, 0), 0o644); err != nil {
		t.Fatal(err)
	}
	if rr := request(t, s, path); rr.Code != http.StatusOK {
		t.Fatalf("second request: %d %s", rr.Code, rr.Body.String())
	}
	s.dynHLS.mu.RLock()
	defer s.dynHLS.mu.RUnlock()
	if len(s.dynHLS.sessions) != 1 {
		t.Fatalf("sessions = %d, want 1 after source change", len(s.dynHLS.sessions))
	}
	if _, ok := s.dynHLS.sessions[firstID]; ok {
		t.Fatal("stale session was not replaced")
	}
}

func TestLibraryCacheReuseAcrossServerRestart(t *testing.T) {
	cache := t.TempDir()
	cfg := testProfileConfig(t, cache)
	path := "/play/hls/web-h264/samples/sample.mp4/variant/low/segment/000000.m4s"

	s1 := New(cfg)
	t.Cleanup(s1.Close)
	first := request(t, s1, path)
	if first.Code != http.StatusOK {
		t.Fatalf("first status=%d body=%s", first.Code, first.Body.String())
	}

	s2 := New(cfg)
	t.Cleanup(s2.Close)
	second := request(t, s2, path)
	if second.Code != http.StatusOK {
		t.Fatalf("second status=%d body=%s", second.Code, second.Body.String())
	}
	if !bytes.Equal(first.Body.Bytes(), second.Body.Bytes()) {
		t.Fatal("cached segment changed across server restart")
	}
	if got := s2.metrics.segmentCacheHits.Load(); got == 0 {
		t.Fatal("second server did not record a cache hit")
	}
}

func TestLibraryMalformedPlaybackURLs(t *testing.T) {
	s := New(testProfileConfig(t, t.TempDir()))
	t.Cleanup(s.Close)
	cases := []struct {
		path string
		want int
	}{
		{"/play/hls/missing/samples/sample.mp4/master.m3u8", http.StatusNotFound},
		{"/play/hls/web-h264/missing/sample.mp4/master.m3u8", http.StatusNotFound},
		{"/play/hls/web-h264/samples/sample.mp4/variant/missing/video.m3u8", http.StatusNotFound},
		{"/play/hls/web-h264/samples/sample.mp4/variant/low/segment/not-a-number.m4s", http.StatusBadRequest},
		{"/play/hls/web-h264/samples/sample.mp4/variant/low/segment/999999.m4s", http.StatusRequestedRangeNotSatisfiable},
		{"/play/dash/web-h264/samples/sample.mp4/variant/missing/segment/000000.m4s", http.StatusNotFound},
		{"/play/dash/web-h264/samples/sample.mp4/variant/low/segment/not-a-number.m4s", http.StatusBadRequest},
		{"/play/dash/web-h264/samples/sample.mp4/variant/low/segment/999999.m4s", http.StatusRequestedRangeNotSatisfiable},
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			rr := request(t, s, tc.path)
			if rr.Code != tc.want {
				t.Fatalf("status=%d want=%d body=%s", rr.Code, tc.want, rr.Body.String())
			}
		})
	}
}
