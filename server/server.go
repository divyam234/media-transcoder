// Package server exposes the transcoder API as an embeddable HTTP service.
package server

import (
	"context"
	"crypto/subtle"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	transcoder "media-transcoder"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/go-chi/cors"
)

type Config struct {
	Logger             *slog.Logger
	RequestTimeout     time.Duration
	APIKeys            []string
	RateLimitPerMinute int
	MaxConcurrentJobs  int
	// CacheRoot forces all dynamic playback cache directories under a server-owned root.
	// When set, client-provided cache_dir is ignored.
	CacheRoot string
	// VFSCacheRoot is the dedicated rclone VFS cache directory. Empty uses rclone default.
	VFSCacheRoot string
	// AllowedInputRoots restricts input_path to these roots. Empty means allow any path.
	AllowedInputRoots []string
	CORS              CORSConfig
	ConfigPath        string
	Libraries         map[string]LibraryConfig
	Profiles          map[string]PlaybackProfile
}

// CORSConfig controls browser access to the dynamic playback API.
// Empty values use safe public-playback defaults suitable for local media origins.
type CORSConfig struct {
	AllowedOrigins   []string
	AllowedMethods   []string
	AllowedHeaders   []string
	ExposedHeaders   []string
	AllowCredentials bool
	MaxAge           int
}

type Server struct {
	router            chi.Router
	logger            *slog.Logger
	timeout           time.Duration
	keys              []string
	rate              *rateLimiter
	jobs              *JobManager
	dynHLS            *DynamicHLSManager
	dynDASH           *DynamicDASHManager
	metrics           *Metrics
	cacheRoot         string
	vfsCacheRoot      string
	allowedInputRoots []string
	configPath        string
	libraries         map[string]LibraryConfig
	profiles          map[string]PlaybackProfile
	configMu          sync.RWMutex
	sessionMu         sync.Mutex
	retiredInputs     []func()
	vfsMu             sync.Mutex
	libraryVFS        map[string]*libraryVFS
}

func New(cfg Config) *Server {
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if cfg.RequestTimeout <= 0 {
		cfg.RequestTimeout = 30 * time.Minute
	}
	if cfg.MaxConcurrentJobs <= 0 {
		cfg.MaxConcurrentJobs = 4
	}
	s := &Server{router: chi.NewRouter(), logger: cfg.Logger, timeout: cfg.RequestTimeout, keys: cfg.APIKeys, rate: newRateLimiter(cfg.RateLimitPerMinute), jobs: NewJobManager(cfg.MaxConcurrentJobs), dynHLS: NewDynamicHLSManager(cfg.MaxConcurrentJobs), dynDASH: NewDynamicDASHManager(cfg.MaxConcurrentJobs), metrics: &Metrics{}, cacheRoot: cfg.CacheRoot, vfsCacheRoot: cfg.VFSCacheRoot, allowedInputRoots: cleanRoots(cfg.AllowedInputRoots), configPath: cfg.ConfigPath, libraries: normalizeLibraries(cfg.Libraries), profiles: normalizeProfiles(cfg.Profiles), libraryVFS: map[string]*libraryVFS{}}
	s.routes(cfg.CORS)
	return s
}

func (s *Server) Handler() http.Handler { return s.router }

func (s *Server) Close() {
	hlsSessions := s.dynHLS.StopAll()
	for _, sess := range hlsSessions {
		if sess.cancel != nil {
			sess.cancel()
		}
	}
	dashSessions := s.dynDASH.StopAll()
	for _, sess := range dashSessions {
		if sess.cancel != nil {
			sess.cancel()
		}
	}
	s.dynHLS.Wait()
	s.dynDASH.Wait()
	for _, sess := range hlsSessions {
		if sess.InputCleanup != nil {
			sess.InputCleanup()
		}
	}
	for _, sess := range dashSessions {
		if sess.InputCleanup != nil {
			sess.InputCleanup()
		}
	}
	s.sessionMu.Lock()
	retired := s.retiredInputs
	s.retiredInputs = nil
	s.sessionMu.Unlock()
	for _, cleanup := range retired {
		cleanup()
	}
	s.shutdownLibraryVFS()
}

