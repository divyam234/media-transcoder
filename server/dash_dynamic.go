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
	InputPath       string                 `json:"input_path"`
	Options         transcoder.DASHOptions `json:"options"`
	CacheDir        string                 `json:"cache_dir,omitempty"`
	PrewarmSegments int                    `json:"prewarm_segments,omitempty"`
}

type DynamicDASHSessionResponse struct {
	ID           string               `json:"id"`
	ManifestURL  string               `json:"manifest_url"`
	Duration     float64              `json:"duration"`
	SegmentTime  float64              `json:"segment_time"`
	SegmentCount int                  `json:"segment_count"`
	Info         transcoder.MediaInfo `json:"info"`
}

type DynamicDASHSession struct {
	ID              string
	InputPath       string
	Options         transcoder.DASHOptions
	CacheDir        string
	PrewarmSegments int
	Info            transcoder.MediaInfo
	CreatedAt       time.Time
	ctx             context.Context
	cancel          context.CancelFunc
}

type DynamicDASHManager struct {
	mu       sync.RWMutex
	sessions map[string]*DynamicDASHSession
	locks    map[string]*sync.Mutex
	sem      chan struct{}
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
	if requestedAudioMode == "" {
		if info.HasAudio {
			req.Options.AudioMode = transcoder.AudioTranscode
		} else {
			req.Options.AudioMode = transcoder.AudioSkip
		}
	}
	id := newID()
	cacheRoot := s.cacheRootFor(req.CacheDir, "go-media-transcoder-dash")
	cacheDir := filepath.Join(cacheRoot, id)
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	sessCtx, cancel := context.WithCancel(context.Background())
	sess := &DynamicDASHSession{ID: id, InputPath: req.InputPath, Options: req.Options, CacheDir: cacheDir, PrewarmSegments: req.PrewarmSegments, Info: info, CreatedAt: time.Now(), ctx: sessCtx, cancel: cancel}
	s.dynDASH.Add(sess)
	s.metrics.dashSessions.Add(1)
	writeJSON(w, http.StatusCreated, s.dynamicDASHResponse(sess))
}

func (s *Server) dynamicDASHResponse(sess *DynamicDASHSession) DynamicDASHSessionResponse {
	segs := int(math.Ceil(sess.Info.Duration / sess.Options.SegmentSeconds))
	base := "/v1/playback/dash/" + sess.ID
	return DynamicDASHSessionResponse{ID: sess.ID, ManifestURL: base + "/manifest.mpd", Duration: sess.Info.Duration, SegmentTime: sess.Options.SegmentSeconds, SegmentCount: segs, Info: sess.Info}
}

