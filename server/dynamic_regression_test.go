package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	transcoder "media-transcoder"
)

func createTestHLSSession(t *testing.T, ts *httptest.Server, cache string) DynamicHLSSessionResponse {
	t.Helper()
	body := map[string]any{
		"input_path": filepath.Join("..", "testdata", "sample.mp4"),
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
		payload, _ := io.ReadAll(resp.Body)
		t.Fatalf("create HLS status=%d body=%s", resp.StatusCode, string(payload))
	}
	var created DynamicHLSSessionResponse
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}
	return created
}

func createTestDASHSession(t *testing.T, ts *httptest.Server, cache string) DynamicDASHSessionResponse {
	t.Helper()
	body := map[string]any{
		"input_path": filepath.Join("..", "testdata", "sample.mp4"),
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
		payload, _ := io.ReadAll(resp.Body)
		t.Fatalf("create DASH status=%d body=%s", resp.StatusCode, string(payload))
	}
	var created DynamicDASHSessionResponse
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}
	return created
}

func TestDynamicHLSSegmentConcurrentRequestsShareCacheAndLeaveNoTmp(t *testing.T) {
	cache := t.TempDir()
	srv := New(Config{RequestTimeout: 2 * time.Minute, MaxConcurrentJobs: 2})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	created := createTestHLSSession(t, ts, cache)

	const requestCount = 5
	var wg sync.WaitGroup
	bodies := make([][]byte, requestCount)
	codes := make([]int, requestCount)
	for i := 0; i < requestCount; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			resp, err := http.Get(ts.URL + "/v1/playback/hls/" + created.ID + "/segment/000000.ts")
			if err != nil {
				codes[i] = -1
				bodies[i] = []byte(err.Error())
				return
			}
			defer resp.Body.Close()
			codes[i] = resp.StatusCode
			bodies[i], _ = io.ReadAll(resp.Body)
		}(i)
	}
	wg.Wait()

	for i, code := range codes {
		if code != http.StatusOK {
			t.Fatalf("request %d status=%d body=%s", i, code, string(bodies[i]))
		}
		if len(bodies[i]) == 0 {
			t.Fatalf("request %d returned empty segment", i)
		}
		if i > 0 && !bytes.Equal(bodies[0], bodies[i]) {
			t.Fatalf("concurrent request %d returned bytes different from cached segment", i)
		}
	}
	entries, err := os.ReadDir(filepath.Join(cache, created.ID))
	if err != nil {
		t.Fatal(err)
	}
	var mediaFiles, tmpFiles int
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			tmpFiles++
		}
		if strings.HasSuffix(e.Name(), ".ts") {
			mediaFiles++
		}
	}
	if tmpFiles != 0 {
		t.Fatalf("left temporary files after concurrent HLS generation: %d", tmpFiles)
	}
	if mediaFiles != 1 {
		t.Fatalf("expected exactly one cached HLS segment, got %d entries=%v", mediaFiles, entries)
	}
}

func TestDynamicHLSCanceledContextDoesNotCreateTempOrCache(t *testing.T) {
	cache := t.TempDir()
	srv := New(Config{RequestTimeout: time.Minute, MaxConcurrentJobs: 1})
	sessCtx, sessCancel := context.WithCancel(context.Background())
	defer sessCancel()
	sess := &DynamicHLSSession{
		ID:        "cancel-hls",
		InputPath: filepath.Join("..", "testdata", "sample.mp4"),
		Options: transcoder.HLSOptions{TranscodeOptions: transcoder.TranscodeOptions{
			Width: 160, AudioMode: transcoder.AudioSkip, CRF: 32, Preset: "ultrafast",
		}, SegmentSeconds: 1},
		CacheDir: cache,
		Info:     transcoder.MediaInfo{Duration: 3, Width: 320, Height: 180, FPS: 24},
		ctx:      sessCtx,
		cancel:   sessCancel,
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	path := filepath.Join(cache, "000000.ts")
	err := srv.ensureDynamicSegment(ctx, sess, 0, path)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("canceled generation created segment: %v", err)
	}
	matches, _ := filepath.Glob(filepath.Join(cache, "*.tmp"))
	if len(matches) != 0 {
		t.Fatalf("canceled generation left temp files: %v", matches)
	}
}

