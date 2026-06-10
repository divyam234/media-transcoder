package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	transcoder "media-transcoder"
)

type DynamicHLSSessionRequest struct {
	InputPath       string                     `json:"input_path"`
	Options         transcoder.HLSOptions      `json:"options"`
	Variants        []transcoder.LadderVariant `json:"variants,omitempty"`
	CacheDir        string                     `json:"cache_dir,omitempty"`
	PrewarmSegments int                        `json:"prewarm_segments,omitempty"`
}

type DynamicHLSSessionResponse struct {
	ID           string               `json:"id"`
	MasterURL    string               `json:"master_url"`
	PlaylistURL  string               `json:"playlist_url"`
	Duration     float64              `json:"duration"`
	SegmentTime  float64              `json:"segment_time"`
	SegmentCount int                  `json:"segment_count"`
	Variants     []DynamicHLSVariant  `json:"variants,omitempty"`
	Info         transcoder.MediaInfo `json:"info"`
}

type DynamicHLSVariant struct {
	Name      string                `json:"name"`
	Width     int                   `json:"width,omitempty"`
	Height    int                   `json:"height,omitempty"`
	Bandwidth int                   `json:"bandwidth,omitempty"`
	Options   transcoder.HLSOptions `json:"-"`
}

type DynamicHLSSession struct {
	ID              string
	InputPath       string
	Options         transcoder.HLSOptions
	Variants        []DynamicHLSVariant
	CacheDir        string
	PrewarmSegments int
	Info            transcoder.MediaInfo
	CreatedAt       time.Time
	ctx             context.Context
	cancel          context.CancelFunc
}

type DynamicHLSManager struct {
	mu       sync.RWMutex
	sessions map[string]*DynamicHLSSession
	locks    map[string]*sync.Mutex
	sem      chan struct{}
}

func NewDynamicHLSManager(maxConcurrent int) *DynamicHLSManager {
	if maxConcurrent <= 0 {
		maxConcurrent = 2
	}
	return &DynamicHLSManager{sessions: map[string]*DynamicHLSSession{}, locks: map[string]*sync.Mutex{}, sem: make(chan struct{}, maxConcurrent)}
}

func (m *DynamicHLSManager) Add(s *DynamicHLSSession) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sessions[s.ID] = s
}
func (m *DynamicHLSManager) Get(id string) (*DynamicHLSSession, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	sess, ok := m.sessions[id]
	return sess, ok
}
func (m *DynamicHLSManager) Delete(id string) (*DynamicHLSSession, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	sess, ok := m.sessions[id]
	if !ok {
		return nil, false
	}
	delete(m.sessions, id)
	for key := range m.locks {
		if strings.HasPrefix(key, id+":") {
			delete(m.locks, key)
		}
	}
	return sess, true
}
func (m *DynamicHLSManager) segmentLock(key string) *sync.Mutex {
	m.mu.Lock()
	defer m.mu.Unlock()
	l := m.locks[key]
	if l == nil {
		l = &sync.Mutex{}
		m.locks[key] = l
	}
	return l
}

func newID() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

func (s *Server) createDynamicHLSSession(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	var req DynamicHLSSessionRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.InputPath == "" {
		writeError(w, http.StatusBadRequest, errors.New("input_path is required"))
		return
	}
	requestedAudioMode := req.Options.AudioMode
	req.Options.ApplyDefaults("virtual.m3u8")
	// Dynamic playback should not perform a full static HLS transcode. SegmentPattern is irrelevant here.
	if req.Options.SegmentSeconds <= 0 {
		req.Options.SegmentSeconds = 4
	}
	if err := s.validateInputPath(req.InputPath); err != nil {
		writeError(w, http.StatusForbidden, err)
		return
	}
	info, err := transcoder.ProbeFile(ctx, req.InputPath)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if requestedAudioMode == "" {
		if info.HasAudio {
			req.Options.AudioMode = transcoder.AudioTranscode
		} else {
			req.Options.AudioMode = transcoder.AudioSkip
		}
	}
	id := newID()
	cacheRoot := s.cacheRootFor(req.CacheDir, "go-media-transcoder-hls")
	cacheDir := filepath.Join(cacheRoot, id)
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	sessCtx, cancel := context.WithCancel(context.Background())
	sess := &DynamicHLSSession{ID: id, InputPath: req.InputPath, Options: req.Options, CacheDir: cacheDir, PrewarmSegments: req.PrewarmSegments, Info: info, CreatedAt: time.Now(), ctx: sessCtx, cancel: cancel}
	sess.Variants = buildHLSVariants(req.Options, req.Variants, info)
	s.dynHLS.Add(sess)
	s.metrics.hlsSessions.Add(1)
	writeJSON(w, http.StatusCreated, s.dynamicHLSResponse(r, sess))
}

