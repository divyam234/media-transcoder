package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	transcoder "media-transcoder"

	"github.com/go-chi/chi/v5"
	"gopkg.in/yaml.v3"
)

// FileConfig is the YAML configuration loaded by --config. It intentionally
// models only server-owned playback concerns: library roots and reusable
// output profiles. Runtime segment generation remains direct libav/cgo.
type FileConfig struct {
	Server    FileServerConfig           `yaml:"server" json:"server"`
	Libraries map[string]LibraryConfig   `yaml:"libraries" json:"libraries"`
	Profiles  map[string]PlaybackProfile `yaml:"profiles" json:"profiles"`
}

type FileServerConfig struct {
	Addr                       string   `yaml:"addr" json:"addr"`
	CacheRoot                  string   `yaml:"cache_root" json:"cache_root"`
	VFSCacheRoot               string   `yaml:"vfs_cache_root" json:"vfs_cache_root"`
	Debug                      bool     `yaml:"debug" json:"debug"`
	RequestTimeout             string   `yaml:"request_timeout" json:"request_timeout"`
	HardwareDecoderIdleTimeout string   `yaml:"hardware_decoder_idle_timeout" json:"hardware_decoder_idle_timeout"`
	MaxJobs                    int      `yaml:"max_jobs" json:"max_jobs"`
	RateLimit                  int      `yaml:"rate_limit" json:"rate_limit"`
	APIKeys                    []string `yaml:"api_keys" json:"api_keys"`
	AllowInputRoots            []string `yaml:"allow_input_roots" json:"allow_input_roots"`
	HTTPAllowedHosts           []string `yaml:"http_allowed_hosts" json:"http_allowed_hosts"`
	CORSOrigins                []string `yaml:"cors_origins" json:"cors_origins"`
	CORSCredentials            bool     `yaml:"cors_credentials" json:"cors_credentials"`
}

type LibraryConfig struct {
	ID            string             `yaml:"-" json:"id"`
	VFS           string             `yaml:"vfs,omitempty" json:"-"`
	EncodedConfig string             `yaml:"encoded_config,omitempty" json:"-"`
	Options       map[string]string  `yaml:"options,omitempty" json:"-"`
	HTTP          *HTTPLibraryConfig `yaml:"http,omitempty" json:"-"`
}

type HTTPLibraryConfig struct {
	BaseURL string            `yaml:"base_url" json:"-"`
	Headers map[string]string `yaml:"headers,omitempty" json:"-"`
}

type PlaybackProfile struct {
	ID             string                     `yaml:"-" json:"id"`
	Container      string                     `yaml:"container" json:"container"`
	SegmentType    string                     `yaml:"segment_type" json:"segment_type"`
	SegmentSeconds float64                    `yaml:"segment_seconds" json:"segment_seconds"`
	Audio          AudioProfile               `yaml:"audio" json:"audio"`
	Video          VideoProfile               `yaml:"video" json:"video"`
	Variants       []transcoder.LadderVariant `yaml:"variants" json:"variants"`
}

type AudioProfile struct {
	Mode       transcoder.AudioMode `yaml:"mode" json:"mode"`
	Codec      string               `yaml:"codec" json:"codec"`
	Bitrate    transcoder.Bitrate   `yaml:"bitrate" json:"bitrate"`
	Channels   int                  `yaml:"channels" json:"channels"`
	SampleRate int                  `yaml:"sample_rate" json:"sample_rate"`
}

type VideoProfile struct {
	Codec          string  `yaml:"codec" json:"codec"`
	EncoderName    string  `yaml:"encoder_name" json:"encoder_name"`
	HardwareDevice string  `yaml:"hardware_device" json:"hardware_device"`
	HardwareDecode bool    `yaml:"hardware_decode" json:"hardware_decode"`
	Preset         string  `yaml:"preset" json:"preset"`
	CRF            int     `yaml:"crf" json:"crf"`
	GOPSize        int     `yaml:"gop_size" json:"gop_size"`
	MaxBFrames     int     `yaml:"max_b_frames" json:"max_b_frames"`
	FPS            float64 `yaml:"fps" json:"fps"`
}

