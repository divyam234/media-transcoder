package server

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	transcoder "media-transcoder"
)

type DynamicDASHSessionRequest struct {
	InputPath       string                     `json:"input_path"`
	Options         transcoder.DASHOptions     `json:"options"`
	Variants        []transcoder.LadderVariant `json:"variants,omitempty"`
	CacheDir        string                     `json:"cache_dir,omitempty"`
	PrewarmSegments int                        `json:"prewarm_segments,omitempty"`
}

type DynamicDASHSessionResponse struct {
	ID           string               `json:"id"`
	ManifestURL  string               `json:"manifest_url"`
	Duration     float64              `json:"duration"`
	SegmentTime  float64              `json:"segment_time"`
	SegmentCount int                  `json:"segment_count"`
	Info         transcoder.MediaInfo `json:"info"`
	Variants     []DynamicDASHVariant `json:"variants,omitempty"`
}

type DynamicDASHVariant struct {
	Name      string                     `json:"name"`
	Width     int                        `json:"width,omitempty"`
	Height    int                        `json:"height,omitempty"`
	Bandwidth int                        `json:"bandwidth,omitempty"`
	Codec     transcoder.CodecDescriptor `json:"codec,omitempty"`
	Options   transcoder.DASHOptions     `json:"-"`
}

type DynamicDASHSession struct {
	ID              string
	InputPath       string
	Options         transcoder.DASHOptions
	AudioOptions    transcoder.TranscodeOptions
	AudioCodec      transcoder.CodecDescriptor
	Variants        []DynamicDASHVariant
	CacheDir        string
	PrewarmSegments int
	Info            transcoder.MediaInfo
	CreatedAt       time.Time
	SourceKey       string
	InputCleanup    func()
	codecMu         sync.Mutex
	ctx             context.Context
	cancel          context.CancelFunc
}

type DynamicDASHManager struct {
	mu       sync.RWMutex
	sessions map[string]*DynamicDASHSession
	locks    map[string]*sync.Mutex
	sem      chan struct{}
	wg       sync.WaitGroup
	closed   bool
}

func NewDynamicDASHManager(maxConcurrent int) *DynamicDASHManager {
	if maxConcurrent <= 0 {
		maxConcurrent = 2
	}
	return &DynamicDASHManager{sessions: map[string]*DynamicDASHSession{}, locks: map[string]*sync.Mutex{}, sem: make(chan struct{}, maxConcurrent)}
}

func (m *DynamicDASHManager) Add(s *DynamicDASHSession) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sessions[s.ID] = s
}

func (m *DynamicDASHManager) ReplaceSourceSession(s *DynamicDASHSession) []*DynamicDASHSession {
	m.mu.Lock()
	defer m.mu.Unlock()
	var stale []*DynamicDASHSession
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

func (m *DynamicDASHManager) beginBackground() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return false
	}
	m.wg.Add(1)
	return true
}

func (m *DynamicDASHManager) endBackground() { m.wg.Done() }

func (m *DynamicDASHManager) Wait() { m.wg.Wait() }

func (m *DynamicDASHManager) StopAll() []*DynamicDASHSession {
	m.mu.Lock()
	m.closed = true
	defer m.mu.Unlock()
	out := make([]*DynamicDASHSession, 0, len(m.sessions))
	for id, sess := range m.sessions {
		out = append(out, sess)
		delete(m.sessions, id)
	}
	m.locks = map[string]*sync.Mutex{}
	return out
}