func (s *Server) deleteDynamicDASHSession(_ context.Context, w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
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

func (s *Server) dynamicDASHManifest(_ context.Context, w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	sess, ok := s.dynDASH.Get(id)
	if !ok {
		writeError(w, http.StatusNotFound, errors.New("dash session not found"))
		return
	}
	w.Header().Set("Content-Type", "application/dash+xml")
	_, _ = w.Write([]byte(buildDynamicDASHMPD(sess)))
}

func buildDynamicDASHMPD(sess *DynamicDASHSession) string {
	dur := sess.Info.Duration
	segDur := sess.Options.SegmentSeconds
	if segDur <= 0 {
		segDur = 4
	}
	count := int(math.Ceil(dur / segDur))
	if count < 1 {
		count = 1
	}
	width := sess.Options.Width
	if width <= 0 {
		width = sess.Info.Width
	}
	height := sess.Info.Height
	if sess.Info.Width > 0 && sess.Info.Height > 0 && width > 0 {
		height = int(math.Round(float64(width) * float64(sess.Info.Height) / float64(sess.Info.Width)))
	}
	fps := sess.Options.FPS
	if fps <= 0 {
		fps = sess.Info.FPS
	}
	bandwidth := sess.Options.VideoBitrate
	if bandwidth <= 0 {
		bandwidth = width * 2200
	}
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	b.WriteString(fmt.Sprintf(`<MPD xmlns="urn:mpeg:dash:schema:mpd:2011" type="static" mediaPresentationDuration="PT%.3fS" minBufferTime="PT%.3fS" profiles="urn:mpeg:dash:profile:isoff-on-demand:2011">`+"\n", dur, segDur))
	b.WriteString(fmt.Sprintf(`  <Period id="0" start="PT0S" duration="PT%.3fS">`+"\n", dur))
	b.WriteString(`    <AdaptationSet id="0" contentType="video" mimeType="video/mp4" segmentAlignment="true" startWithSAP="1">` + "\n")
	if fps > 0 {
		b.WriteString(fmt.Sprintf(`      <Representation id="v0" bandwidth="%d" width="%d" height="%d" frameRate="%.3f">`+"\n", bandwidth, width, height, fps))
	} else {
		b.WriteString(fmt.Sprintf(`      <Representation id="v0" bandwidth="%d" width="%d" height="%d">`+"\n", bandwidth, width, height))
	}
	b.WriteString(fmt.Sprintf(`        <SegmentList timescale="1000" duration="%d">`+"\n", int(math.Round(segDur*1000))))
	for i := 0; i < count; i++ {
		b.WriteString(fmt.Sprintf(`          <SegmentURL media="segment/%06d.m4s"/>`+"\n", i))
	}
	b.WriteString("        </SegmentList>\n")
	b.WriteString("      </Representation>\n")
	b.WriteString("    </AdaptationSet>\n")
	b.WriteString("  </Period>\n")
	b.WriteString("</MPD>\n")
	return b.String()
}

func (s *Server) dynamicDASHSegment(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	sess, ok := s.dynDASH.Get(id)
	if !ok {
		writeError(w, http.StatusNotFound, errors.New("dash session not found"))
		return
	}
	idx, err := parseDashSegmentIndex(r.PathValue("name"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	segDur := sess.Options.SegmentSeconds
	count := int(math.Ceil(sess.Info.Duration / segDur))
	if idx < 0 || idx >= count {
		writeError(w, http.StatusRequestedRangeNotSatisfiable, errors.New("segment index outside media duration"))
		return
	}
	path := filepath.Join(sess.CacheDir, fmt.Sprintf("%06d.m4s", idx))
	if err := s.ensureDynamicDASHSegment(ctx, sess, idx, path); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	for n := 1; n <= sess.PrewarmSegments; n++ {
		next := idx + n
		if next < count {
			go func(i int) {
				_ = s.ensureDynamicDASHSegment(sess.ctx, sess, i, filepath.Join(sess.CacheDir, fmt.Sprintf("%06d.m4s", i)))
			}(next)
		}
	}
	w.Header().Set("Content-Type", "video/mp4")
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	http.ServeFile(w, r, path)
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

func (s *Server) ensureDynamicDASHSegment(ctx context.Context, sess *DynamicDASHSession, idx int, path string) error {
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
	lock := s.dynDASH.segmentLock(sess.ID + ":" + strconv.Itoa(idx))
	lock.Lock()
	defer lock.Unlock()
	if st, err := os.Stat(path); err == nil && st.Size() > 0 {
		s.metrics.segmentCacheHits.Add(1)
		return nil
	}
	select {
	case s.dynDASH.sem <- struct{}{}:
		defer func() { <-s.dynDASH.sem }()
	case <-ctx.Done():
		return ctx.Err()
	case <-sess.ctx.Done():
		return sess.ctx.Err()
	}
	tmp := path + ".tmp"
	_ = os.Remove(tmp)
	opts := sess.Options.TranscodeOptions
	opts.StartTime = float64(idx) * sess.Options.SegmentSeconds
	opts.Duration = sess.Options.SegmentSeconds
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
	if _, err := transcoder.TranscodeFMP4SegmentFromFile(genCtx, sess.InputPath, tmp, opts); err != nil {
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
