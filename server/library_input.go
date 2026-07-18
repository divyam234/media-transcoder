package server

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
)

type resolvedLibraryInput struct {
	Input       string
	Size        int64
	ModTime     time.Time
	Fingerprint string
	Cleanup     func()
}

func validateLibraryConfig(lib LibraryConfig) error {
	hasVFS := strings.TrimSpace(lib.VFS) != "" || strings.TrimSpace(lib.EncodedConfig) != ""
	hasHTTP := lib.HTTP != nil
	if hasVFS == hasHTTP {
		return errors.New("library requires exactly one of vfs/encoded_config or http")
	}
	if hasVFS {
		_, err := decodeLibraryVFSConfig(lib)
		return err
	}
	base, err := parseHTTPLibraryBaseURL(lib.HTTP.BaseURL)
	if err != nil {
		return err
	}
	lib.HTTP.BaseURL = base.String()
	for name, value := range lib.HTTP.Headers {
		if strings.TrimSpace(name) == "" {
			return errors.New("http.headers contains an empty header name")
		}
		if strings.ContainsAny(value, "\r\n") {
			return fmt.Errorf("http header %q contains a newline", name)
		}
	}
	return nil
}

func parseHTTPLibraryBaseURL(raw string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return nil, fmt.Errorf("parse http.base_url: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" || u.Host == "" {
		return nil, errors.New("http.base_url must be an absolute http or https URL")
	}
	if u.User != nil {
		return nil, errors.New("http.base_url must not contain user info")
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("http.base_url must not contain a query or fragment")
	}
	if !strings.HasSuffix(u.Path, "/") {
		u.Path += "/"
	}
	return u, nil
}

func (s *Server) resolveLibraryInput(ctx context.Context, libraryID, rawRel string) (resolvedLibraryInput, error) {
	s.configMu.RLock()
	lib, ok := s.libraries[libraryID]
	s.configMu.RUnlock()
	if !ok {
		return resolvedLibraryInput{}, errors.New("library not found")
	}
	if lib.HTTP == nil {
		resolved, err := s.resolveLibraryVFSInput(ctx, libraryID, rawRel)
		if err != nil {
			return resolvedLibraryInput{}, err
		}
		return resolvedLibraryInput{
			Input:       resolved.Input,
			Size:        resolved.Info.Size(),
			ModTime:     resolved.Info.ModTime(),
			Fingerprint: fmt.Sprintf("vfs|%d|%d", resolved.Info.Size(), resolved.Info.ModTime().UnixNano()),
			Cleanup:     resolved.Cleanup,
		}, nil
	}

	rel, err := urlPathClean(rawRel)
	if err != nil {
		return resolvedLibraryInput{}, err
	}
	base, err := parseHTTPLibraryBaseURL(lib.HTTP.BaseURL)
	if err != nil {
		return resolvedLibraryInput{}, err
	}
	target := base.ResolveReference(&url.URL{Path: rel})
	resolved, err := s.resolveInput(ctx, "", target.String(), lib.HTTP.Headers)
	if err != nil {
		return resolvedLibraryInput{}, err
	}
	return resolvedLibraryInput{
		Input:       resolved.Input,
		Size:        resolved.Size,
		ModTime:     resolved.ModTime,
		Fingerprint: resolved.SourceKey,
		Cleanup:     resolved.Cleanup,
	}, nil
}