func (m *DynamicDASHManager) Get(id string) (*DynamicDASHSession, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	sess, ok := m.sessions[id]
	return sess, ok
}
func (m *DynamicDASHManager) Delete(id string) (*DynamicDASHSession, bool) {
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

func (m *DynamicDASHManager) segmentLock(key string) *sync.Mutex {
	m.mu.Lock()
	defer m.mu.Unlock()
	l := m.locks[key]
	if l == nil {
		l = &sync.Mutex{}
		m.locks[key] = l
	}
	return l
}

func (s *Server) createDynamicDASHSession(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	var req DynamicDASHSessionRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.InputPath == "" {
		writeError(w, http.StatusBadRequest, errors.New("input_path is required"))
		return
	}
	requestedAudioMode := req.Options.AudioMode
	req.Options.ApplyDefaults()
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
	audioOpts := req.Options.TranscodeOptions
	audioOpts.AudioMode = requestedAudioMode
	if info.HasAudio {
		if audioOpts.AudioMode == "" {
			audioOpts.AudioMode = transcoder.AudioTranscode
		}
	} else {
		audioOpts.AudioMode = transcoder.AudioSkip
	}
	req.Options.AudioMode = transcoder.AudioSkip
	id := newID()
	cacheRoot := s.cacheRootFor(req.CacheDir, "media-transcoder-dash")
	cacheDir := filepath.Join(cacheRoot, id)
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	sessCtx, cancel := context.WithCancel(context.Background())
	sess := &DynamicDASHSession{ID: id, InputPath: req.InputPath, Options: req.Options, AudioOptions: audioOpts, CacheDir: cacheDir, PrewarmSegments: req.PrewarmSegments, Info: info, CreatedAt: time.Now(), ctx: sessCtx, cancel: cancel}
	sess.Variants = buildDASHVariants(req.Options, req.Variants, info)
	s.dynDASH.Add(sess)
	s.metrics.dashSessions.Add(1)
	writeJSON(w, http.StatusCreated, s.dynamicDASHResponse(sess))
}

func dashVariantDir(sess *DynamicDASHSession, variant DynamicDASHVariant) string {
	name := safeVariantName(variant.Name)
	if name == "" || name == "default" {
		return sess.CacheDir
	}
	return filepath.Join(sess.CacheDir, name)
}

func dashInitPathFor(sess *DynamicDASHSession, variant DynamicDASHVariant) string {
	return filepath.Join(dashVariantDir(sess, variant), "init.mp4")
}

func dashSegmentPathFor(sess *DynamicDASHSession, variant DynamicDASHVariant, idx int) string {
	return filepath.Join(dashVariantDir(sess, variant), fmt.Sprintf("%06d.m4s", idx))
}

func dashAudioDir(sess *DynamicDASHSession) string { return filepath.Join(sess.CacheDir, "audio") }
func dashAudioInitPath(sess *DynamicDASHSession) string {
	return filepath.Join(dashAudioDir(sess), "init.mp4")
}
func dashAudioSegmentPath(sess *DynamicDASHSession, idx int) string {
	return filepath.Join(dashAudioDir(sess), fmt.Sprintf("%06d.m4s", idx))
}

func dashInitPath(sess *DynamicDASHSession) string {
	return dashInitPathFor(sess, defaultDASHVariant(sess))
}
func dashSegmentPath(sess *DynamicDASHSession, idx int) string {
	return dashSegmentPathFor(sess, defaultDASHVariant(sess), idx)
}

func (s *Server) dynamicDASHResponse(sess *DynamicDASHSession) DynamicDASHSessionResponse {
	segs := int(math.Ceil(sess.Info.Duration / sess.Options.SegmentSeconds))
	base := "/v1/playback/dash/" + sess.ID
	return DynamicDASHSessionResponse{ID: sess.ID, ManifestURL: base + "/manifest.mpd", Duration: sess.Info.Duration, SegmentTime: sess.Options.SegmentSeconds, SegmentCount: segs, Info: sess.Info, Variants: sess.Variants}
}

func (s *Server) deleteDynamicDASHSession(_ context.Context, w http.ResponseWriter, r *http.Request) {
	id := routeParam(r, "id")
	sess, ok := s.dynDASH.Delete(id)
	if !ok {
		writeError(w, http.StatusNotFound, errors.New("dash session not found"))
		return
	}
	if sess.cancel != nil {
		sess.cancel()
	}
	_ = os.RemoveAll(sess.CacheDir)
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "deleted"})
}