func TestDynamicHLSSessionDeleteCancelsRemovesCacheAndLocks(t *testing.T) {
	cache := t.TempDir()
	srv := New(Config{RequestTimeout: 2 * time.Minute, MaxConcurrentJobs: 1})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	created := createTestHLSSession(t, ts, cache)

	resp, err := http.Get(ts.URL + "/v1/playback/hls/" + created.ID + "/segment/000000.ts")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("segment status=%d", resp.StatusCode)
	}
	if _, err := os.Stat(filepath.Join(cache, created.ID, "000000.ts")); err != nil {
		t.Fatalf("expected cached segment before delete: %v", err)
	}

	req, _ := http.NewRequest(http.MethodDelete, ts.URL+"/v1/playback/hls/"+created.ID, nil)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("delete status=%d", resp.StatusCode)
	}
	if _, ok := srv.dynHLS.Get(created.ID); ok {
		t.Fatal("session still present after delete")
	}
	if _, err := os.Stat(filepath.Join(cache, created.ID)); !os.IsNotExist(err) {
		t.Fatalf("cache dir not removed: %v", err)
	}
	srv.dynHLS.mu.RLock()
	defer srv.dynHLS.mu.RUnlock()
	for key := range srv.dynHLS.locks {
		if strings.HasPrefix(key, created.ID+":") {
			t.Fatalf("left per-segment lock after delete: %s", key)
		}
	}
}

func TestDynamicDASHCanceledContextAndDeleteCleanup(t *testing.T) {
	cache := t.TempDir()
	srv := New(Config{RequestTimeout: 2 * time.Minute, MaxConcurrentJobs: 1})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	created := createTestDASHSession(t, ts, cache)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	sess, ok := srv.dynDASH.Get(created.ID)
	if !ok {
		t.Fatal("session missing")
	}
	path := filepath.Join(cache, created.ID, "000000.m4s")
	err := srv.ensureDynamicDASHSegment(ctx, sess, 0, path)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("canceled DASH generation created segment: %v", err)
	}

	req, _ := http.NewRequest(http.MethodDelete, ts.URL+"/v1/playback/dash/"+created.ID, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("delete DASH status=%d", resp.StatusCode)
	}
	if _, ok := srv.dynDASH.Get(created.ID); ok {
		t.Fatal("DASH session still present after delete")
	}
	if _, err := os.Stat(filepath.Join(cache, created.ID)); !os.IsNotExist(err) {
		t.Fatalf("DASH cache dir not removed: %v", err)
	}
}

func TestDynamicPlaybackRepeatedCachedRequestsDoNotLeakGoroutines(t *testing.T) {
	cache := t.TempDir()
	srv := New(Config{RequestTimeout: 2 * time.Minute, MaxConcurrentJobs: 2})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	created := createTestHLSSession(t, ts, cache)
	segURL := ts.URL + "/v1/playback/hls/" + created.ID + "/segment/000000.ts"

	resp, err := http.Get(segURL)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("initial segment status=%d", resp.StatusCode)
	}

	runtime.GC()
	before := runtime.NumGoroutine()
	for i := 0; i < 30; i++ {
		for _, url := range []string{ts.URL + created.PlaylistURL, segURL} {
			resp, err := http.Get(url)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("%s status=%d", url, resp.StatusCode)
			}
		}
	}
	deadline := time.Now().Add(2 * time.Second)
	var after int
	for time.Now().Before(deadline) {
		runtime.GC()
		after = runtime.NumGoroutine()
		if after <= before+6 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("possible goroutine leak: before=%d after=%d", before, after)
}

func TestOptionalDynamicPlaybackRSSBounded(t *testing.T) {
	if os.Getenv("TRANSCODER_STRESS_TEST") != "1" {
		t.Skip("set TRANSCODER_STRESS_TEST=1 to run RSS stress test")
	}
	cache := t.TempDir()
	srv := New(Config{RequestTimeout: 2 * time.Minute, MaxConcurrentJobs: 2})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	created := createTestHLSSession(t, ts, cache)
	before := readRSSKB(t)
	for i := 0; i < 12; i++ {
		idx := i % max(1, min(created.SegmentCount, 3))
		url := ts.URL + "/v1/playback/hls/" + created.ID + "/segment/" + strings.Repeat("0", 6-len(strconv.Itoa(idx))) + strconv.Itoa(idx) + ".ts"
		resp, err := http.Get(url)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("segment status=%d", resp.StatusCode)
		}
	}
	runtime.GC()
	after := readRSSKB(t)
	if delta := after - before; delta > 192*1024 {
		t.Fatalf("RSS grew too much: before=%dKB after=%dKB delta=%dKB", before, after, delta)
	}
}

func readRSSKB(t *testing.T) int64 {
	t.Helper()
	b, err := os.ReadFile("/proc/self/status")
	if err != nil {
		t.Skipf("/proc/self/status unavailable: %v", err)
	}
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, "VmRSS:") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				v, err := strconv.ParseInt(fields[1], 10, 64)
				if err == nil {
					return v
				}
			}
		}
	}
	t.Skip("VmRSS not found")
	return 0
}
