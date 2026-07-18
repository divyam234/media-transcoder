package transcoder

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

var ErrHTTPRangeUnsupported = errors.New("http source does not support byte ranges")

// HTTPSourceOptions configures a seekable HTTP media source. Headers are copied
// onto probe and range requests, except headers controlled by the reader itself.
type HTTPSourceOptions struct {
	Client       *http.Client
	Headers      http.Header
	ExpectedSize int64
	ExpectedETag string
}

// HTTPSourceInfo describes the immutable object observed during the initial
// range probe.
type HTTPSourceInfo struct {
	URL          string
	Size         int64
	ETag         string
	LastModified time.Time
	ContentType  string
}

// NewHTTPSource probes an HTTP or HTTPS object and returns a Source backed by
// independent HTTP range readers. The origin must return 206 and a valid
// Content-Range response for a bytes=0-0 request.
func NewHTTPSource(ctx context.Context, rawURL string, opts HTTPSourceOptions) (Source, HTTPSourceInfo, error) {
	parsed, err := validateHTTPSourceURL(rawURL)
	if err != nil {
		return Source{}, HTTPSourceInfo{}, err
	}
	info, err := probeHTTPSource(ctx, parsed.String(), opts)
	if err != nil {
		return Source{}, HTTPSourceInfo{}, err
	}
	name := parsed.Host + parsed.EscapedPath()
	if name == "" {
		name = "http-input"
	}
	client := opts.Client
	if client == nil {
		client = http.DefaultClient
	}
	headers := cloneHTTPSourceHeaders(opts.Headers)
	source := FromReadSeekerFactory(name, func() (io.ReadSeekCloser, error) {
		return newHTTPRangeReadSeeker(parsed.String(), client, headers, info), nil
	})
	return source, info, nil
}

func validateHTTPSourceURL(rawURL string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return nil, fmt.Errorf("parse http source URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("http source URL must use http or https")
	}
	if u.Host == "" {
		return nil, fmt.Errorf("http source URL requires a host")
	}
	if u.User != nil {
		return nil, fmt.Errorf("http source URL must not contain user info")
	}
	if u.Fragment != "" {
		return nil, fmt.Errorf("http source URL must not contain a fragment")
	}
	return u, nil
}

func probeHTTPSource(ctx context.Context, rawURL string, opts HTTPSourceOptions) (HTTPSourceInfo, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return HTTPSourceInfo{}, err
	}
	applyHTTPSourceHeaders(req.Header, opts.Headers)
	req.Header.Set("Range", "bytes=0-0")
	req.Header.Set("Accept-Encoding", "identity")
	client := opts.Client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return HTTPSourceInfo{}, fmt.Errorf("probe http source: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusPartialContent {
		return HTTPSourceInfo{}, fmt.Errorf("%w: probe returned %s", ErrHTTPRangeUnsupported, resp.Status)
	}
	if encoding := strings.TrimSpace(resp.Header.Get("Content-Encoding")); encoding != "" && !strings.EqualFold(encoding, "identity") {
		return HTTPSourceInfo{}, fmt.Errorf("http source returned unsupported content encoding %q", encoding)
	}
	start, end, total, ok := parseHTTPContentRange(resp.Header.Get("Content-Range"))
	if !ok || start != 0 || end != 0 || total <= 0 {
		return HTTPSourceInfo{}, fmt.Errorf("%w: invalid Content-Range %q", ErrHTTPRangeUnsupported, resp.Header.Get("Content-Range"))
	}
	if opts.ExpectedSize > 0 && opts.ExpectedSize != total {
		return HTTPSourceInfo{}, fmt.Errorf("http source size changed: expected %d, got %d", opts.ExpectedSize, total)
	}
	etag := strings.TrimSpace(resp.Header.Get("ETag"))
	if expected := strings.TrimSpace(opts.ExpectedETag); expected != "" && etag != expected {
		return HTTPSourceInfo{}, fmt.Errorf("http source ETag changed: expected %q, got %q", expected, etag)
	}
	var lastModified time.Time
	if raw := resp.Header.Get("Last-Modified"); raw != "" {
		lastModified, _ = http.ParseTime(raw)
	}
	var one [1]byte
	if _, err := io.ReadFull(resp.Body, one[:]); err != nil {
		return HTTPSourceInfo{}, fmt.Errorf("read http source probe: %w", err)
	}
	return HTTPSourceInfo{
		URL:          rawURL,
		Size:         total,
		ETag:         etag,
		LastModified: lastModified,
		ContentType:  strings.TrimSpace(resp.Header.Get("Content-Type")),
	}, nil
}

type httpRangeReadSeeker struct {
	url     string
	client  *http.Client
	headers http.Header
	info    HTTPSourceInfo

	ctx    context.Context
	cancel context.CancelFunc
	body   io.ReadCloser
	pos    int64
	closed bool
}

func newHTTPRangeReadSeeker(rawURL string, client *http.Client, headers http.Header, info HTTPSourceInfo) *httpRangeReadSeeker {
	ctx, cancel := context.WithCancel(context.Background())
	return &httpRangeReadSeeker{
		url:     rawURL,
		client:  client,
		headers: cloneHTTPSourceHeaders(headers),
		info:    info,
		ctx:     ctx,
		cancel:  cancel,
	}
}