func (s *Server) dynamicDASHManifest(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	id := routeParam(r, "id")
	sess, ok := s.dynDASH.Get(id)
	if !ok {
		writeError(w, http.StatusNotFound, errors.New("dash session not found"))
		return
	}
	if err := s.ensureDASHCodecDescriptors(ctx, sess); err != nil {
		s.logger.Warn("dash codec probe failed", "id", id, "err", err)
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	prewarm := sess.PrewarmSegments
	if prewarm <= 0 {
		prewarm = 3
	}
	s.prewarmDASH(sess, 0, prewarm)
	s.logger.Debug("dash manifest served", "id", id, "prewarm", prewarm)
	s.writeDASHManifest(w, sess)
}

func (s *Server) ensureDASHCodecDescriptors(ctx context.Context, sess *DynamicDASHSession) error {
	sess.codecMu.Lock()
	defer sess.codecMu.Unlock()
	for i := range sess.Variants {
		if sess.Variants[i].Codec.CodecString != "" {
			continue
		}
		variant := sess.Variants[i]
		if err := s.ensureDynamicDASHVariantSegment(ctx, sess, variant, 0, dashSegmentPathFor(sess, variant, 0)); err != nil {
			return err
		}
		codec, err := transcoder.ProbeCodec(dashInitPathFor(sess, variant), false)
		if err != nil {
			return err
		}
		sess.Variants[i].Codec = codec
	}
	if sess.Info.HasAudio && sess.AudioOptions.AudioMode != transcoder.AudioSkip && sess.AudioCodec.CodecString == "" {
		if err := s.ensureDynamicDASHAudioSegment(ctx, sess, 0); err != nil {
			return err
		}
		codec, err := transcoder.ProbeCodec(dashAudioInitPath(sess), true)
		if err != nil {
			return err
		}
		sess.AudioCodec = codec
	}
	return nil
}

func (s *Server) writeDASHManifest(w http.ResponseWriter, sess *DynamicDASHSession) {
	w.Header().Set("Content-Type", "application/dash+xml")
	_, _ = w.Write([]byte(buildDynamicDASHMPD(sess)))
}

func dashSegmentWindow(duration, segmentSeconds float64, idx int) (start, length float64, ok bool) {
	if segmentSeconds <= 0 {
		segmentSeconds = 4
	}
	if idx < 0 || duration <= 0 {
		return 0, 0, false
	}
	start = float64(idx) * segmentSeconds
	if start >= duration {
		return 0, 0, false
	}
	length = math.Min(segmentSeconds, duration-start)
	return start, length, length > 0
}

func writeDASHSegmentTimeline(b *strings.Builder, duration, segmentSeconds float64) {
	if segmentSeconds <= 0 {
		segmentSeconds = 4
	}
	timescale := 1000
	full := int(math.Floor(duration / segmentSeconds))
	last := duration - float64(full)*segmentSeconds
	b.WriteString(`          <SegmentTimeline>` + "\n")
	if full > 0 {
		repeat := full - 1
		b.WriteString(fmt.Sprintf(`            <S t="0" d="%d" r="%d"/>`+"\n", int(math.Round(segmentSeconds*float64(timescale))), repeat))
	}
	if last > 0.0005 {
		b.WriteString(fmt.Sprintf(`            <S d="%d"/>`+"\n", int(math.Round(last*float64(timescale)))))
	}
	b.WriteString("          </SegmentTimeline>\n")
}

func buildDynamicDASHMPD(sess *DynamicDASHSession) string {
	dur := sess.Info.Duration
	segDur := sess.Options.SegmentSeconds
	if segDur <= 0 {
		segDur = 4
	}
	vars := sess.Variants
	if len(vars) == 0 {
		vars = buildDASHVariants(sess.Options, nil, sess.Info)
	}
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	b.WriteString(fmt.Sprintf(`<MPD xmlns="urn:mpeg:dash:schema:mpd:2011" type="static" mediaPresentationDuration="PT%.3fS" minBufferTime="PT%.3fS" profiles="urn:mpeg:dash:profile:isoff-on-demand:2011">`+"\n", dur, segDur))
	b.WriteString(fmt.Sprintf(`  <Period id="0" start="PT0S" duration="PT%.3fS">`+"\n", dur))
	b.WriteString(`    <AdaptationSet id="0" mimeType="video/mp4" segmentAlignment="true" subsegmentAlignment="true" startWithSAP="1">` + "\n")
	for _, v := range vars {
		fps := v.Options.FPS
		if fps <= 0 {
			fps = sess.Info.FPS
		}
		bandwidth := v.Bandwidth
		if bandwidth <= 0 {
			bandwidth = max(1, v.Width) * 2200
		}
		codecs := v.Codec.CodecString
		if codecs == "" {
			codecs = dashVariantCodecs(sess, v)
		}
		prefix := "segment/"
		if v.Name != "default" && v.Name != "" {
			prefix = "variant/" + v.Name + "/segment/"
		}
		if fps > 0 {
			b.WriteString(fmt.Sprintf(`      <Representation id="%s" bandwidth="%d" width="%d" height="%d" frameRate="%.3f" codecs="%s">`+"\n", v.Name, bandwidth, v.Width, v.Height, fps, codecs))
		} else {
			b.WriteString(fmt.Sprintf(`      <Representation id="%s" bandwidth="%d" width="%d" height="%d" codecs="%s">`+"\n", v.Name, bandwidth, v.Width, v.Height, codecs))
		}
		b.WriteString(fmt.Sprintf(`        <SegmentTemplate timescale="1000" startNumber="0" initialization="%sinit.mp4" media="%s$Number%%06d$.m4s">`+"\n", prefix, prefix))
		writeDASHSegmentTimeline(&b, dur, segDur)
		b.WriteString("        </SegmentTemplate>\n")
		b.WriteString("      </Representation>\n")
	}
	b.WriteString("    </AdaptationSet>\n")
	if sess.Info.HasAudio && sess.AudioOptions.AudioMode != transcoder.AudioSkip {
		bandwidth := sess.AudioOptions.AudioBitrate
		if bandwidth <= 0 {
			bandwidth = 128000
		}
		b.WriteString(`    <AdaptationSet id="1" mimeType="audio/mp4" lang="und" segmentAlignment="true" startWithSAP="1">` + "\n")
		audioCodec := sess.AudioCodec.CodecString
		if audioCodec == "" {
			audioCodec = "mp4a.40.2"
		}
		sampleRate := sess.AudioCodec.SampleRate
		if sampleRate <= 0 {
			sampleRate = 48000
		}
		channels := sess.AudioCodec.Channels
		if channels <= 0 {
			channels = 2
		}
		b.WriteString(fmt.Sprintf(`      <Representation id="audio" bandwidth="%d" audioSamplingRate="%d" codecs="%s">`+"\n", bandwidth, sampleRate, audioCodec))
		b.WriteString(fmt.Sprintf(`        <AudioChannelConfiguration schemeIdUri="urn:mpeg:dash:23003:3:audio_channel_configuration:2011" value="%d"/>`+"\n", channels))
		b.WriteString(`        <SegmentTemplate timescale="1000" startNumber="0" initialization="audio/segment/init.mp4" media="audio/segment/$Number%06d$.m4s">` + "\n")
		writeDASHSegmentTimeline(&b, dur, segDur)
		b.WriteString("        </SegmentTemplate>\n")
		b.WriteString("      </Representation>\n")
		b.WriteString("    </AdaptationSet>\n")
	}
	b.WriteString("  </Period>\n")
	b.WriteString("</MPD>\n")
	return b.String()
}

func dashVariantCodecs(_ *DynamicDASHSession, variant DynamicDASHVariant) string {
	enc := strings.ToLower(strings.TrimSpace(variant.Options.EncoderName))
	switch {
	case strings.Contains(enc, "265"), strings.Contains(enc, "hevc"), strings.Contains(enc, "x265"):
		return "hvc1.1.6.L93.B0"
	case strings.Contains(enc, "av1"):
		return "av01.0.08M.08"
	default:
		return "avc1.64001f"
	}
}

func buildDASHVariants(base transcoder.DASHOptions, requested []transcoder.LadderVariant, info transcoder.MediaInfo) []DynamicDASHVariant {
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
		return []DynamicDASHVariant{{Name: "default", Width: width, Height: height, Bandwidth: int(bw), Options: base}}
	}
	out := make([]DynamicDASHVariant, 0, len(requested))
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
		if rv.Height > 0 {
			opts.Height = rv.Height
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
		height := opts.Height
		if height <= 0 {
			height = scaledHeight(width, info)
		}
		bw := rv.VideoBitrate + rv.AudioBitrate
		if bw <= 0 {
			bw = transcoder.Bitrate(opts.VideoBitrate + opts.AudioBitrate)
		}
		if bw <= 0 {
			bw = transcoder.Bitrate(max(1, width) * 2200)
		}
		out = append(out, DynamicDASHVariant{Name: name, Width: width, Height: height, Bandwidth: int(bw), Options: opts})
	}
	return out
}

