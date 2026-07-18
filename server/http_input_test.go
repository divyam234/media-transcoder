package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestServerProbeHTTPSource(t *testing.T) {
	media, err := os.ReadFile(filepath.Join("..", "testdata", "sample.mp4"))
	if err != nil {
		t.Fatal(err)
	}
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer origin-secret" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("ETag", `"sample-v1"`)
		w.Header().Set("Content-Type", "video/mp4")
		http.ServeContent(w, r, "sample.mp4", time.Unix(1, 0), bytes.NewReader(media))
	}))
	defer origin.Close()
	originURL, err := url.Parse(origin.URL)
	if err != nil {
		t.Fatal(err)
	}

	s := New(Config{RequestTimeout: time.Minute, HTTPAllowedHosts: []string{originURL.Host}})
	t.Cleanup(s.Close)
	body, _ := json.Marshal(map[string]any{
		"input_url": origin.URL + "/sample.mp4",
		"input_headers": map[string]string{
			"Authorization": "Bearer origin-secret",
		},
	})
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/probe", bytes.NewReader(body))
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
}

func TestDynamicPlaybackSessionsAcceptHTTPSource(t *testing.T) {
	media, err := os.ReadFile(filepath.Join("..", "testdata", "sample.mp4"))
	if err != nil {
		t.Fatal(err)
	}
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"sample-v1"`)
		w.Header().Set("Content-Type", "video/mp4")
		http.ServeContent(w, r, "sample.mp4", time.Unix(1, 0), bytes.NewReader(media))
	}))
	defer origin.Close()
	originURL, err := url.Parse(origin.URL)
	if err != nil {
		t.Fatal(err)
	}

	s := New(Config{RequestTimeout: time.Minute, CacheRoot: t.TempDir(), HTTPAllowedHosts: []string{originURL.Host}})
	t.Cleanup(s.Close)
	for _, endpoint := range []string{
		"/v1/playback/hls/sessions",
		"/v1/playback/dash/sessions",
	} {
		body, _ := json.Marshal(map[string]any{"input_url": origin.URL + "/sample.mp4"})
		rr := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, endpoint, bytes.NewReader(body))
		s.Handler().ServeHTTP(rr, req)
		if rr.Code != http.StatusCreated {
			t.Fatalf("%s status=%d body=%s", endpoint, rr.Code, rr.Body.String())
		}
	}
}

func TestConfiguredHTTPLibraryUsesProfilePlayback(t *testing.T) {
	media, err := os.ReadFile(filepath.Join("..", "testdata", "sample.mp4"))
	if err != nil {
		t.Fatal(err)
	}
	var requestedPath string
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestedPath = r.URL.EscapedPath()
		if r.Header.Get("Authorization") != "Bearer library-secret" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("ETag", `"sample-v1"`)
		http.ServeContent(w, r, "sample.mp4", time.Unix(1, 0), bytes.NewReader(media))
	}))
	defer origin.Close()
	originURL, err := url.Parse(origin.URL)
	if err != nil {
		t.Fatal(err)
	}

	s := New(Config{
		RequestTimeout:   time.Minute,
		CacheRoot:        t.TempDir(),
		HTTPAllowedHosts: []string{originURL.Host},
		Libraries: map[string]LibraryConfig{
			"cached": {HTTP: &HTTPLibraryConfig{BaseURL: origin.URL + "/media/", Headers: map[string]string{"Authorization": "Bearer library-secret"}}},
		},
		Profiles: map[string]PlaybackProfile{
			"hls": {Container: "hls", SegmentSeconds: 4},
		},
	})
	t.Cleanup(s.Close)

	sess, err := s.ensureLibraryHLSSession(context.Background(), "hls", "cached", "folder/sample.mp4")
	if err != nil {
		t.Fatal(err)
	}
	if sess == nil || sess.InputPath == "" {
		t.Fatal("HTTP library did not create a playback session")
	}
	if requestedPath != "/media/folder/sample.mp4" {
		t.Fatalf("requested path = %q", requestedPath)
	}
}

func TestServerRejectsHTTPSourceWithoutAllowlist(t *testing.T) {
	s := New(Config{RequestTimeout: time.Minute})
	t.Cleanup(s.Close)
	body, _ := json.Marshal(map[string]string{"input_url": "https://example.com/video.mp4"})
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/probe", bytes.NewReader(body))
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
}

func TestHostMatchesAllowlist(t *testing.T) {
	for _, tc := range []struct {
		raw     string
		allowed []string
		want    bool
	}{
		{"https://media.example.com/file", []string{"media.example.com"}, true},
		{"https://media.example.com:8443/file", []string{"media.example.com:8443"}, true},
		{"https://cdn.example.com/file", []string{"*.example.com"}, true},
		{"https://example.com/file", []string{"*.example.com"}, false},
		{"https://anything.invalid/file", []string{"*"}, true},
	} {
		u, err := url.Parse(tc.raw)
		if err != nil {
			t.Fatal(err)
		}
		if got := hostMatchesAllowlist(u, cleanHTTPAllowedHosts(tc.allowed)); got != tc.want {
			t.Fatalf("hostMatchesAllowlist(%q, %v)=%v want=%v", tc.raw, tc.allowed, got, tc.want)
		}
	}
}
