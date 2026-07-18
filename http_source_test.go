package transcoder

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
)

func TestHTTPSourceReadSeekAndHeaders(t *testing.T) {
	data := []byte("abcdefghijklmnopqrstuvwxyz")
	const etag = `"object-v1"`
	var mu sync.Mutex
	var ranges []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer secret" {
			http.Error(w, "missing authorization", http.StatusUnauthorized)
			return
		}
		rawRange := r.Header.Get("Range")
		mu.Lock()
		ranges = append(ranges, rawRange)
		mu.Unlock()
		start, end, ok := testHTTPRange(rawRange, int64(len(data)))
		if !ok {
			http.Error(w, "range required", http.StatusRequestedRangeNotSatisfiable)
			return
		}
		w.Header().Set("Accept-Ranges", "bytes")
		w.Header().Set("Content-Type", "video/mp4")
		w.Header().Set("ETag", etag)
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(data)))
		w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(data[start : end+1])
	}))
	defer server.Close()

	source, info, err := NewHTTPSource(context.Background(), server.URL+"/media", HTTPSourceOptions{
		Headers: http.Header{"Authorization": []string{"Bearer secret"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if info.Size != int64(len(data)) || info.ETag != etag || info.ContentType != "video/mp4" {
		t.Fatalf("unexpected source info: %+v", info)
	}
	reader, err := source.OpenReadSeeker()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()

	buf := make([]byte, 4)
	if _, err := io.ReadFull(reader, buf); err != nil || string(buf) != "abcd" {
		t.Fatalf("initial read=%q err=%v", buf, err)
	}
	if pos, err := reader.Seek(10, io.SeekStart); err != nil || pos != 10 {
		t.Fatalf("seek start pos=%d err=%v", pos, err)
	}
	buf = make([]byte, 3)
	if _, err := io.ReadFull(reader, buf); err != nil || string(buf) != "klm" {
		t.Fatalf("middle read=%q err=%v", buf, err)
	}
	if pos, err := reader.Seek(-3, io.SeekEnd); err != nil || pos != int64(len(data)-3) {
		t.Fatalf("seek end pos=%d err=%v", pos, err)
	}
	buf = make([]byte, 3)
	if _, err := io.ReadFull(reader, buf); err != nil || string(buf) != "xyz" {
		t.Fatalf("tail read=%q err=%v", buf, err)
	}

	mu.Lock()
	gotRanges := append([]string(nil), ranges...)
	mu.Unlock()
	want := []string{"bytes=0-0", "bytes=0-", "bytes=10-", "bytes=23-"}
	if strings.Join(gotRanges, ",") != strings.Join(want, ",") {
		t.Fatalf("ranges=%v want=%v", gotRanges, want)
	}
}

func TestHTTPSourceRejectsNonRangeOrigin(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "4")
		_, _ = w.Write([]byte("data"))
	}))
	defer server.Close()

	_, _, err := NewHTTPSource(context.Background(), server.URL, HTTPSourceOptions{})
	if err == nil || !strings.Contains(err.Error(), ErrHTTPRangeUnsupported.Error()) {
		t.Fatalf("expected range support error, got %v", err)
	}
}

func testHTTPRange(value string, size int64) (start, end int64, ok bool) {
	if !strings.HasPrefix(value, "bytes=") {
		return 0, 0, false
	}
	parts := strings.SplitN(strings.TrimPrefix(value, "bytes="), "-", 2)
	if len(parts) != 2 || parts[0] == "" {
		return 0, 0, false
	}
	start, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || start < 0 || start >= size {
		return 0, 0, false
	}
	end = size - 1
	if parts[1] != "" {
		end, err = strconv.ParseInt(parts[1], 10, 64)
		if err != nil || end < start || end >= size {
			return 0, 0, false
		}
	}
	return start, end, true
}