func defaultDASHVariant(sess *DynamicDASHSession) DynamicDASHVariant {
	if len(sess.Variants) > 0 {
		return sess.Variants[0]
	}
	return buildDASHVariants(sess.Options, nil, sess.Info)[0]
}

func findDASHVariant(sess *DynamicDASHSession, name string) (DynamicDASHVariant, bool) {
	if name == "" || name == "default" {
		return defaultDASHVariant(sess), true
	}
	name = safeVariantName(name)
	for _, v := range sess.Variants {
		if v.Name == name {
			return v, true
		}
	}
	return DynamicDASHVariant{}, false
}

func (s *Server) dynamicDASHSegment(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	s.dynamicDASHVariantSegment(ctx, w, r, "default")
}

func (s *Server) dynamicDASHNamedVariantSegment(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	s.dynamicDASHVariantSegment(ctx, w, r, routeParam(r, "variant"))
}

func (s *Server) dynamicDASHVariantSegment(ctx context.Context, w http.ResponseWriter, r *http.Request, variantName string) {
	id := routeParam(r, "id")
	sess, ok := s.dynDASH.Get(id)
	if !ok {
		writeError(w, http.StatusNotFound, errors.New("dash session not found"))
		return
	}
	variant, ok := findDASHVariant(sess, variantName)
	if !ok {
		writeError(w, http.StatusNotFound, errors.New("dash variant not found"))
		return
	}
	name := routeParam(r, "name")
	if name == "init.mp4" {
		initPath := dashInitPathFor(sess, variant)
		if err := s.ensureDynamicDASHVariantSegment(ctx, sess, variant, 0, dashSegmentPathFor(sess, variant, 0)); err != nil {
			s.logger.Warn("dash init failed", "id", id, "variant", variant.Name, "err", err)
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		w.Header().Set("Content-Type", "video/mp4")
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		http.ServeFile(w, r, initPath)
		return
	}
	idx, err := parseDashSegmentIndex(name)
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
	path := dashSegmentPathFor(sess, variant, idx)
	s.prewarmDASHVariant(sess, variant, idx+1, sess.PrewarmSegments)
	if err := s.ensureDynamicDASHVariantSegment(ctx, sess, variant, idx, path); err != nil {
		s.logger.Warn("dash segment failed", "id", id, "variant", variant.Name, "index", idx, "err", err)
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	w.Header().Set("Content-Type", "video/mp4")
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	http.ServeFile(w, r, path)
}

func (s *Server) dynamicDASHAudioSegment(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	id := routeParam(r, "id")
	sess, ok := s.dynDASH.Get(id)
	if !ok {
		writeError(w, http.StatusNotFound, errors.New("dash session not found"))
		return
	}
	if !sess.Info.HasAudio || sess.AudioOptions.AudioMode == transcoder.AudioSkip {
		writeError(w, http.StatusNotFound, errors.New("dash audio not available"))
		return
	}
	name := routeParam(r, "name")
	if name == "init.mp4" {
		if err := s.ensureDynamicDASHAudioSegment(ctx, sess, 0); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		w.Header().Set("Content-Type", "audio/mp4")
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		http.ServeFile(w, r, dashAudioInitPath(sess))
		return
	}
	idx, err := parseDashSegmentIndex(name)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	count := int(math.Ceil(sess.Info.Duration / sess.Options.SegmentSeconds))
	if idx < 0 || idx >= count {
		writeError(w, http.StatusRequestedRangeNotSatisfiable, errors.New("segment index outside media duration"))
		return
	}
	if err := s.ensureDynamicDASHAudioSegment(ctx, sess, idx); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	w.Header().Set("Content-Type", "audio/mp4")
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	http.ServeFile(w, r, dashAudioSegmentPath(sess, idx))
}

func parseDashSegmentIndex(name string) (int, error) {
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

func (s *Server) prewarmDASH(sess *DynamicDASHSession, startIdx, count int) {
	s.prewarmDASHVariant(sess, defaultDASHVariant(sess), startIdx, count)
}

func (s *Server) prewarmDASHVariant(sess *DynamicDASHSession, variant DynamicDASHVariant, startIdx, count int) {
	if sess == nil || count <= 0 || sess.ctx == nil || sess.ctx.Err() != nil {
		return
	}
	segDur := variant.Options.SegmentSeconds
	if segDur <= 0 {
		segDur = 4
	}
	maxCount := int(math.Ceil(sess.Info.Duration / segDur))
	for n := 0; n < count; n++ {
		idx := startIdx + n
		if idx < 0 || idx >= maxCount {
			continue
		}
		path := dashSegmentPathFor(sess, variant, idx)
		if !s.dynDASH.beginBackground() {
			return
		}
		go func(i int, p string) {
			defer s.dynDASH.endBackground()
			if err := s.ensureDynamicDASHVariantSegment(sess.ctx, sess, variant, i, p); err != nil && sess.ctx.Err() == nil {
				s.logger.Debug("dash prewarm failed", "id", sess.ID, "variant", variant.Name, "index", i, "err", err)
			}
		}(idx, path)
	}
}

func (s *Server) ensureDynamicDASHSegment(ctx context.Context, sess *DynamicDASHSession, idx int, path string) error {
	return s.ensureDynamicDASHVariantSegment(ctx, sess, defaultDASHVariant(sess), idx, path)
}

func (s *Server) ensureDynamicDASHAudioSegment(ctx context.Context, sess *DynamicDASHSession, idx int) error {
	path := dashAudioSegmentPath(sess, idx)
	if validateFMP4Media(path) && validateFMP4Init(dashAudioInitPath(sess)) {
		s.metrics.segmentCacheHits.Add(1)
		return nil
	}
	_ = os.Remove(path)
	lock := s.dynDASH.segmentLock(sess.ID + ":audio:" + strconv.Itoa(idx))
	lock.Lock()
	defer lock.Unlock()
	if validateFMP4Media(path) && validateFMP4Init(dashAudioInitPath(sess)) {
		s.metrics.segmentCacheHits.Add(1)
		return nil
	}
	_ = os.Remove(path)
	select {
	case s.dynDASH.sem <- struct{}{}:
		defer func() { <-s.dynDASH.sem }()
	case <-ctx.Done():
		return ctx.Err()
	case <-sess.ctx.Done():
		return sess.ctx.Err()
	}
	if err := os.MkdirAll(dashAudioDir(sess), 0o755); err != nil {
		s.metrics.segmentErrors.Add(1)
		s.metrics.dashAudioErrors.Add(1)
		return err
	}
	tmp := path + ".tmp"
	full := tmp + ".full.mp4"
	_ = os.Remove(tmp)
	_ = os.Remove(full)
	opts := sess.AudioOptions
	start, length, ok := dashSegmentWindow(sess.Info.Duration, sess.Options.SegmentSeconds, idx)
	if !ok {
		return errors.New("segment index outside media duration")
	}
	opts.StartTime = start
	opts.Duration = length
	opts.TimestampOffset = opts.StartTime
	if _, err := transcoder.TranscodeAudioFMP4SegmentFromFile(ctx, sess.InputPath, full, opts); err != nil {
		_ = os.Remove(full)
		_ = os.Remove(tmp)
		s.metrics.segmentErrors.Add(1)
		s.metrics.dashAudioErrors.Add(1)
		return err
	}
	if err := splitHLSFMP4(full, dashAudioInitPath(sess), tmp); err != nil {
		_ = os.Remove(full)
		_ = os.Remove(tmp)
		s.metrics.segmentErrors.Add(1)
		s.metrics.dashAudioErrors.Add(1)
		return err
	}
	if err := patchFMP4DecodeTime(dashAudioInitPath(sess), tmp, opts.StartTime); err != nil {
		_ = os.Remove(full)
		_ = os.Remove(tmp)
		s.metrics.segmentErrors.Add(1)
		s.metrics.dashAudioErrors.Add(1)
		return err
	}
	initData, err := os.ReadFile(dashAudioInitPath(sess))
	if err != nil {
		_ = os.Remove(full)
		_ = os.Remove(tmp)
		s.metrics.segmentErrors.Add(1)
		s.metrics.dashAudioErrors.Add(1)
		return err
	}
	timescale, err := mp4TrackTimescale(initData)
	if err != nil {
		_ = os.Remove(full)
		_ = os.Remove(tmp)
		s.metrics.segmentErrors.Add(1)
		s.metrics.dashAudioErrors.Add(1)
		return err
	}
	if err := patchFMP4SampleDuration(tmp, uint64(math.Round(opts.Duration*float64(timescale)))); err != nil {
		_ = os.Remove(full)
		_ = os.Remove(tmp)
		s.metrics.segmentErrors.Add(1)
		s.metrics.dashAudioErrors.Add(1)
		return err
	}
	_ = os.Remove(full)
	if err := os.Rename(tmp, path); err != nil {
		s.metrics.segmentErrors.Add(1)
		s.metrics.dashAudioErrors.Add(1)
		return err
	}
	s.metrics.segmentsGenerated.Add(1)
	s.metrics.dashAudioGenerated.Add(1)
	return nil
}

func (s *Server) ensureDynamicDASHVariantSegment(ctx context.Context, sess *DynamicDASHSession, variant DynamicDASHVariant, idx int, path string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if sess.ctx != nil {
		if err := sess.ctx.Err(); err != nil {
			return err
		}
	}
	if validateFMP4Media(path) && validateFMP4Init(dashInitPathFor(sess, variant)) {
		s.metrics.segmentCacheHits.Add(1)
		return nil
	}
	_ = os.Remove(path)
	lock := s.dynDASH.segmentLock(sess.ID + ":" + variant.Name + ":" + strconv.Itoa(idx))
	lock.Lock()
	defer lock.Unlock()
	if validateFMP4Media(path) && validateFMP4Init(dashInitPathFor(sess, variant)) {
		s.metrics.segmentCacheHits.Add(1)
		return nil
	}
	_ = os.Remove(path)
	select {
	case s.dynDASH.sem <- struct{}{}:
		defer func() { <-s.dynDASH.sem }()
	case <-ctx.Done():
		return ctx.Err()
	case <-sess.ctx.Done():
		return sess.ctx.Err()
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	fullTmp := tmp + ".full.mp4"
	_ = os.Remove(tmp)
	_ = os.Remove(fullTmp)
	opts := variant.Options.TranscodeOptions
	start, length, ok := dashSegmentWindow(sess.Info.Duration, variant.Options.SegmentSeconds, idx)
	if !ok {
		return errors.New("segment index outside media duration")
	}
	opts.StartTime = start
	opts.Duration = length
	opts.TimestampOffset = opts.StartTime
	opts.FastStart = false
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
	s.logger.Info("dash segment generate start", "id", sess.ID, "variant", variant.Name, "index", idx, "start_time", opts.StartTime, "duration", opts.Duration, "audio_mode", opts.AudioMode, "output", path)
	if _, err := transcoder.TranscodeFMP4SegmentFromFile(genCtx, sess.InputPath, fullTmp, opts); err != nil {
		s.metrics.segmentErrors.Add(1)
		s.metrics.dashVideoErrors.Add(1)
		_ = os.Remove(fullTmp)
		_ = os.Remove(tmp)
		return err
	}
	if err := splitHLSFMP4(fullTmp, dashInitPathFor(sess, variant), tmp); err != nil {
		s.metrics.segmentErrors.Add(1)
		s.metrics.dashVideoErrors.Add(1)
		_ = os.Remove(fullTmp)
		_ = os.Remove(tmp)
		return err
	}
	if err := patchFMP4DecodeTime(dashInitPathFor(sess, variant), tmp, opts.StartTime); err != nil {
		s.metrics.segmentErrors.Add(1)
		s.metrics.dashVideoErrors.Add(1)
		_ = os.Remove(fullTmp)
		_ = os.Remove(tmp)
		return err
	}
	_ = os.Remove(fullTmp)
	if err := os.Rename(tmp, path); err != nil {
		s.metrics.segmentErrors.Add(1)
		s.metrics.dashVideoErrors.Add(1)
		return err
	}
	if st, statErr := os.Stat(path); statErr == nil {
		s.logger.Info("dash segment generate done", "id", sess.ID, "variant", variant.Name, "index", idx, "elapsed", time.Since(started).String(), "bytes", st.Size(), "path", path)
	}
	s.metrics.segmentsGenerated.Add(1)
	s.metrics.dashVideoGenerated.Add(1)
	return nil
}
