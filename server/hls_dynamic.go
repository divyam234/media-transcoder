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
	InputPath       string                     `json:"input_path,omitempty"`
	InputURL        string                     `json:"input_url,omitempty"`
	InputHeaders    map[string]string          `json:"input_headers,omitempty"`
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
	Name       string                     `json:"name"`
	Width      int                        `json:"width,omitempty"`
	Height     int                        `json:"height,omitempty"`
	Bandwidth  int                        `json:"bandwidth,omitempty"`
	VideoCodec transcoder.CodecDescriptor `json:"video_codec,omitempty"`
	AudioCodec transcoder.CodecDescriptor `json:"audio_codec,omitempty"`
	Options    transcoder.HLSOptions      `json:"-"`
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
	SourceKey       string
	InputCleanup    func()
	ctx             context.Context
	cancel          context.CancelFunc
	codecMu         sync.Mutex
	prewarmMu       sync.Mutex
	decoderMu       sync.Mutex
	videoDecoder    *transcoder.FMP4VideoDecoder
	decoderTimer    *time.Timer
}

func (sess *DynamicHLSSession) getVideoDecoder(opts transcoder.TranscodeOptions) (*transcoder.FMP4VideoDecoder, error) {
	sess.decoderMu.Lock()
	defer sess.decoderMu.Unlock()
	if sess.decoderTimer != nil {
		sess.decoderTimer.Stop()
		sess.decoderTimer = nil
	}
	if sess.videoDecoder == nil {
		dec, err := transcoder.NewFMP4VideoDecoder(sess.InputPath, opts)
		if err != nil {
			return nil, err
		}
		sess.videoDecoder = dec
	}
	return sess.videoDecoder, nil
}

func (sess *DynamicHLSSession) scheduleVideoDecoderClose() {
	sess.decoderMu.Lock()
	defer sess.decoderMu.Unlock()
	if sess.decoderTimer != nil {
		sess.decoderTimer.Stop()
	}
	sess.decoderTimer = time.AfterFunc(15*time.Second, func() {
		sess.decoderMu.Lock()
		defer sess.decoderMu.Unlock()
		if sess.videoDecoder != nil {
			sess.videoDecoder.Close()
			sess.videoDecoder = nil
		}
		sess.decoderTimer = nil
	})
}

func (sess *DynamicHLSSession) closeVideoDecoder() {
	sess.decoderMu.Lock()
	defer sess.decoderMu.Unlock()
	if sess.decoderTimer != nil {
		sess.decoderTimer.Stop()
		sess.decoderTimer = nil
	}
	if sess.videoDecoder != nil {
		sess.videoDecoder.Close()
		sess.videoDecoder = nil
	}
}

type DynamicHLSManager struct {
	mu       sync.RWMutex
	sessions map[string]*DynamicHLSSession
	locks    map[string]*sync.Mutex
	sem      chan struct{}
	wg       sync.WaitGroup
	closed   bool
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

func (m *DynamicHLSManager) ReplaceSourceSession(s *DynamicHLSSession) []*DynamicHLSSession {
	m.mu.Lock()
	defer m.mu.Unlock()
	var stale []*DynamicHLSSession
	if s.SourceKey != "" {
		for id, existing := range m.sessions {
			if id == s.ID || existing.SourceKey != s.SourceKey {
				continue
			}
			stale = append(stale, existing)
			delete(m.sessions, id)
			for key := range m.locks {
				if strings.HasPrefix(key, id+":") {
					delete(m.locks, key)
				}
			}
		}
	}
	m.sessions[s.ID] = s
	return stale
}

func (m *DynamicHLSManager) beginBackground() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return false
	}
	m.wg.Add(1)
	return true
}

func (m *DynamicHLSManager) endBackground() { m.wg.Done() }

func (m *DynamicHLSManager) Wait() { m.wg.Wait() }

