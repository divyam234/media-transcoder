package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	transcoder "media-transcoder"

	_ "github.com/rclone/rclone/backend/all"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/config"
	"github.com/rclone/rclone/fs/config/configfile"
	"github.com/rclone/rclone/fs/config/configmap"
	"github.com/rclone/rclone/fs/config/configstruct"
	"github.com/rclone/rclone/vfs"
	"github.com/rclone/rclone/vfs/vfscommon"
)

var (
	rcloneCacheOnce sync.Once
	rcloneCacheErr  error
)

func init() {
	configfile.Install()
}

func initRcloneCacheDir(vfsCacheRoot string) error {
	rcloneCacheOnce.Do(func() {
		if strings.TrimSpace(vfsCacheRoot) == "" {
			return
		}
		cacheDir := filepath.Clean(vfsCacheRoot)
		if err := os.MkdirAll(cacheDir, 0o755); err != nil {
			rcloneCacheErr = err
			return
		}
		rcloneCacheErr = config.SetCacheDir(cacheDir)
	})
	return rcloneCacheErr
}

type encodedVFSConfig struct {
	VFS     string            `json:"vfs"`
	Options map[string]string `json:"options,omitempty"`
}

type libraryVFS struct {
	spec string
	vfs  *vfs.VFS
}

func decodeLibraryVFSConfig(lib LibraryConfig) (encodedVFSConfig, error) {
	cfg := encodedVFSConfig{VFS: strings.TrimSpace(lib.VFS)}
	if lib.EncodedConfig != "" {
		raw, err := decodeBase64Config(lib.EncodedConfig)
		if err != nil {
			return encodedVFSConfig{}, fmt.Errorf("decode encoded_config: %w", err)
		}
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return encodedVFSConfig{}, fmt.Errorf("parse encoded_config: %w", err)
		}
	}
	if cfg.VFS == "" {
		return encodedVFSConfig{}, errors.New("library requires vfs or encoded_config")
	}
	return cfg, nil
}

func decodeBase64Config(value string) ([]byte, error) {
	value = strings.TrimSpace(value)
	encodings := []*base64.Encoding{
		base64.RawURLEncoding,
		base64.URLEncoding,
		base64.RawStdEncoding,
		base64.StdEncoding,
	}
	var last error
	for _, encoding := range encodings {
		decoded, err := encoding.DecodeString(value)
		if err == nil {
			return decoded, nil
		}
		last = err
	}
	return nil, last
}

func (s *Server) getLibraryVFS(ctx context.Context, libraryID string, lib LibraryConfig) (*libraryVFS, error) {
	if err := initRcloneCacheDir(s.vfsCacheRoot); err != nil {
		return nil, fmt.Errorf("initialize rclone cache directory: %w", err)
	}
	cfg, err := decodeLibraryVFSConfig(lib)
	if err != nil {
		return nil, err
	}
	fingerprintBytes, _ := json.Marshal(cfg)
	fingerprint := string(fingerprintBytes)

	s.vfsMu.Lock()
	defer s.vfsMu.Unlock()
	if existing := s.libraryVFS[libraryID]; existing != nil && existing.spec == fingerprint {
		return existing, nil
	}

	remote, err := fs.NewFs(ctx, cfg.VFS)
	if err != nil && !errors.Is(err, fs.ErrorIsFile) {
		return nil, fmt.Errorf("open rclone filesystem %q: %w", cfg.VFS, err)
	}
	options := vfscommon.Opt
	options.ReadOnly = true
	if len(cfg.Options) != 0 {
		if err := configstruct.Set(configmap.Simple(cfg.Options), &options); err != nil {
			return nil, fmt.Errorf("apply VFS options: %w", err)
		}
		options.ReadOnly = true
	}
	instance := &libraryVFS{spec: fingerprint, vfs: vfs.New(ctx, remote, &options)}
	s.libraryVFS[libraryID] = instance
	return instance, nil
}

type vfsReadSeekCloser struct {
	vfs.Handle
}

func (h *vfsReadSeekCloser) Close() error {
	closeErr := h.Handle.Close()
	releaseErr := h.Handle.Release()
	if closeErr != nil {
		return closeErr
	}
	return releaseErr
}

var _ io.ReadSeekCloser = (*vfsReadSeekCloser)(nil)

type resolvedVFSInput struct {
	Input   string
	Info    os.FileInfo
	Cleanup func()
}

func (s *Server) resolveLibraryVFSInput(ctx context.Context, libraryID, rawRel string) (resolvedVFSInput, error) {
	s.configMu.RLock()
	lib, ok := s.libraries[libraryID]
	s.configMu.RUnlock()
	if !ok {
		return resolvedVFSInput{}, errors.New("library not found")
	}
	rel, err := urlPathClean(rawRel)
	if err != nil {
		return resolvedVFSInput{}, err
	}
	instance, err := s.getLibraryVFS(ctx, libraryID, lib)
	if err != nil {
		return resolvedVFSInput{}, err
	}
	instance.vfs.FlushDirCache()
	handle, err := instance.vfs.Open(rel)
	if err != nil {
		return resolvedVFSInput{}, fmt.Errorf("open %q through rclone VFS: %w", rel, err)
	}
	info, err := handle.Stat()
	_ = handle.Close()
	_ = handle.Release()
	if err != nil {
		return resolvedVFSInput{}, fmt.Errorf("stat %q through rclone VFS: %w", rel, err)
	}
	if info.IsDir() {
		return resolvedVFSInput{}, errors.New("media path is a directory")
	}
	input, unregister, err := transcoder.RegisterReadSeekerFactory(rel, func() (io.ReadSeekCloser, error) {
		opened, err := instance.vfs.Open(rel)
		if err != nil {
			return nil, err
		}
		return &vfsReadSeekCloser{Handle: opened}, nil
	})
	if err != nil {
		return resolvedVFSInput{}, err
	}
	return resolvedVFSInput{Input: input, Info: info, Cleanup: unregister}, nil
}

func (s *Server) shutdownLibraryVFS() {
	s.vfsMu.Lock()
	instances := make([]*libraryVFS, 0, len(s.libraryVFS))
	for _, instance := range s.libraryVFS {
		instances = append(instances, instance)
	}
	s.libraryVFS = map[string]*libraryVFS{}
	s.vfsMu.Unlock()
	for _, instance := range instances {
		if instance != nil && instance.vfs != nil {
			instance.vfs.Shutdown()
		}
	}
}