func (s *Server) dynamicHLSResponse(r *http.Request, sess *DynamicHLSSession) DynamicHLSSessionResponse {
	segs := int(math.Ceil(sess.Info.Duration / sess.Options.SegmentSeconds))
	base := "/v1/playback/hls/" + sess.ID
	return DynamicHLSSessionResponse{ID: sess.ID, MasterURL: base + "/master.m3u8", PlaylistURL: base + "/video.m3u8", Duration: sess.Info.Duration, SegmentTime: sess.Options.SegmentSeconds, SegmentCount: segs, Variants: sess.Variants, Info: sess.Info}
}

func (s *Server) dynamicHLSMaster(_ context.Context, w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	sess, ok := s.dynHLS.Get(id)
	if !ok {
		writeError(w, http.StatusNotFound, errors.New("hls session not found"))
		return
	}
	var b strings.Builder
	b.WriteString("#EXTM3U\n#EXT-X-VERSION:6\n")
	vars := sess.Variants
	if len(vars) == 0 {
		vars = buildHLSVariants(sess.Options, nil, sess.Info)
	}
	for _, v := range vars {
		uri := "video.m3u8"
		if v.Name != "default" {
			uri = "variant/" + url.PathEscape(v.Name) + "/video.m3u8"
		}
		b.WriteString(fmt.Sprintf("#EXT-X-STREAM-INF:BANDWIDTH=%d,RESOLUTION=%dx%d\n%s\n", v.Bandwidth, v.Width, v.Height, uri))
	}
	body := b.String()
	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	_, _ = w.Write([]byte(body))
}

func (s *Server) deleteDynamicHLSSession(_ context.Context, w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	sess, ok := s.dynHLS.Delete(id)
	if !ok {
		writeError(w, http.StatusNotFound, errors.New("hls session not found"))
		return
	}
	if sess.cancel != nil {
		sess.cancel()
	}
	_ = os.RemoveAll(sess.CacheDir)
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "deleted"})
}

func (s *Server) dynamicHLSPlaylist(_ context.Context, w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	sess, ok := s.dynHLS.Get(id)
	if !ok {
		writeError(w, http.StatusNotFound, errors.New("hls session not found"))
		return
	}
	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	_, _ = w.Write([]byte(buildDynamicPlaylist(sess, defaultHLSVariant(sess))))
}

func buildDynamicPlaylist(sess *DynamicHLSSession, variant DynamicHLSVariant) string {
	dur := sess.Info.Duration
	segDur := variant.Options.SegmentSeconds
	if segDur <= 0 {
		segDur = 4
	}
	count := int(math.Ceil(dur / segDur))
	if count < 1 {
		count = 1
	}
	target := int(math.Ceil(segDur))
	var b strings.Builder
	b.WriteString("#EXTM3U\n")
	b.WriteString("#EXT-X-VERSION:6\n")
	b.WriteString(fmt.Sprintf("#EXT-X-TARGETDURATION:%d\n", target))
	b.WriteString("#EXT-X-MEDIA-SEQUENCE:0\n")
	b.WriteString("#EXT-X-PLAYLIST-TYPE:VOD\n")
	b.WriteString("#EXT-X-INDEPENDENT-SEGMENTS\n")
	for i := 0; i < count; i++ {
		d := segDur
		if end := float64(i+1) * segDur; end > dur && dur > float64(i)*segDur {
			d = dur - float64(i)*segDur
		}
		if d <= 0 {
			d = segDur
		}
		b.WriteString(fmt.Sprintf("#EXTINF:%.3f,\nsegment/%06d.ts\n", d, i))
	}
	b.WriteString("#EXT-X-ENDLIST\n")
	return b.String()
}