func (s *Server) routes(corsCfg CORSConfig) {
	r := s.router
	r.Use(middleware.RequestID)
	r.Use(middleware.Recoverer)
	r.Use(cors.Handler(normalizeCORS(corsCfg)))
	r.Get("/openapi.yaml", s.openAPISchema)
	r.Get("/v1/openapi.yaml", s.openAPISchema)

	r.Group(func(r chi.Router) {
		r.Use(s.secure)
		r.Get("/healthz", s.health)
		r.Get("/v1/capabilities", s.withTimeout(s.capabilities))
		r.Get("/v1/capabilities/runtime", s.withTimeout(s.runtimeCapabilities))
		r.Get("/v1/capabilities/codecs", s.withTimeout(s.codecCapabilities))
		r.Get("/v1/capabilities/hardware", s.withTimeout(s.hardwareCapabilities))
		r.Get("/v1/metrics", s.withTimeout(s.metricsHandler))
		r.Post("/v1/probe", s.withTimeout(s.probe))
		r.Post("/v1/plan/device", s.withTimeout(s.devicePlan))
		r.Get("/v1/profiles", s.withTimeout(s.listProfiles))
		r.Get("/v1/profiles/{id}", s.withTimeout(s.getProfile))
		r.Get("/v1/libraries", s.withTimeout(s.listLibraries))
		r.Get("/v1/libraries/{id}", s.withTimeout(s.getLibrary))
		r.Post("/v1/admin/reload", s.withTimeout(s.reloadConfig))
		r.Get("/play/hls/{profile}/{library}/*", s.withTimeout(s.libraryHLS))
		r.Get("/play/dash/{profile}/{library}/*", s.withTimeout(s.libraryDASH))
		// Dynamic playback origin endpoints only. This service intentionally does not
		// expose static "transcode-to-output" routes; playlists/manifests are virtual
		// and media segments are generated on demand by direct libav seeking.
		r.Post("/v1/playback/hls/sessions", s.withTimeout(s.createDynamicHLSSession))
		r.Get("/v1/playback/hls/{id}/master.m3u8", s.withTimeout(s.dynamicHLSMaster))
		r.Get("/v1/playback/hls/{id}/video.m3u8", s.withTimeout(s.dynamicHLSPlaylist))
		r.Get("/v1/playback/hls/{id}/segment/{name}", s.withTimeout(s.dynamicHLSSegment))
		r.Get("/v1/playback/hls/{id}/variant/{variant}/video.m3u8", s.withTimeout(s.dynamicHLSVariantPlaylist))
		r.Get("/v1/playback/hls/{id}/variant/{variant}/segment/{name}", s.withTimeout(s.dynamicHLSNamedVariantSegment))
		r.Delete("/v1/playback/hls/{id}", s.withTimeout(s.deleteDynamicHLSSession))

		r.Post("/v1/playback/dash/sessions", s.withTimeout(s.createDynamicDASHSession))
		r.Get("/v1/playback/dash/{id}/manifest.mpd", s.withTimeout(s.dynamicDASHManifest))
		r.Get("/v1/playback/dash/{id}/segment/{name}", s.withTimeout(s.dynamicDASHSegment))
		r.Get("/v1/playback/dash/{id}/audio/segment/{name}", s.withTimeout(s.dynamicDASHAudioSegment))
		r.Get("/v1/playback/dash/{id}/variant/{variant}/segment/{name}", s.withTimeout(s.dynamicDASHNamedVariantSegment))
		r.Delete("/v1/playback/dash/{id}", s.withTimeout(s.deleteDynamicDASHSession))
	})
}