func LoadConfigFile(path string) (Config, error) {
	fc, err := LoadPlaybackConfig(path)
	if err != nil {
		return Config{}, err
	}
	cfg := Config{ConfigPath: path, Libraries: fc.Libraries, Profiles: fc.Profiles}
	cfg.CacheRoot = fc.Server.CacheRoot
	cfg.VFSCacheRoot = fc.Server.VFSCacheRoot
	cfg.APIKeys = fc.Server.APIKeys
	cfg.RateLimitPerMinute = fc.Server.RateLimit
	cfg.MaxConcurrentJobs = fc.Server.MaxJobs
	cfg.AllowedInputRoots = fc.Server.AllowInputRoots
	cfg.HTTPAllowedHosts = fc.Server.HTTPAllowedHosts
	if fc.Server.RequestTimeout != "" {
		d, err := time.ParseDuration(fc.Server.RequestTimeout)
		if err != nil {
			return Config{}, fmt.Errorf("server.request_timeout: %w", err)
		}
		cfg.RequestTimeout = d
	}
	if fc.Server.HardwareDecoderIdleTimeout != "" {
		d, err := time.ParseDuration(fc.Server.HardwareDecoderIdleTimeout)
		if err != nil {
			return Config{}, fmt.Errorf("server.hardware_decoder_idle_timeout: %w", err)
		}
		if d <= 0 {
			return Config{}, fmt.Errorf("server.hardware_decoder_idle_timeout must be > 0")
		}
		cfg.HardwareDecoderIdleTimeout = d
	}
	if len(fc.Server.CORSOrigins) > 0 || fc.Server.CORSCredentials {
		cfg.CORS.AllowedOrigins = fc.Server.CORSOrigins
		cfg.CORS.AllowCredentials = fc.Server.CORSCredentials
	}
	return cfg, nil
}

func LoadPlaybackConfig(path string) (*FileConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg FileConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}
	if cfg.Libraries == nil {
		cfg.Libraries = map[string]LibraryConfig{}
	}
	if cfg.Profiles == nil {
		cfg.Profiles = map[string]PlaybackProfile{}
	}
	for id, lib := range cfg.Libraries {
		lib.ID = id
		if err := validateLibraryConfig(lib); err != nil {
			return nil, fmt.Errorf("library %s: %w", id, err)
		}
		cfg.Libraries[id] = lib
	}
	for id, p := range cfg.Profiles {
		p.ID = id
		applyProfileDefaults(&p)
		for i, variant := range p.Variants {
			if _, err := transcoder.ParseCropAspect(variant.CropAspect); err != nil {
				return nil, fmt.Errorf("profile %s variant %d: %w", id, i, err)
			}
		}
		cfg.Profiles[id] = p
	}
	return &cfg, nil
}

func applyProfileDefaults(p *PlaybackProfile) {
	if p.Container == "" {
		p.Container = "hls"
	}
	if p.SegmentType == "" {
		p.SegmentType = "fmp4"
	}
	if p.SegmentSeconds <= 0 {
		p.SegmentSeconds = 4
	}
	if p.Audio.Mode == "" {
		p.Audio.Mode = transcoder.AudioTranscode
	}
	if p.Audio.Codec == "" {
		p.Audio.Codec = "aac"
	}
	if p.Audio.Bitrate <= 0 {
		p.Audio.Bitrate = 128000
	}
	if p.Audio.Channels <= 0 {
		p.Audio.Channels = 2
	}
	if p.Video.Preset == "" {
		p.Video.Preset = string(transcoder.PresetFastest)
	}
	if p.Video.CRF <= 0 {
		p.Video.CRF = 28
	}
	if p.Video.GOPSize <= 0 {
		p.Video.GOPSize = int(mathRound(p.SegmentSeconds * 24))
		if p.Video.GOPSize <= 0 {
			p.Video.GOPSize = 96
		}
	}
	if p.Video.MaxBFrames < 0 {
		p.Video.MaxBFrames = 0
	}
	if len(p.Variants) == 0 {
		p.Variants = transcoder.DefaultLadder()
	}
}