func (m *DynamicHLSManager) StopAll() []*DynamicHLSSession {
	m.mu.Lock()
	m.closed = true
	defer m.mu.Unlock()
	out := make([]*DynamicHLSSession, 0, len(m.sessions))
	for id, sess := range m.sessions {
		out = append(out, sess)
		delete(m.sessions, id)
	}
	m.locks = map[string]*sync.Mutex{}
	return out
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
	requestedAudioMode := req.Options.AudioMode
	req.Options.ApplyDefaults("virtual.m3u8")
	// Dynamic playback should not perform a full static HLS transcode. SegmentPattern is irrelevant here.
	if req.Options.SegmentSeconds <= 0 {
		req.Options.SegmentSeconds = 4
	}
	resolved, err := s.resolveInput(ctx, req.InputPath, req.InputURL, req.InputHeaders)
	if err != nil {
		writeError(w, inputErrorStatus(err), err)
		return
	}
	keepInput := false
	defer func() {
		if !keepInput && resolved.Cleanup != nil {
			resolved.Cleanup()
		}
	}()
	info, err := transcoder.ProbeFile(ctx, resolved.Input)
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
	if req.PrewarmSegments <= 0 {
		req.PrewarmSegments = 3
	}
	id := newID()
	cacheRoot := s.cacheRootFor(req.CacheDir, "media-transcoder-hls")
	cacheDir := filepath.Join(cacheRoot, id)
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	sessCtx, cancel := context.WithCancel(context.Background())
	sess := &DynamicHLSSession{ID: id, InputPath: resolved.Input, InputCleanup: resolved.Cleanup, SourceKey: resolved.SourceKey, Options: req.Options, CacheDir: cacheDir, PrewarmSegments: req.PrewarmSegments, Info: info, CreatedAt: time.Now(), ctx: sessCtx, cancel: cancel}
	sess.Variants = buildHLSVariants(req.Options, req.Variants, info)
	s.dynHLS.Add(sess)
	keepInput = true
	s.metrics.hlsSessions.Add(1)
	s.logger.Info("hls session created", "id", id, "input", resolved.Display, "duration", info.Duration, "segments", int(math.Ceil(info.Duration/req.Options.SegmentSeconds)), "segment_seconds", req.Options.SegmentSeconds, "audio_mode", req.Options.AudioMode, "prewarm_segments", req.PrewarmSegments)
	writeJSON(w, http.StatusCreated, s.dynamicHLSResponse(r, sess))
}

func (s *Server) dynamicHLSResponse(r *http.Request, sess *DynamicHLSSession) DynamicHLSSessionResponse {
	segs := int(math.Ceil(sess.Info.Duration / sess.Options.SegmentSeconds))
	base := "/v1/playback/hls/" + sess.ID
	return DynamicHLSSessionResponse{ID: sess.ID, MasterURL: base + "/master.m3u8", PlaylistURL: base + "/video.m3u8", Duration: sess.Info.Duration, SegmentTime: sess.Options.SegmentSeconds, SegmentCount: segs, Variants: sess.Variants, Info: sess.Info}
}