func normalizeCORS(cfg CORSConfig) cors.Options {
	if len(cfg.AllowedOrigins) == 0 {
		cfg.AllowedOrigins = []string{"*"}
	}
	if len(cfg.AllowedMethods) == 0 {
		cfg.AllowedMethods = []string{http.MethodGet, http.MethodPost, http.MethodDelete, http.MethodOptions}
	}
	if len(cfg.AllowedHeaders) == 0 {
		cfg.AllowedHeaders = []string{"Accept", "Authorization", "Content-Type", "Range", "X-API-Key", "X-Requested-With"}
	}
	if len(cfg.ExposedHeaders) == 0 {
		cfg.ExposedHeaders = []string{"Accept-Ranges", "Content-Length", "Content-Range", "Content-Type"}
	}
	if cfg.MaxAge == 0 {
		cfg.MaxAge = 300
	}
	return cors.Options{
		AllowedOrigins:   cfg.AllowedOrigins,
		AllowedMethods:   cfg.AllowedMethods,
		AllowedHeaders:   cfg.AllowedHeaders,
		ExposedHeaders:   cfg.ExposedHeaders,
		AllowCredentials: cfg.AllowCredentials,
		MaxAge:           cfg.MaxAge,
	}
}

func (s *Server) secure(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.authorized(r) {
			writeError(w, http.StatusUnauthorized, errors.New("unauthorized"))
			return
		}
		if !s.rate.Allow(r.RemoteAddr) {
			writeError(w, http.StatusTooManyRequests, errors.New("rate limit exceeded"))
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) authorized(r *http.Request) bool {
	if len(s.keys) == 0 {
		return true
	}
	got := r.Header.Get("X-API-Key")
	if got == "" && strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
		got = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	}
	for _, key := range s.keys {
		if subtle.ConstantTimeCompare([]byte(got), []byte(key)) == 1 {
			return true
		}
	}
	return false
}

func (s *Server) withTimeout(fn func(context.Context, http.ResponseWriter, *http.Request)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), s.timeout)
		defer cancel()
		fn(ctx, w, r)
	}
}

func routeParam(r *http.Request, key string) string { return chi.URLParam(r, key) }

func cleanRoots(roots []string) []string {
	out := make([]string, 0, len(roots))
	for _, root := range roots {
		if root == "" {
			continue
		}
		abs, err := filepath.Abs(root)
		if err == nil {
			out = append(out, filepath.Clean(abs))
		}
	}
	return out
}

func (s *Server) validateInputPath(path string) error {
	if len(s.allowedInputRoots) == 0 {
		return nil
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	abs = filepath.Clean(abs)
	if _, err := os.Stat(abs); err != nil {
		return err
	}
	for _, root := range s.allowedInputRoots {
		rel, err := filepath.Rel(root, abs)
		if err == nil && rel != ".." && !strings.HasPrefix(rel, "../") {
			return nil
		}
	}
	return fmt.Errorf("input_path is outside allowed roots")
}

func (s *Server) cacheRootFor(clientCacheDir, fallbackName string) string {
	if s.cacheRoot != "" {
		return filepath.Join(s.cacheRoot, fallbackName)
	}
	if clientCacheDir != "" {
		return clientCacheDir
	}
	return filepath.Join(os.TempDir(), fallbackName)
}

func (s *Server) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) openAPISchema(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/yaml")
	_, _ = w.Write(transcoder.OpenAPISchema)
}

func (s *Server) capabilities(_ context.Context, w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, transcoder.Capabilities())
}

func (s *Server) runtimeCapabilities(_ context.Context, w http.ResponseWriter, _ *http.Request) {
	caps, err := transcoder.RuntimeCapabilities()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, caps)
}

func (s *Server) codecCapabilities(_ context.Context, w http.ResponseWriter, _ *http.Request) {
	caps, err := transcoder.RuntimeCapabilities()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"video_encoders": caps.VideoEncoders,
		"video_decoders": caps.VideoDecoders,
		"audio_encoders": caps.AudioEncoders,
		"audio_decoders": caps.AudioDecoders,
		"muxers":         caps.Muxers,
		"demuxers":       caps.Demuxers,
	})
}

func (s *Server) hardwareCapabilities(_ context.Context, w http.ResponseWriter, _ *http.Request) {
	caps, err := transcoder.RuntimeCapabilities()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"hardware_accelerators": transcoder.SupportedHardwareAccelerators(),
		"video_codecs":          []transcoder.VideoCodec{transcoder.VideoH264, transcoder.VideoHEVC, transcoder.VideoAV1, transcoder.VideoMJPEG},
		"matrix":                transcoder.HardwareSupportMatrix(caps),
	})
}