func hlsSegmentPath(sess *DynamicHLSSession, variant DynamicHLSVariant, idx int) string {
	name := safeVariantName(variant.Name)
	if name == "" || name == "default" {
		return filepath.Join(sess.CacheDir, fmt.Sprintf("%06d.ts", idx))
	}
	return filepath.Join(sess.CacheDir, name, fmt.Sprintf("%06d.ts", idx))
}

func buildHLSVariants(base transcoder.HLSOptions, requested []transcoder.LadderVariant, info transcoder.MediaInfo) []DynamicHLSVariant {
	if len(requested) == 0 {
		width := base.Width
		if width <= 0 {
			width = info.Width
		}
		height := scaledHeight(width, info)
		bw := base.VideoBitrate
		if bw <= 0 {
			bw = max(1, width) * 2200
		}
		return []DynamicHLSVariant{{Name: "default", Width: width, Height: height, Bandwidth: bw, Options: base}}
	}
	out := make([]DynamicHLSVariant, 0, len(requested))
	for _, rv := range requested {
		name := safeVariantName(rv.Name)
		if name == "" || name == "default" {
			if rv.Height > 0 {
				name = fmt.Sprintf("%dp", rv.Height)
			} else if rv.Width > 0 {
				name = fmt.Sprintf("w%d", rv.Width)
			} else {
				name = fmt.Sprintf("v%d", len(out))
			}
		}
		opts := base
		if rv.Width > 0 {
			opts.Width = rv.Width
		}
		if rv.FPS > 0 {
			opts.FPS = rv.FPS
		}
		if rv.CRF > 0 {
			opts.CRF = rv.CRF
		}
		if rv.VideoBitrate > 0 {
			opts.VideoBitrate = rv.VideoBitrate
		}
		if rv.AudioBitrate > 0 {
			opts.AudioBitrate = rv.AudioBitrate
		}
		width := opts.Width
		if width <= 0 {
			width = info.Width
		}
		height := rv.Height
		if height <= 0 {
			height = scaledHeight(width, info)
		}
		bw := rv.VideoBitrate + rv.AudioBitrate
		if bw <= 0 {
			bw = opts.VideoBitrate + opts.AudioBitrate
		}
		if bw <= 0 {
			bw = max(1, width) * 2200
		}
		out = append(out, DynamicHLSVariant{Name: name, Width: width, Height: height, Bandwidth: bw, Options: opts})
	}
	return out
}

func scaledHeight(width int, info transcoder.MediaInfo) int {
	if width <= 0 {
		return info.Height
	}
	if info.Width <= 0 || info.Height <= 0 {
		return 0
	}
	return int(math.Round(float64(width) * float64(info.Height) / float64(info.Width)))
}

func safeVariantName(name string) string {
	name = strings.TrimSpace(name)
	var b strings.Builder
	for _, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func defaultHLSVariant(sess *DynamicHLSSession) DynamicHLSVariant {
	if len(sess.Variants) > 0 {
		return sess.Variants[0]
	}
	return buildHLSVariants(sess.Options, nil, sess.Info)[0]
}

func findHLSVariant(sess *DynamicHLSSession, name string) (DynamicHLSVariant, bool) {
	if name == "" || name == "default" {
		return defaultHLSVariant(sess), true
	}
	name = safeVariantName(name)
	for _, v := range sess.Variants {
		if v.Name == name {
			return v, true
		}
	}
	return DynamicHLSVariant{}, false
}

func parseSegmentIndex(name string) (int, error) {
	name = strings.TrimSuffix(name, ".ts")
	name = strings.TrimPrefix(name, "seg-")
	name = strings.TrimPrefix(name, "segment-")
	if name == "" {
		return 0, errors.New("empty segment index")
	}
	n, err := strconv.Atoi(name)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("invalid segment index %q", name)
	}
	return n, nil
}

func (s *Server) dynamicHLSSegment(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	s.dynamicHLSVariantSegment(ctx, w, r, "default")
}