func (s *Server) dynamicHLSMaster(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	id := routeParam(r, "id")
	sess, ok := s.dynHLS.Get(id)
	if !ok {
		writeError(w, http.StatusNotFound, errors.New("hls session not found"))
		return
	}
	if err := s.ensureHLSCodecDescriptors(ctx, sess); err != nil {
		s.logger.Warn("hls codec probe failed", "id", id, "err", err)
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	s.writeHLSMaster(w, sess)
}

func (s *Server) ensureHLSCodecDescriptors(ctx context.Context, sess *DynamicHLSSession) error {
	sess.codecMu.Lock()
	defer sess.codecMu.Unlock()
	for i := range sess.Variants {
		v := sess.Variants[i]
		needVideo := v.VideoCodec.CodecString == ""
		needAudio := sess.Info.HasAudio && v.Options.AudioMode != transcoder.AudioSkip && v.AudioCodec.CodecString == ""
		if !needVideo && !needAudio {
			continue
		}
		segmentPath := hlsSegmentPath(sess, v, 0)
		if err := s.ensureDynamicVariantSegment(ctx, sess, v, 0, segmentPath); err != nil {
			return err
		}
		probePath := segmentPath
		if hlsUsesFMP4(v) {
			probePath = hlsInitPath(sess, v)
		}
		if needVideo {
			codec, err := transcoder.ProbeCodec(probePath, false)
			if err != nil {
				return err
			}
			sess.Variants[i].VideoCodec = codec
		}
		if needAudio {
			codec, err := transcoder.ProbeCodec(probePath, true)
			if err != nil {
				return err
			}
			sess.Variants[i].AudioCodec = codec
		}
	}
	return nil
}

func (s *Server) writeHLSMaster(w http.ResponseWriter, sess *DynamicHLSSession) {
	body := buildHLSMasterPlaylist(sess)
	s.logger.Debug("hls master served", "id", sess.ID, "variants", len(sess.Variants))
	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	_, _ = w.Write([]byte(body))
}

func buildHLSMasterPlaylist(sess *DynamicHLSSession) string {
	var b strings.Builder
	b.WriteString("#EXTM3U\n#EXT-X-VERSION:7\n#EXT-X-INDEPENDENT-SEGMENTS\n")
	vars := sess.Variants
	if len(vars) == 0 {
		vars = buildHLSVariants(sess.Options, nil, sess.Info)
	}
	for _, v := range vars {
		uri := "video.m3u8"
		if v.Name != "default" {
			uri = "variant/" + url.PathEscape(v.Name) + "/video.m3u8"
		}
		codecs := v.VideoCodec.CodecString
		if codecs == "" {
			codecs = "avc1.64001f"
		}
		if v.Options.AudioMode != transcoder.AudioSkip && sess.Info.HasAudio {
			audioCodec := v.AudioCodec.CodecString
			if audioCodec == "" {
				audioCodec = "mp4a.40.2"
			}
			codecs += "," + audioCodec
		}
		fps := v.Options.FPS
		if fps <= 0 {
			fps = sess.Info.FPS
		}
		if fps > 0 {
			b.WriteString(fmt.Sprintf("#EXT-X-STREAM-INF:BANDWIDTH=%d,RESOLUTION=%dx%d,FRAME-RATE=%.3f,CODECS=\"%s\"\n%s\n", v.Bandwidth, v.Width, v.Height, fps, codecs, uri))
		} else {
			b.WriteString(fmt.Sprintf("#EXT-X-STREAM-INF:BANDWIDTH=%d,RESOLUTION=%dx%d,CODECS=\"%s\"\n%s\n", v.Bandwidth, v.Width, v.Height, codecs, uri))
		}
	}
	return b.String()
}

func (s *Server) deleteDynamicHLSSession(_ context.Context, w http.ResponseWriter, r *http.Request) {
	id := routeParam(r, "id")
	sess, ok := s.dynHLS.Delete(id)
	if !ok {
		writeError(w, http.StatusNotFound, errors.New("hls session not found"))
		return
	}
	if sess.cancel != nil {
		sess.cancel()
	}
	sess.closeVideoDecoder()
	if sess.InputCleanup != nil {
		s.sessionMu.Lock()
		s.retiredInputs = append(s.retiredInputs, sess.InputCleanup)
		s.sessionMu.Unlock()
	}
	_ = os.RemoveAll(sess.CacheDir)
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "deleted"})
}

func (s *Server) dynamicHLSPlaylist(_ context.Context, w http.ResponseWriter, r *http.Request) {
	id := routeParam(r, "id")
	sess, ok := s.dynHLS.Get(id)
	if !ok {
		writeError(w, http.StatusNotFound, errors.New("hls session not found"))
		return
	}
	variant := defaultHLSVariant(sess)
	s.prewarmHLS(sess, variant, 0, max(1, sess.PrewarmSegments))
	s.logger.Debug("hls playlist served", "id", id, "variant", variant.Name, "prewarm", sess.PrewarmSegments)
	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	_, _ = w.Write([]byte(buildDynamicPlaylist(sess, variant)))
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
	isFMP4 := hlsUsesFMP4(variant)
	target := int(math.Ceil(segDur))
	var b strings.Builder
	b.WriteString("#EXTM3U\n")
	if isFMP4 {
		b.WriteString("#EXT-X-VERSION:7\n")
	} else {
		b.WriteString("#EXT-X-VERSION:6\n")
	}
	b.WriteString(fmt.Sprintf("#EXT-X-TARGETDURATION:%d\n", target))
	b.WriteString("#EXT-X-MEDIA-SEQUENCE:0\n")
	b.WriteString("#EXT-X-PLAYLIST-TYPE:VOD\n")
	b.WriteString("#EXT-X-INDEPENDENT-SEGMENTS\n")
	if isFMP4 {
		// Dynamic fMP4 segments are generated independently on demand. The MAP
		// URI is relative to this media playlist and works for both default and
		// named variant routes. We repeat it after discontinuities below because
		// some players reset fMP4 track state at each boundary.
		b.WriteString("#EXT-X-MAP:URI=\"segment/init.mp4\"\n")
	}
	for i := 0; i < count; i++ {
		d := segDur
		if end := float64(i+1) * segDur; end > dur && dur > float64(i)*segDur {
			d = dur - float64(i)*segDur
		}
		if d <= 0 {
			d = segDur
		}
		// All dynamic HLS segments are generated as independent seek/transcode
		// windows. Advertise discontinuities so players do not try to force AAC
		// encoder priming or fresh fMP4 fragments into one continuous mux timeline.
		// The EXTINF list still gives the player a correct VOD seek timeline.
		if i > 0 {
			b.WriteString("#EXT-X-DISCONTINUITY\n")
		}
		b.WriteString(fmt.Sprintf("#EXTINF:%.3f,\nsegment/%06d%s\n", d, i, hlsSegmentExt(variant)))
	}
	b.WriteString("#EXT-X-ENDLIST\n")
	return b.String()
}