type probeRequest struct {
	InputPath string `json:"input_path"`
}
type progressiveRequest struct {
	InputPath  string                      `json:"input_path"`
	OutputPath string                      `json:"output_path"`
	Options    transcoder.TranscodeOptions `json:"options"`
}
type hlsRequest struct {
	InputPath    string                `json:"input_path"`
	PlaylistPath string                `json:"playlist_path"`
	Options      transcoder.HLSOptions `json:"options"`
}
type dashRequest struct {
	InputPath string                 `json:"input_path"`
	MPDPath   string                 `json:"mpd_path"`
	Options   transcoder.DASHOptions `json:"options"`
}
type abrHLSRequest struct {
	InputPath      string                   `json:"input_path"`
	MasterPlaylist string                   `json:"master_playlist"`
	Options        transcoder.ABRHLSOptions `json:"options"`
}
type devicePlanRequest struct {
	InputPath    string                        `json:"input_path"`
	OutputPath   string                        `json:"output_path"`
	Capabilities transcoder.ClientCapabilities `json:"capabilities"`
}

func (s *Server) probe(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	var req probeRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.InputPath == "" {
		writeError(w, http.StatusBadRequest, errors.New("input_path is required"))
		return
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
	writeJSON(w, http.StatusOK, info)
}
func (s *Server) progressive(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	var req progressiveRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.InputPath == "" || req.OutputPath == "" {
		writeError(w, http.StatusBadRequest, errors.New("input_path and output_path are required"))
		return
	}
	res, err := transcoder.TranscodeProgressiveFromFile(ctx, req.InputPath, req.OutputPath, req.Options)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}
func (s *Server) hls(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	var req hlsRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.InputPath == "" || req.PlaylistPath == "" {
		writeError(w, http.StatusBadRequest, errors.New("input_path and playlist_path are required"))
		return
	}
	res, err := transcoder.TranscodeHLSFromFile(ctx, req.InputPath, req.PlaylistPath, req.Options)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}
func (s *Server) dash(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	var req dashRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.InputPath == "" || req.MPDPath == "" {
		writeError(w, http.StatusBadRequest, errors.New("input_path and mpd_path are required"))
		return
	}
	res, err := transcoder.TranscodeDASHFromFile(ctx, req.InputPath, req.MPDPath, req.Options)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}
func (s *Server) abrHLS(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	var req abrHLSRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.InputPath == "" || req.MasterPlaylist == "" {
		writeError(w, http.StatusBadRequest, errors.New("input_path and master_playlist are required"))
		return
	}
	res, err := transcoder.TranscodeABRHLSFromFile(ctx, req.InputPath, req.MasterPlaylist, req.Options)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}
func (s *Server) profile(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	var profile transcoder.Profile
	if !decodeJSON(w, r, &profile) {
		return
	}
	res, err := transcoder.TranscodeProfiledDirect(ctx, profile)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}
func (s *Server) devicePlan(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	var req devicePlanRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.InputPath == "" || req.OutputPath == "" {
		writeError(w, http.StatusBadRequest, errors.New("input_path and output_path are required"))
		return
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
	profile := transcoder.BuildDeviceProfile(info, req.Capabilities, req.OutputPath)
	profile.InputPath = req.InputPath
	plan, err := transcoder.BuildPlan(profile)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, plan)
}

type SessionKind string

const (
	SessionProgressive SessionKind = "progressive"
	SessionHLS         SessionKind = "hls"
	SessionDASH        SessionKind = "dash"
	SessionABRHLS      SessionKind = "abr_hls"
	SessionProfiled    SessionKind = "profile"
)

type CreateSessionRequest struct {
	Kind        SessionKind        `json:"kind"`
	Progressive progressiveRequest `json:"progressive,omitempty"`
	HLS         hlsRequest         `json:"hls,omitempty"`
	DASH        dashRequest        `json:"dash,omitempty"`
	ABRHLS      abrHLSRequest      `json:"abr_hls,omitempty"`
	Profile     transcoder.Profile `json:"profile,omitempty"`
}

type JobStatus string

const (
	JobQueued    JobStatus = "queued"
	JobRunning   JobStatus = "running"
	JobSucceeded JobStatus = "succeeded"
	JobFailed    JobStatus = "failed"
	JobCanceled  JobStatus = "canceled"
)

type Job struct {
	ID         string             `json:"id"`
	Kind       SessionKind        `json:"kind"`
	Status     JobStatus          `json:"status"`
	CreatedAt  time.Time          `json:"created_at"`
	StartedAt  *time.Time         `json:"started_at,omitempty"`
	FinishedAt *time.Time         `json:"finished_at,omitempty"`
	Progress   float64            `json:"progress"`
	OutputPath string             `json:"output_path,omitempty"`
	Error      string             `json:"error,omitempty"`
	cancel     context.CancelFunc `json:"-"`
	done       chan struct{}      `json:"-"`
}

type JobManager struct {
	mu   sync.RWMutex
	jobs map[string]*Job
	sem  chan struct{}
	next atomic.Uint64
}

func NewJobManager(max int) *JobManager {
	if max <= 0 {
		max = 2
	}
	return &JobManager{jobs: map[string]*Job{}, sem: make(chan struct{}, max)}
}
func (m *JobManager) Start(parent context.Context, kind SessionKind, run func(context.Context) (string, error)) *Job {
	id := fmt.Sprintf("job-%d", m.next.Add(1))
	ctx, cancel := context.WithCancel(parent)
	j := &Job{ID: id, Kind: kind, Status: JobQueued, CreatedAt: time.Now(), Progress: 0, cancel: cancel, done: make(chan struct{})}
	m.mu.Lock()
	m.jobs[id] = j
	m.mu.Unlock()
	go func() {
		defer close(j.done)
		select {
		case m.sem <- struct{}{}:
			defer func() { <-m.sem }()
		case <-ctx.Done():
			m.finish(id, JobCanceled, "", ctx.Err())
			return
		}
		m.start(id)
		out, err := run(ctx)
		if err != nil {
			if errors.Is(ctx.Err(), context.Canceled) {
				m.finish(id, JobCanceled, out, ctx.Err())
			} else {
				m.finish(id, JobFailed, out, err)
			}
			return
		}
		m.finish(id, JobSucceeded, out, nil)
	}()
	cp, _ := m.Get(id)
	return cp
}
func (m *JobManager) start(id string) {
	now := time.Now()
	m.mu.Lock()
	defer m.mu.Unlock()
	if j := m.jobs[id]; j != nil {
		j.Status = JobRunning
		j.StartedAt = &now
		j.Progress = 0.1
	}
}
func (m *JobManager) finish(id string, status JobStatus, out string, err error) {
	now := time.Now()
	m.mu.Lock()
	defer m.mu.Unlock()
	if j := m.jobs[id]; j != nil {
		j.Status = status
		j.FinishedAt = &now
		j.OutputPath = out
		if status == JobSucceeded {
			j.Progress = 1
		}
		if err != nil {
			j.Error = err.Error()
		}
	}
}
func (m *JobManager) Get(id string) (*Job, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	j, ok := m.jobs[id]
	if !ok {
		return nil, false
	}
	cp := *j
	cp.cancel = nil
	cp.done = nil
	return &cp, true
}
func (m *JobManager) List() []*Job {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*Job, 0, len(m.jobs))
	for _, j := range m.jobs {
		cp := *j
		cp.cancel = nil
		cp.done = nil
		out = append(out, &cp)
	}
	return out
}
func (m *JobManager) Cancel(id string) bool {
	m.mu.RLock()
	j := m.jobs[id]
	m.mu.RUnlock()
	if j == nil {
		return false
	}
	j.cancel()
	return true
}