func mathRound(v float64) int {
	if v < 0 {
		return int(v - 0.5)
	}
	return int(v + 0.5)
}

func (p PlaybackProfile) HLSOptions() transcoder.HLSOptions {
	o := transcoder.HLSOptions{}
	o.SegmentSeconds = p.SegmentSeconds
	o.SegmentType = p.SegmentType
	o.PlaylistType = "vod"
	o.AudioMode = p.Audio.Mode
	o.AudioCodec = p.Audio.Codec
	o.AudioBitrate = int(p.Audio.Bitrate)
	o.AudioChannels = p.Audio.Channels
	o.EncoderName = p.Video.EncoderName
	o.HardwareDevice = p.Video.HardwareDevice
	o.HardwareDecode = p.Video.HardwareDecode
	o.Preset = p.Video.Preset
	o.CRF = p.Video.CRF
	o.GOPSize = p.Video.GOPSize
	o.MaxBFrames = p.Video.MaxBFrames
	o.FPS = p.Video.FPS
	return o
}

func (p PlaybackProfile) DASHOptions() transcoder.DASHOptions {
	h := p.HLSOptions()
	return transcoder.DASHOptions{TranscodeOptions: h.TranscodeOptions, SegmentSeconds: h.SegmentSeconds}
}

func (s *Server) listProfiles(_ context.Context, w http.ResponseWriter, _ *http.Request) {
	s.configMu.RLock()
	defer s.configMu.RUnlock()
	ids := make([]string, 0, len(s.profiles))
	for id := range s.profiles {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]PlaybackProfile, 0, len(ids))
	for _, id := range ids {
		out = append(out, s.profiles[id])
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) getProfile(_ context.Context, w http.ResponseWriter, r *http.Request) {
	id := routeParam(r, "id")
	s.configMu.RLock()
	p, ok := s.profiles[id]
	s.configMu.RUnlock()
	if !ok {
		writeError(w, http.StatusNotFound, errors.New("profile not found"))
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (s *Server) listLibraries(_ context.Context, w http.ResponseWriter, _ *http.Request) {
	s.configMu.RLock()
	defer s.configMu.RUnlock()
	ids := make([]string, 0, len(s.libraries))
	for id := range s.libraries {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]LibraryConfig, 0, len(ids))
	for _, id := range ids {
		out = append(out, s.libraries[id])
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) getLibrary(_ context.Context, w http.ResponseWriter, r *http.Request) {
	id := routeParam(r, "id")
	s.configMu.RLock()
	lib, ok := s.libraries[id]
	s.configMu.RUnlock()
	if !ok {
		writeError(w, http.StatusNotFound, errors.New("library not found"))
		return
	}
	writeJSON(w, http.StatusOK, lib)
}

func (s *Server) reloadConfig(_ context.Context, w http.ResponseWriter, _ *http.Request) {
	if s.configPath == "" {
		writeError(w, http.StatusBadRequest, errors.New("no config path was provided"))
		return
	}
	fc, err := LoadPlaybackConfig(s.configPath)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	s.configMu.Lock()
	s.libraries = fc.Libraries
	s.profiles = fc.Profiles
	s.configMu.Unlock()
	s.shutdownLibraryVFS()
	writeJSON(w, http.StatusOK, map[string]any{"status": "reloaded", "profiles": len(fc.Profiles), "libraries": len(fc.Libraries)})
}

func urlPathClean(p string) (string, error) {
	p = strings.TrimPrefix(p, "/")
	if p == "" {
		return "", errors.New("media path is required")
	}
	if strings.Contains(p, "\x00") || filepath.IsAbs(p) {
		return "", errors.New("invalid media path")
	}
	parts := strings.Split(p, "/")
	clean := make([]string, 0, len(parts))
	for _, part := range parts {
		if part == "" || part == "." {
			continue
		}
		if part == ".." {
			return "", errors.New("path traversal is not allowed")
		}
		clean = append(clean, part)
	}
	if len(clean) == 0 {
		return "", errors.New("media path is required")
	}
	return strings.Join(clean, "/"), nil
}

func pathInside(root, full string) bool {
	rel, err := filepath.Rel(root, full)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, "../") && !filepath.IsAbs(rel)
}

func (s *Server) getProfileCopy(id string) (PlaybackProfile, bool) {
	s.configMu.RLock()
	defer s.configMu.RUnlock()
	p, ok := s.profiles[id]
	return p, ok
}

func (s *Server) libraryHLS(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	profileID := routeParam(r, "profile")
	libraryID := routeParam(r, "library")
	rest := chi.URLParam(r, "*")
	mediaPath, kind, variant, name, err := parseLibraryPlaybackPath(rest, "hls")
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	sess, err := s.ensureLibraryHLSSession(ctx, profileID, libraryID, mediaPath)
	if err != nil {
		status := http.StatusBadRequest
		if strings.Contains(err.Error(), "not found") {
			status = http.StatusNotFound
		}
		writeError(w, status, err)
		return
	}
	switch kind {
	case "master":
		if err := s.ensureHLSCodecDescriptors(ctx, sess); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		s.writeHLSMaster(w, sess)
	case "playlist":
		if variant == "" {
			s.dynamicHLSPlaylist(ctx, w, requestWithSessionID(r, sess.ID))
			return
		}
		s.dynamicHLSVariantPlaylist(ctx, w, requestWithSessionAndVariant(r, sess.ID, variant))
		return
	case "segment":
		if variant == "" {
			s.dynamicHLSSegment(ctx, w, requestWithSessionAndName(r, sess.ID, name))
			return
		}
		s.dynamicHLSVariantSegment(ctx, w, requestWithSessionVariantName(r, sess.ID, variant, name), variant)
		return
	}
}

func (s *Server) libraryDASH(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	profileID := routeParam(r, "profile")
	libraryID := routeParam(r, "library")
	rest := chi.URLParam(r, "*")
	mediaPath, kind, variant, name, err := parseLibraryPlaybackPath(rest, "dash")
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	sess, err := s.ensureLibraryDASHSession(ctx, profileID, libraryID, mediaPath)
	if err != nil {
		status := http.StatusBadRequest
		if strings.Contains(err.Error(), "not found") {
			status = http.StatusNotFound
		}
		writeError(w, status, err)
		return
	}
	switch kind {
	case "manifest":
		if err := s.ensureDASHCodecDescriptors(ctx, sess); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		s.writeDASHManifest(w, sess)
	case "segment":
		s.dynamicDASHVariantSegment(ctx, w, requestWithDASHSessionVariantName(r, sess.ID, variant, name), variant)
	case "audio_segment":
		s.dynamicDASHAudioSegment(ctx, w, requestWithSessionAndName(r, sess.ID, name))
	}
}

func parseLibraryPlaybackPath(rest, protocol string) (mediaPath, kind, variant, name string, err error) {
	rest = strings.TrimPrefix(rest, "/")
	switch protocol {
	case "hls":
		if strings.HasSuffix(rest, "/master.m3u8") {
			return strings.TrimSuffix(rest, "/master.m3u8"), "master", "", "", nil
		}
		if strings.HasSuffix(rest, "/video.m3u8") {
			base := strings.TrimSuffix(rest, "/video.m3u8")
			marker := "/variant/"
			if i := strings.LastIndex(base, marker); i >= 0 {
				return base[:i], "playlist", base[i+len(marker):], "", nil
			}
			return base, "playlist", "", "", nil
		}
		marker := "/variant/"
		if i := strings.LastIndex(rest, marker); i >= 0 {
			media := rest[:i]
			tail := rest[i+len(marker):]
			parts := strings.Split(tail, "/segment/")
			if len(parts) == 2 {
				return media, "segment", parts[0], parts[1], nil
			}
		}
		if i := strings.LastIndex(rest, "/segment/"); i >= 0 {
			return rest[:i], "segment", "", rest[i+len("/segment/"):], nil
		}
	case "dash":
		if strings.HasSuffix(rest, "/manifest.mpd") {
			return strings.TrimSuffix(rest, "/manifest.mpd"), "manifest", "", "", nil
		}
		if i := strings.LastIndex(rest, "/audio/segment/"); i >= 0 {
			return rest[:i], "audio_segment", "audio", rest[i+len("/audio/segment/"):], nil
		}
		marker := "/variant/"
		if i := strings.LastIndex(rest, marker); i >= 0 {
			media := rest[:i]
			tail := rest[i+len(marker):]
			parts := strings.Split(tail, "/segment/")
			if len(parts) == 2 {
				return media, "segment", parts[0], parts[1], nil
			}
		}
		if i := strings.LastIndex(rest, "/segment/"); i >= 0 {
			return rest[:i], "segment", "default", rest[i+len("/segment/"):], nil
		}
	}
	return "", "", "", "", errors.New("unsupported playback URL")
}

func profileHash(p PlaybackProfile) string {
	data, _ := yaml.Marshal(p)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:8])
}