func hlsUsesFMP4(variant DynamicHLSVariant) bool {
	return strings.EqualFold(strings.TrimSpace(variant.Options.SegmentType), "fmp4")
}

func hlsSegmentExt(variant DynamicHLSVariant) string {
	if hlsUsesFMP4(variant) {
		return ".m4s"
	}
	return ".ts"
}

func hlsSegmentContentType(variant DynamicHLSVariant) string {
	if hlsUsesFMP4(variant) {
		return "video/mp4"
	}
	return "video/mp2t"
}

func hlsVariantDir(sess *DynamicHLSSession, variant DynamicHLSVariant) string {
	name := safeVariantName(variant.Name)
	if name == "" || name == "default" {
		return sess.CacheDir
	}
	return filepath.Join(sess.CacheDir, name)
}

func hlsInitPath(sess *DynamicHLSSession, variant DynamicHLSVariant) string {
	return filepath.Join(hlsVariantDir(sess, variant), "init.mp4")
}

func hlsSegmentPath(sess *DynamicHLSSession, variant DynamicHLSVariant, idx int) string {
	return filepath.Join(hlsVariantDir(sess, variant), fmt.Sprintf("%06d%s", idx, hlsSegmentExt(variant)))
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
		return []DynamicHLSVariant{{Name: "default", Width: width, Height: height, Bandwidth: int(bw), Options: base}}
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
		crop := transcoder.CropRect{}
		if rv.CropAspect != "" {
			if resolved, err := transcoder.CenteredCropForAspect(info, rv.CropAspect); err == nil {
				crop = resolved
				opts.CropWidth = crop.Width
				opts.CropHeight = crop.Height
				opts.CropX = crop.X
				opts.CropY = crop.Y
			}
		}
		if rv.FPS > 0 {
			opts.FPS = rv.FPS
		}
		if rv.CRF > 0 {
			opts.CRF = rv.CRF
		}
		if rv.VideoBitrate > 0 {
			opts.VideoBitrate = int(rv.VideoBitrate)
		}
		if rv.AudioBitrate > 0 {
			opts.AudioBitrate = int(rv.AudioBitrate)
		}
		width := opts.Width
		if width <= 0 {
			width = info.Width
		}
		height := rv.Height
		if crop.Width > 0 && crop.Height > 0 {
			height = transcoder.ScaledHeightForCrop(width, crop, info)
		} else if height <= 0 {
			height = transcoder.ScaledHeightForCrop(width, crop, info)
		}
		bw := rv.VideoBitrate + rv.AudioBitrate
		if bw <= 0 {
			bw = transcoder.Bitrate(opts.VideoBitrate + opts.AudioBitrate)
		}
		if bw <= 0 {
			bw = transcoder.Bitrate(max(1, width) * 2200)
		}
		out = append(out, DynamicHLSVariant{Name: name, Width: width, Height: height, Bandwidth: int(bw), Options: opts})
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
	name = strings.TrimSuffix(name, ".m4s")
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
	id := routeParam(r, "id")
	sess, ok := s.dynHLS.Get(id)
	if !ok {
		writeError(w, http.StatusNotFound, errors.New("hls session not found"))
		return
	}
	variant, ok := findHLSVariant(sess, routeParam(r, "variant"))
	if !ok {
		writeError(w, http.StatusNotFound, errors.New("hls variant not found"))
		return
	}
	s.prewarmHLS(sess, variant, 0, max(1, sess.PrewarmSegments))
	s.logger.Debug("hls variant playlist served", "id", id, "variant", variant.Name, "prewarm", sess.PrewarmSegments)
	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	_, _ = w.Write([]byte(buildDynamicPlaylist(sess, variant)))
}

