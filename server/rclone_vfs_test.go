package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	transcoder "media-transcoder"
	"testing"
)

func TestDecodeEncodedLibraryVFSConfig(t *testing.T) {
	root := t.TempDir()
	raw, err := json.Marshal(encodedVFSConfig{
		VFS: root,
		Options: map[string]string{
			"vfs_cache_mode": "full",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	lib := LibraryConfig{EncodedConfig: base64.RawURLEncoding.EncodeToString(raw)}
	got, err := decodeLibraryVFSConfig(lib)
	if err != nil {
		t.Fatal(err)
	}
	if got.VFS != root || got.Options["vfs_cache_mode"] != "full" {
		t.Fatalf("decoded config mismatch: %+v", got)
	}
}

func TestLocalLibraryStreamsDirectlyThroughAVIO(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", "testdata"))
	if err != nil {
		t.Fatal(err)
	}
	cache := t.TempDir()
	s := New(Config{
		CacheRoot: cache,
		Libraries: map[string]LibraryConfig{
			"media": {VFS: root},
		},
	})
	t.Cleanup(s.Close)

	resolved, err := s.resolveLibraryVFSInput(context.Background(), "media", "sample.mp4")
	if err != nil {
		t.Fatal(err)
	}
	defer resolved.Cleanup()
	if !strings.HasPrefix(resolved.Input, "goavio:") {
		t.Fatalf("input %q is not a custom AVIO stream", resolved.Input)
	}
	info, err := transcoder.ProbeFile(context.Background(), resolved.Input)
	if err != nil {
		t.Fatal(err)
	}
	if info.Width <= 0 || info.Height <= 0 || info.Duration <= 0 {
		t.Fatalf("invalid probed media info: %+v", info)
	}
	if _, err := os.Stat(filepath.Join(cache, "media-transcoder-vfs")); !os.IsNotExist(err) {
		t.Fatalf("direct AVIO streaming should not materialize a VFS cache: %v", err)
	}
}

func TestPlainLibraryVFSOptionsAcceptHumanReadableSizes(t *testing.T) {
	lib := LibraryConfig{
		VFS: t.TempDir(),
		Options: map[string]string{
			"vfs_cache_mode":     "full",
			"vfs_cache_max_size": "100GiB",
			"vfs_read_ahead":     "64MiB",
		},
	}
	cfg, err := decodeLibraryVFSConfig(lib)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Options["vfs_cache_max_size"] != "100GiB" || cfg.Options["vfs_read_ahead"] != "64MiB" {
		t.Fatalf("options were not preserved: %#v", cfg.Options)
	}
}