func (s *Server) createSession(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	var req CreateSessionRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	job := s.jobs.Start(ctx, req.Kind, func(jobCtx context.Context) (string, error) {
		switch req.Kind {
		case SessionProgressive:
			res, err := transcoder.TranscodeProgressiveFromFile(jobCtx, req.Progressive.InputPath, req.Progressive.OutputPath, req.Progressive.Options)
			if res != nil {
				return res.OutputPath, err
			}
			return req.Progressive.OutputPath, err
		case SessionHLS:
			res, err := transcoder.TranscodeHLSFromFile(jobCtx, req.HLS.InputPath, req.HLS.PlaylistPath, req.HLS.Options)
			if res != nil {
				return res.PlaylistPath, err
			}
			return req.HLS.PlaylistPath, err
		case SessionDASH:
			res, err := transcoder.TranscodeDASHFromFile(jobCtx, req.DASH.InputPath, req.DASH.MPDPath, req.DASH.Options)
			if res != nil {
				return res.MPDPath, err
			}
			return req.DASH.MPDPath, err
		case SessionABRHLS:
			res, err := transcoder.TranscodeABRHLSFromFile(jobCtx, req.ABRHLS.InputPath, req.ABRHLS.MasterPlaylist, req.ABRHLS.Options)
			if res != nil {
				return res.MasterPlaylist, err
			}
			return req.ABRHLS.MasterPlaylist, err
		case SessionProfiled:
			res, err := transcoder.TranscodeProfiledDirect(jobCtx, req.Profile)
			if res != nil {
				return res.OutputPath, err
			}
			return req.Profile.OutputPath, err
		default:
			return "", fmt.Errorf("unsupported session kind %q", req.Kind)
		}
	})
	writeJSON(w, http.StatusAccepted, job)
}
func (s *Server) listSessions(_ context.Context, w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.jobs.List())
}
func (s *Server) getSession(_ context.Context, w http.ResponseWriter, r *http.Request) {
	id := routeParam(r, "id")
	j, ok := s.jobs.Get(id)
	if !ok {
		writeError(w, http.StatusNotFound, errors.New("session not found"))
		return
	}
	writeJSON(w, http.StatusOK, j)
}
func (s *Server) cancelSession(_ context.Context, w http.ResponseWriter, r *http.Request) {
	if !s.jobs.Cancel(routeParam(r, "id")) {
		writeError(w, http.StatusNotFound, errors.New("session not found"))
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "cancel_requested"})
}
func (s *Server) sessionEvents(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	id := routeParam(r, "id")
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, errors.New("streaming unsupported"))
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			j, ok := s.jobs.Get(id)
			if !ok {
				fmt.Fprintf(w, "event: error\ndata: session not found\n\n")
				flusher.Flush()
				return
			}
			b, _ := json.Marshal(j)
			fmt.Fprintf(w, "event: progress\ndata: %s\n\n", b)
			flusher.Flush()
			if j.Status == JobSucceeded || j.Status == JobFailed || j.Status == JobCanceled {
				return
			}
		}
	}
}