func (s *Server) dynamicHLSNamedVariantSegment(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	s.dynamicHLSVariantSegment(ctx, w, r, routeParam(r, "variant"))
}

func (s *Server) dynamicHLSVariantSegment(ctx context.Context, w http.ResponseWriter, r *http.Request, variantName string) {
	id := routeParam(r, "id")
	sess, ok := s.dynHLS.Get(id)
	if !ok {
		writeError(w, http.StatusNotFound, errors.New("hls session not found"))
		return
	}
	name := routeParam(r, "name")
	variant, ok := findHLSVariant(sess, variantName)
	if !ok {
		writeError(w, http.StatusNotFound, errors.New("hls variant not found"))
		return
	}
	if hlsUsesFMP4(variant) && name == "init.mp4" {
		initPath := hlsInitPath(sess, variant)
		if err := s.ensureDynamicVariantSegment(ctx, sess, variant, 0, hlsSegmentPath(sess, variant, 0)); err != nil {
			s.logger.Warn("hls fmp4 init failed", "id", id, "variant", variant.Name, "err", err)
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		w.Header().Set("Content-Type", "video/mp4")
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		http.ServeFile(w, r, initPath)
		return
	}
	idx, err := parseSegmentIndex(name)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
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
		s.logger.Warn("hls segment failed", "id", id, "variant", variant.Name, "index", idx, "err", err)
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	s.prewarmHLS(sess, variant, idx+1, sess.PrewarmSegments)
	w.Header().Set("Content-Type", hlsSegmentContentType(variant))
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	http.ServeFile(w, r, path)
}

func (s *Server) prewarmHLS(sess *DynamicHLSSession, variant DynamicHLSVariant, startIdx, count int) {
	if sess == nil || count <= 0 || sess.ctx == nil || sess.ctx.Err() != nil {
		return
	}
	if !sess.prewarmMu.TryLock() {
		return
	}
	if !s.dynHLS.beginBackground() {
		sess.prewarmMu.Unlock()
		return
	}
	go func() {
		defer s.dynHLS.endBackground()
		defer sess.prewarmMu.Unlock()

		segDur := variant.Options.SegmentSeconds
		if segDur <= 0 {
			segDur = 4
		}
		maxCount := int(math.Ceil(sess.Info.Duration / segDur))
		for n := 0; n < count; n++ {
			idx := startIdx + n
			if idx < 0 || idx >= maxCount || sess.ctx.Err() != nil {
				continue
			}
			path := hlsSegmentPath(sess, variant, idx)
			if err := s.ensureDynamicVariantSegment(sess.ctx, sess, variant, idx, path); err != nil && sess.ctx.Err() == nil {
				s.logger.Debug("hls prewarm failed", "id", sess.ID, "variant", variant.Name, "index", idx, "err", err)
			}
		}
	}()
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
		s.logger.Debug("hls segment cache hit", "id", sess.ID, "variant", variant.Name, "index", idx, "path", path, "bytes", st.Size())
		return nil
	}
	lock := s.dynHLS.segmentLock(sess.ID + ":" + variant.Name + ":" + strconv.Itoa(idx))
	lock.Lock()
	defer lock.Unlock()
	if st, err := os.Stat(path); err == nil && st.Size() > 0 {
		s.metrics.segmentCacheHits.Add(1)
		s.logger.Debug("hls segment cache hit after lock", "id", sess.ID, "variant", variant.Name, "index", idx, "path", path, "bytes", st.Size())
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
	// HLS media segments are generated as independent windows and the playlist
	// marks every boundary as a discontinuity. Keep packet timestamps local to
	// each segment; this is significantly more robust for VLC/hls.js seeking than
	// pretending independently encoded AAC/fMP4 windows are one continuous stream.
	opts.TimestampOffset = 0
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
	started := time.Now()
	s.logger.Info("hls segment generate start", "id", sess.ID, "variant", variant.Name, "index", idx, "start_time", opts.StartTime, "duration", opts.Duration, "audio_mode", opts.AudioMode, "output", path)
	if hlsUsesFMP4(variant) {
		fullTmp := tmp + ".full.mp4"
		_ = os.Remove(fullTmp)
		var transcodeErr error
		if opts.HardwareDecode {
			dec, err := sess.getVideoDecoder(opts)
			if err != nil {
				transcodeErr = err
			} else {
				transcodeErr = dec.Transcode(genCtx, fullTmp, opts)
				if transcodeErr == nil {
					sess.scheduleVideoDecoderClose()
				}
			}
		} else {
			_, transcodeErr = transcoder.TranscodeFMP4SegmentFromFile(genCtx, sess.InputPath, fullTmp, opts)
		}
		if transcodeErr != nil {
			if opts.HardwareDecode {
				sess.closeVideoDecoder()
			}
			s.metrics.segmentErrors.Add(1)
			_ = os.Remove(fullTmp)
			_ = os.Remove(tmp)
			return transcodeErr
		}
		if err := splitHLSFMP4(fullTmp, hlsInitPath(sess, variant), tmp); err != nil {
			s.metrics.segmentErrors.Add(1)
			_ = os.Remove(fullTmp)
			_ = os.Remove(tmp)
			return err
		}
		_ = os.Remove(fullTmp)
	} else {
		if _, err := transcoder.TranscodeSegmentFromFile(genCtx, sess.InputPath, tmp, opts); err != nil {
			s.metrics.segmentErrors.Add(1)
			_ = os.Remove(tmp)
			return err
		}
	}
	if err := os.Rename(tmp, path); err != nil {
		s.metrics.segmentErrors.Add(1)
		return err
	}
	if st, statErr := os.Stat(path); statErr == nil {
		s.logger.Info("hls segment generate done", "id", sess.ID, "variant", variant.Name, "index", idx, "elapsed", time.Since(started).String(), "bytes", st.Size(), "path", path)
	} else {
		s.logger.Info("hls segment generate done", "id", sess.ID, "variant", variant.Name, "index", idx, "elapsed", time.Since(started).String(), "path", path)
	}
	s.metrics.segmentsGenerated.Add(1)
	return nil
}

func splitHLSFMP4(fullPath, initPath, mediaPath string) error {
	data, err := os.ReadFile(fullPath)
	if err != nil {
		return err
	}
	moof := findMP4Box(data, "moof")
	if moof <= 0 || moof >= len(data) {
		return fmt.Errorf("generated fMP4 segment does not contain a moof box")
	}
	if err := os.MkdirAll(filepath.Dir(initPath), 0o755); err != nil {
		return err
	}
	if st, err := os.Stat(initPath); err != nil || st.Size() == 0 {
		// Multiple segment prewarm workers for the same variant can race to create
		// the shared fMP4 initialization section. Use a unique temp file instead of
		// init.mp4.tmp so one worker cannot rename another worker's temp file.
		tmpInit := fmt.Sprintf("%s.%d.%d.tmp", initPath, os.Getpid(), time.Now().UnixNano())
		if err := os.WriteFile(tmpInit, data[:moof], 0o644); err != nil {
			return err
		}
		if err := os.Rename(tmpInit, initPath); err != nil {
			_ = os.Remove(tmpInit)
			return err
		}
	}
	return os.WriteFile(mediaPath, data[moof:], 0o644)
}

func findMP4Box(data []byte, typ string) int {
	if len(typ) != 4 {
		return -1
	}
	for off := 0; off+8 <= len(data); {
		sz := int(uint32(data[off])<<24 | uint32(data[off+1])<<16 | uint32(data[off+2])<<8 | uint32(data[off+3]))
		boxType := string(data[off+4 : off+8])
		if boxType == typ {
			return off
		}
		if sz == 1 {
			if off+16 > len(data) {
				return -1
			}
			sz64 := uint64(data[off+8])<<56 | uint64(data[off+9])<<48 | uint64(data[off+10])<<40 | uint64(data[off+11])<<32 | uint64(data[off+12])<<24 | uint64(data[off+13])<<16 | uint64(data[off+14])<<8 | uint64(data[off+15])
			if sz64 > uint64(len(data)-off) || sz64 < 16 {
				return -1
			}
			sz = int(sz64)
		} else if sz == 0 {
			return -1
		} else if sz < 8 || off+sz > len(data) {
			return -1
		}
		off += sz
	}
	return -1
}