func librarySessionID(prefix, profileID, libraryID, rel string, resolved resolvedLibraryInput, p PlaybackProfile) string {
	h := sha256.New()
	fmt.Fprintf(h, "%s|%s|%s|%s|%d|%d|%s|%s", prefix, profileID, libraryID, rel, resolved.Size, resolved.ModTime.UnixNano(), resolved.Fingerprint, profileHash(p))
	return hex.EncodeToString(h.Sum(nil))[:24]
}

func (s *Server) ensureLibraryHLSSession(ctx context.Context, profileID, libraryID, rel string) (*DynamicHLSSession, error) {
	p, ok := s.getProfileCopy(profileID)
	if !ok {
		return nil, errors.New("profile not found")
	}
	resolved, err := s.resolveLibraryInput(ctx, libraryID, rel)
	if err != nil {
		return nil, err
	}
	id := librarySessionID("hls", profileID, libraryID, rel, resolved, p)
	if sess, ok := s.dynHLS.Get(id); ok {
		resolved.Cleanup()
		return sess, nil
	}
	s.sessionMu.Lock()
	defer s.sessionMu.Unlock()
	if sess, ok := s.dynHLS.Get(id); ok {
		resolved.Cleanup()
		return sess, nil
	}
	info, err := transcoder.ProbeFile(ctx, resolved.Input)
	if err != nil {
		resolved.Cleanup()
		return nil, err
	}
	opts := p.HLSOptions()
	if info.HasAudio && opts.AudioMode == "" {
		opts.AudioMode = transcoder.AudioTranscode
	}
	cacheRoot := s.cacheRootFor("", "media-transcoder-hls")
	cacheDir := filepath.Join(cacheRoot, id)
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		resolved.Cleanup()
		return nil, err
	}
	sessCtx, cancel := context.WithCancel(context.Background())
	sourceKey := "hls|" + profileID + "|" + libraryID + "|" + rel
	sess := &DynamicHLSSession{ID: id, InputPath: resolved.Input, InputCleanup: resolved.Cleanup, Options: opts, Variants: buildHLSVariants(opts, p.Variants, info), CacheDir: cacheDir, PrewarmSegments: 2, Info: info, CreatedAt: time.Now(), SourceKey: sourceKey, ctx: sessCtx, cancel: cancel, decoderIdleTimeout: s.hardwareDecoderIdleTimeout}
	for _, stale := range s.dynHLS.ReplaceSourceSession(sess) {
		if stale.cancel != nil {
			stale.cancel()
		}
		stale.closeVideoDecoder()
		if stale.InputCleanup != nil {
			s.retiredInputs = append(s.retiredInputs, stale.InputCleanup)
		}
		_ = os.RemoveAll(stale.CacheDir)
	}
	s.metrics.hlsSessions.Add(1)
	return sess, nil
}