func (r *httpRangeReadSeeker) Read(p []byte) (int, error) {
	if r.closed {
		return 0, errors.New("http source is closed")
	}
	if len(p) == 0 {
		return 0, nil
	}
	if r.pos >= r.info.Size {
		return 0, io.EOF
	}
	if r.body == nil {
		if err := r.openRange(); err != nil {
			return 0, err
		}
	}
	remaining := r.info.Size - r.pos
	if int64(len(p)) > remaining {
		p = p[:remaining]
	}
	n, err := r.body.Read(p)
	r.pos += int64(n)
	if err != nil {
		_ = r.closeBody()
		if errors.Is(err, io.EOF) && r.pos < r.info.Size {
			err = io.ErrUnexpectedEOF
		}
	}
	if n == 0 && err == nil {
		return 0, io.ErrNoProgress
	}
	if r.pos >= r.info.Size && err == nil {
		_ = r.closeBody()
	}
	return n, err
}

func (r *httpRangeReadSeeker) Seek(offset int64, whence int) (int64, error) {
	if r.closed {
		return 0, errors.New("http source is closed")
	}
	var next int64
	switch whence {
	case io.SeekStart:
		next = offset
	case io.SeekCurrent:
		next = r.pos + offset
	case io.SeekEnd:
		next = r.info.Size + offset
	default:
		return 0, fmt.Errorf("invalid seek whence %d", whence)
	}
	if next < 0 || next > r.info.Size {
		return 0, fmt.Errorf("seek offset %d outside source size %d", next, r.info.Size)
	}
	if next != r.pos {
		r.closeBody()
		r.pos = next
	}
	return r.pos, nil
}

func (r *httpRangeReadSeeker) Close() error {
	if r.closed {
		return nil
	}
	r.closed = true
	r.cancel()
	return r.closeBody()
}

func (r *httpRangeReadSeeker) openRange() error {
	req, err := http.NewRequestWithContext(r.ctx, http.MethodGet, r.url, nil)
	if err != nil {
		return err
	}
	applyHTTPSourceHeaders(req.Header, r.headers)
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-", r.pos))
	req.Header.Set("Accept-Encoding", "identity")
	if r.info.ETag != "" {
		req.Header.Set("If-Range", r.info.ETag)
	} else if !r.info.LastModified.IsZero() {
		req.Header.Set("If-Range", r.info.LastModified.UTC().Format(http.TimeFormat))
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return fmt.Errorf("open http source range at %d: %w", r.pos, err)
	}
	if resp.StatusCode != http.StatusPartialContent {
		resp.Body.Close()
		return fmt.Errorf("%w: range at %d returned %s", ErrHTTPRangeUnsupported, r.pos, resp.Status)
	}
	if encoding := strings.TrimSpace(resp.Header.Get("Content-Encoding")); encoding != "" && !strings.EqualFold(encoding, "identity") {
		resp.Body.Close()
		return fmt.Errorf("http source returned unsupported content encoding %q", encoding)
	}
	start, end, total, ok := parseHTTPContentRange(resp.Header.Get("Content-Range"))
	if !ok || start != r.pos || end != r.info.Size-1 || total != r.info.Size {
		resp.Body.Close()
		return fmt.Errorf("http source returned unexpected Content-Range %q", resp.Header.Get("Content-Range"))
	}
	if r.info.ETag != "" && strings.TrimSpace(resp.Header.Get("ETag")) != r.info.ETag {
		resp.Body.Close()
		return fmt.Errorf("http source ETag changed during read")
	}
	if r.info.ETag == "" && !r.info.LastModified.IsZero() {
		modified, parseErr := http.ParseTime(resp.Header.Get("Last-Modified"))
		if parseErr != nil || !modified.Equal(r.info.LastModified) {
			resp.Body.Close()
			return fmt.Errorf("http source modification time changed during read")
		}
	}
	r.body = resp.Body
	return nil
}

func (r *httpRangeReadSeeker) closeBody() error {
	if r.body == nil {
		return nil
	}
	body := r.body
	r.body = nil
	return body.Close()
}

func cloneHTTPSourceHeaders(headers http.Header) http.Header {
	out := make(http.Header, len(headers))
	applyHTTPSourceHeaders(out, headers)
	return out
}

func applyHTTPSourceHeaders(dst, src http.Header) {
	for name, values := range src {
		if strings.EqualFold(name, "Range") || strings.EqualFold(name, "If-Range") || strings.EqualFold(name, "Accept-Encoding") {
			continue
		}
		for _, value := range values {
			dst.Add(name, value)
		}
	}
}

func parseHTTPContentRange(value string) (start, end, total int64, ok bool) {
	value = strings.TrimSpace(value)
	if !strings.HasPrefix(strings.ToLower(value), "bytes ") {
		return 0, 0, 0, false
	}
	value = strings.TrimSpace(value[len("bytes "):])
	parts := strings.Split(value, "/")
	if len(parts) != 2 || parts[1] == "*" {
		return 0, 0, 0, false
	}
	span := strings.Split(parts[0], "-")
	if len(span) != 2 {
		return 0, 0, 0, false
	}
	start, err1 := strconv.ParseInt(span[0], 10, 64)
	end, err2 := strconv.ParseInt(span[1], 10, 64)
	total, err3 := strconv.ParseInt(parts[1], 10, 64)
	if err1 != nil || err2 != nil || err3 != nil || start < 0 || end < start || total <= end {
		return 0, 0, 0, false
	}
	return start, end, total, true
}