func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 2<<20)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return false
	}
	return true
}
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func writeError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

type rateLimiter struct {
	mu        sync.Mutex
	perMinute int
	buckets   map[string]rateBucket
}
type rateBucket struct {
	window time.Time
	count  int
}

func newRateLimiter(perMinute int) *rateLimiter {
	return &rateLimiter{perMinute: perMinute, buckets: map[string]rateBucket{}}
}
func (r *rateLimiter) Allow(key string) bool {
	if r.perMinute <= 0 {
		return true
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now().Truncate(time.Minute)
	b := r.buckets[key]
	if !b.window.Equal(now) {
		b = rateBucket{window: now}
	}
	if b.count >= r.perMinute {
		return false
	}
	b.count++
	r.buckets[key] = b
	return true
}

func normalizeLibraries(in map[string]LibraryConfig) map[string]LibraryConfig {
	out := map[string]LibraryConfig{}
	for id, lib := range in {
		if id == "" || (lib.VFS == "" && lib.EncodedConfig == "") {
			continue
		}
		lib.ID = id
		out[id] = lib
	}
	return out
}

func normalizeProfiles(in map[string]PlaybackProfile) map[string]PlaybackProfile {
	out := map[string]PlaybackProfile{}
	for id, p := range in {
		if id == "" {
			continue
		}
		p.ID = id
		applyProfileDefaults(&p)
		out[id] = p
	}
	return out
}
