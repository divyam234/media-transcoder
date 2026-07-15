package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestServerProbe(t *testing.T) {
	s := New(Config{RequestTimeout: time.Minute})
	t.Cleanup(s.Close)
	body, _ := json.Marshal(map[string]string{"input_path": filepath.Join("..", "testdata", "sample.mp4")})
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/probe", bytes.NewReader(body))
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
}

func TestStaticTranscodeRoutesAreNotExposed(t *testing.T) {
	s := New(Config{RequestTimeout: time.Minute})
	t.Cleanup(s.Close)
	body, _ := json.Marshal(map[string]any{"input_path": filepath.Join("..", "testdata", "sample.mp4"), "output_path": filepath.Join(t.TempDir(), "out.mp4")})
	for _, tc := range []struct {
		method string
		path   string
	}{
		{http.MethodPost, "/v1/transcode/progressive"},
		{http.MethodPost, "/v1/transcode/hls"},
		{http.MethodPost, "/v1/transcode/dash"},
		{http.MethodPost, "/v1/transcode/abr-hls"},
		{http.MethodPost, "/v1/transcode/profile"},
		{http.MethodPost, "/v1/sessions"},
		{http.MethodGet, "/v1/sessions"},
	} {
		rr := httptest.NewRecorder()
		req := httptest.NewRequest(tc.method, tc.path, bytes.NewReader(body))
		s.Handler().ServeHTTP(rr, req)
		if rr.Code != http.StatusNotFound && rr.Code != http.StatusMethodNotAllowed {
			t.Fatalf("%s %s should not be exposed, got status=%d body=%s", tc.method, tc.path, rr.Code, rr.Body.String())
		}
	}
}

func TestServerCapabilitiesAuthAndRateLimit(t *testing.T) {
	s := New(Config{RequestTimeout: time.Minute, APIKeys: []string{"secret"}, RateLimitPerMinute: 1})
	t.Cleanup(s.Close)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/capabilities", nil)
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("unauth status=%d", rr.Code)
	}

	rr = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/v1/capabilities", nil)
	req.Header.Set("X-API-Key", "secret")
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("auth status=%d body=%s", rr.Code, rr.Body.String())
	}

	rr = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/v1/capabilities", nil)
	req.Header.Set("X-API-Key", "secret")
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("rate status=%d body=%s", rr.Code, rr.Body.String())
	}
}

func TestServerRuntimeCapabilityRoutes(t *testing.T) {
	s := New(Config{RequestTimeout: time.Minute})
	t.Cleanup(s.Close)
	for _, path := range []string{
		"/v1/capabilities",
		"/v1/capabilities/runtime",
		"/v1/capabilities/codecs",
		"/v1/capabilities/hardware",
	} {
		rr := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		s.Handler().ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("%s status=%d body=%s", path, rr.Code, rr.Body.String())
		}
		body := rr.Body.String()
		if !strings.Contains(body, "video") && !strings.Contains(body, "ffmpeg") && !strings.Contains(body, "matrix") {
			t.Fatalf("%s returned unexpected payload: %s", path, body)
		}
	}
}

func TestServerOpenAPISchemaRoute(t *testing.T) {
	s := New(Config{RequestTimeout: time.Minute, APIKeys: []string{"secret"}})
	t.Cleanup(s.Close)
	for _, path := range []string{"/openapi.yaml", "/v1/openapi.yaml"} {
		rr := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		s.Handler().ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("%s status=%d body=%s", path, rr.Code, rr.Body.String())
		}
		body := rr.Body.String()
		if !strings.Contains(body, "openapi: 3.1.0") || !strings.Contains(body, "/v1/playback/hls/sessions") {
			t.Fatalf("%s returned invalid schema", path)
		}
	}
}
