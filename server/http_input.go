package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	transcoder "media-transcoder"
)

var errInputNotAllowed = errors.New("input source is not allowed")

type resolvedInput struct {
	Input     string
	Display   string
	SourceKey string
	Size      int64
	ModTime   time.Time
	Cleanup   func()
}

func (s *Server) resolveInput(ctx context.Context, inputPath, inputURL string, inputHeaders map[string]string) (resolvedInput, error) {
	inputPath = strings.TrimSpace(inputPath)
	inputURL = strings.TrimSpace(inputURL)
	if inputPath == "" && inputURL == "" {
		return resolvedInput{}, errors.New("input_path or input_url is required")
	}
	if inputPath != "" && inputURL != "" {
		return resolvedInput{}, errors.New("input_path and input_url are mutually exclusive")
	}
	if inputPath != "" {
		if err := s.validateInputPath(inputPath); err != nil {
			return resolvedInput{}, fmt.Errorf("%w: %v", errInputNotAllowed, err)
		}
		return resolvedInput{Input: inputPath, Display: inputPath, SourceKey: "file|" + inputPath}, nil
	}

	u, err := url.Parse(inputURL)
	if err != nil {
		return resolvedInput{}, fmt.Errorf("parse input_url: %w", err)
	}
	if err := s.validateHTTPSourceURL(u); err != nil {
		return resolvedInput{}, err
	}
	headers := make(http.Header, len(inputHeaders))
	for name, value := range inputHeaders {
		name = strings.TrimSpace(name)
		if name == "" {
			return resolvedInput{}, errors.New("input_headers contains an empty header name")
		}
		if strings.ContainsAny(value, "\r\n") {
			return resolvedInput{}, fmt.Errorf("input header %q contains a newline", name)
		}
		headers.Set(name, value)
	}
	client := s.httpSourceClient()
	source, info, err := transcoder.NewHTTPSource(ctx, u.String(), transcoder.HTTPSourceOptions{Client: client, Headers: headers})
	if err != nil {
		return resolvedInput{}, err
	}
	input, cleanup, err := transcoder.RegisterReadSeekerFactory(source.Name, source.OpenReadSeeker)
	if err != nil {
		return resolvedInput{}, err
	}
	display := u.Scheme + "://" + u.Host + u.EscapedPath()
	if display == "" {
		display = u.Host
	}
	identity := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%s\x00%d\x00%d", info.URL, info.ETag, info.Size, info.LastModified.UnixNano())))
	return resolvedInput{
		Input:     input,
		Display:   display,
		SourceKey: "http|" + hex.EncodeToString(identity[:]),
		Size:      info.Size,
		ModTime:   info.LastModified,
		Cleanup:   cleanup,
	}, nil
}

func (s *Server) validateHTTPSourceURL(u *url.URL) error {
	if u == nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return errors.New("input_url must be an absolute http or https URL")
	}
	if u.User != nil {
		return errors.New("input_url must not contain user info")
	}
	if u.Fragment != "" {
		return errors.New("input_url must not contain a fragment")
	}
	if !hostMatchesAllowlist(u, s.httpAllowedHosts) {
		return fmt.Errorf("%w: HTTP host %q is not allowed", errInputNotAllowed, u.Hostname())
	}
	return nil
}

func (s *Server) httpSourceClient() *http.Client {
	client := *http.DefaultClient
	client.Timeout = s.timeout
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return errors.New("too many HTTP source redirects")
		}
		if err := s.validateHTTPSourceURL(req.URL); err != nil {
			return err
		}
		if len(via) > 0 && !strings.EqualFold(via[0].URL.Host, req.URL.Host) {
			req.Header.Del("Authorization")
			req.Header.Del("Cookie")
		}
		return nil
	}
	return &client
}

func cleanHTTPAllowedHosts(allowed []string) []string {
	out := make([]string, 0, len(allowed))
	seen := make(map[string]struct{}, len(allowed))
	for _, raw := range allowed {
		value := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(raw), "."))
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}

func hostMatchesAllowlist(u *url.URL, allowed []string) bool {
	if len(allowed) == 0 {
		return false
	}
	hostname := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	hostPort := strings.ToLower(strings.TrimSuffix(u.Host, "."))
	for _, raw := range allowed {
		pattern := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(raw), "."))
		switch {
		case pattern == "*":
			return true
		case pattern == hostname || pattern == hostPort:
			return true
		case strings.HasPrefix(pattern, "*."):
			suffix := strings.TrimPrefix(pattern, "*")
			if strings.HasSuffix(hostname, suffix) && hostname != strings.TrimPrefix(suffix, ".") {
				return true
			}
		}
	}
	return false
}

func inputErrorStatus(err error) int {
	if errors.Is(err, errInputNotAllowed) {
		return http.StatusForbidden
	}
	return http.StatusBadRequest
}