func (s *Server) ensureLibraryDASHSession(ctx context.Context, profileID, libraryID, rel string) (*DynamicDASHSession, error) {
	p, ok := s.getProfileCopy(profileID)
	if !ok {
		return nil, errors.New("profile not found")
	}
	resolved, err := s.resolveLibraryInput(ctx, libraryID, rel)
	if err != nil {
		return nil, err
	}
	id := librarySessionID("dash", profileID, libraryID, rel, resolved, p)
	if sess, ok := s.dynDASH.Get(id); ok {
		resolved.Cleanup()
		return sess, nil
	}
	s.sessionMu.Lock()
	defer s.sessionMu.Unlock()
	if sess, ok := s.dynDASH.Get(id); ok {
		resolved.Cleanup()
		return sess, nil
	}
	info, err := transcoder.ProbeFile(ctx, resolved.Input)
	if err != nil {
		resolved.Cleanup()
		return nil, err
	}
	opts := p.DASHOptions()
	fps := opts.FPS
	if fps <= 0 {
		fps = info.FPS
	}
	opts.SegmentSeconds = alignDASHSegmentSeconds(opts.SegmentSeconds, fps)
	audioOpts := opts.TranscodeOptions
	if info.HasAudio {
		if audioOpts.AudioMode == "" {
			audioOpts.AudioMode = transcoder.AudioTranscode
		}
	} else {
		audioOpts.AudioMode = transcoder.AudioSkip
	}
	opts.AudioMode = transcoder.AudioSkip
	cacheRoot := s.cacheRootFor("", "media-transcoder-dash")
	cacheDir := filepath.Join(cacheRoot, id)
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		resolved.Cleanup()
		return nil, err
	}
	sessCtx, cancel := context.WithCancel(context.Background())
	sourceKey := "dash|" + profileID + "|" + libraryID + "|" + rel
	sess := &DynamicDASHSession{ID: id, InputPath: resolved.Input, InputCleanup: resolved.Cleanup, Options: opts, AudioOptions: audioOpts, Variants: buildDASHVariants(opts, p.Variants, info), CacheDir: cacheDir, PrewarmSegments: 3, Info: info, CreatedAt: time.Now(), SourceKey: sourceKey, ctx: sessCtx, cancel: cancel, decoderIdleTimeout: s.hardwareDecoderIdleTimeout}
	for _, stale := range s.dynDASH.ReplaceSourceSession(sess) {
		if stale.cancel != nil {
			stale.cancel()
		}
		stale.closeVideoDecoder()
		if stale.InputCleanup != nil {
			s.retiredInputs = append(s.retiredInputs, stale.InputCleanup)
		}
		_ = os.RemoveAll(stale.CacheDir)
	}
	s.metrics.dashSessions.Add(1)
	return sess, nil
}

// requestWith... creates a shallow clone with chi route params overwritten. It
// lets library playback URLs reuse the existing session handlers instead of
// duplicating segment generation behavior.
func requestWithSessionID(r *http.Request, id string) *http.Request {
	return withRouteParams(r, map[string]string{"id": id})
}
func requestWithSessionAndName(r *http.Request, id, name string) *http.Request {
	return withRouteParams(r, map[string]string{"id": id, "name": name})
}
func requestWithSessionAndVariant(r *http.Request, id, variant string) *http.Request {
	return withRouteParams(r, map[string]string{"id": id, "variant": variant})
}
func requestWithSessionVariantName(r *http.Request, id, variant, name string) *http.Request {
	return withRouteParams(r, map[string]string{"id": id, "variant": variant, "name": name})
}
func requestWithDASHSessionVariantName(r *http.Request, id, variant, name string) *http.Request {
	return withRouteParams(r, map[string]string{"id": id, "variant": variant, "name": name})
}

func withRouteParams(r *http.Request, vals map[string]string) *http.Request {
	nrc := chi.NewRouteContext()
	for k, v := range vals {
		nrc.URLParams.Add(k, v)
	}
	return r.WithContext(context.WithValue(r.Context(), chi.RouteCtxKey, nrc))
}