func (s *Server) dynamicHLSVariantPlaylist(_ context.Context, w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	sess, ok := s.dynHLS.Get(id)
	if !ok {
		writeError(w, http.StatusNotFound, errors.New("hls session not found"))
		return
	}
	variant, ok := findHLSVariant(sess, r.PathValue("variant"))
	if !ok {
		writeError(w, http.StatusNotFound, errors.New("hls variant not found"))
		return
	}
	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	_, _ = w.Write([]byte(buildDynamicPlaylist(sess, variant)))
}

func (s *Server) dynamicHLSNamedVariantSegment(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	s.dynamicHLSVariantSegment(ctx, w, r, r.PathValue("variant"))
}

func (s *Server) dynamicHLSVariantSegment(ctx context.Context, w http.ResponseWriter, r *http.Request, variantName string) {
	id := r.PathValue("id")
	sess, ok := s.dynHLS.Get(id)
	if !ok {
		writeError(w, http.StatusNotFound, errors.New("hls session not found"))
		return
	}
	idx, err := parseSegmentIndex(r.PathValue("name"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	variant, ok := findHLSVariant(sess, variantName)
	if !ok {
		writeError(w, http.StatusNotFound, errors.New("hls variant not found"))
		return
	}
	segDur := variant.Options.SegmentSeconds
	count := int(math.Ceil(sess.Info.Duration / segDur))
	if idx < 0 || idx >= count {
		writeError(w, http.StatusRequestedRangeNotSatisfiable, errors.New("segment index outside media duration"))
		return
	}
	path := hlsSegmentPath(sess, variant, idx)
	if err := s.ensureDynamicVariantSegment(ctx, sess, variant, idx, path); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	for n := 1; n <= sess.PrewarmSegments; n++ {
		next := idx + n
		if next < count {
			go func(i int) {
				_ = s.ensureDynamicVariantSegment(sess.ctx, sess, variant, i, hlsSegmentPath(sess, variant, i))
			}(next)
		}
	}
	w.Header().Set("Content-Type", "video/mp2t")
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	http.ServeFile(w, r, path)
}

func (s *Server) ensureDynamicSegment(ctx context.Context, sess *DynamicHLSSession, idx int, path string) error {
	return s.ensureDynamicVariantSegment(ctx, sess, defaultHLSVariant(sess), idx, path)
}

func (s *Server) ensureDynamicVariantSegment(ctx context.Context, sess *DynamicHLSSession, variant DynamicHLSVariant, idx int, path string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if sess.ctx != nil {
		if err := sess.ctx.Err(); err != nil {
			return err
		}
	}
	if st, err := os.Stat(path); err == nil && st.Size() > 0 {
		s.metrics.segmentCacheHits.Add(1)
		return nil
	}
	lock := s.dynHLS.segmentLock(sess.ID + ":" + variant.Name + ":" + strconv.Itoa(idx))
	lock.Lock()
	defer lock.Unlock()
	if st, err := os.Stat(path); err == nil && st.Size() > 0 {
		s.metrics.segmentCacheHits.Add(1)
		return nil
	}
	select {
	case s.dynHLS.sem <- struct{}{}:
		defer func() { <-s.dynHLS.sem }()
	case <-ctx.Done():
		return ctx.Err()
	case <-sess.ctx.Done():
		return sess.ctx.Err()
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	_ = os.Remove(tmp)
	opts := variant.Options.TranscodeOptions
	opts.StartTime = float64(idx) * variant.Options.SegmentSeconds
	opts.Duration = variant.Options.SegmentSeconds
	opts.FastStart = false
	// Audio copy inside arbitrary on-demand segments needs timestamp-perfect trimming.
	// Keep the default safe for browser seeking; audio support can be enabled by caller once validated for the source/profile.
	if opts.AudioMode == "" {
		if sess.Info.HasAudio {
			opts.AudioMode = transcoder.AudioTranscode
		} else {
			opts.AudioMode = transcoder.AudioSkip
		}
	}
	genCtx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(sess.ctx, cancel)
	defer stop()
	defer cancel()
	if _, err := transcoder.TranscodeSegmentFromFile(genCtx, sess.InputPath, tmp, opts); err != nil {
		s.metrics.segmentErrors.Add(1)
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		s.metrics.segmentErrors.Add(1)
		return err
	}
	s.metrics.segmentsGenerated.Add(1)
	return nil
}
