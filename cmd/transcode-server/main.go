package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"media-transcoder/server"

	"github.com/spf13/pflag"
)

const version = "0.2.0"

func main() {
	var addr string
	var timeout time.Duration
	var showVersion bool
	var apiKeys string
	var rateLimit int
	var maxJobs int
	var cacheRoot string
	var allowedRoots []string
	var debug bool
	var corsOrigins string
	var corsCredentials bool
	var configPath string
	pflag.StringVar(&configPath, "config", "", "YAML config file with libraries/profiles/server defaults")
	pflag.StringVar(&addr, "addr", ":8080", "HTTP listen address")
	pflag.DurationVar(&timeout, "request-timeout", 30*time.Minute, "per-request timeout")
	pflag.StringVar(&apiKeys, "api-keys", "", "comma-separated API keys; empty disables auth")
	pflag.IntVar(&rateLimit, "rate-limit", 0, "requests per minute per remote address; 0 disables rate limit")
	pflag.IntVar(&maxJobs, "max-jobs", 4, "maximum concurrent asynchronous transcode jobs")
	pflag.StringVar(&cacheRoot, "cache-root", "", "server-owned cache root; client cache_dir is ignored when set")
	pflag.StringArrayVar(&allowedRoots, "allow-input-root", nil, "allowed input root; repeat to allow multiple roots; empty allows any path")
	pflag.BoolVar(&debug, "debug", false, "enable debug logging")
	pflag.StringVar(&corsOrigins, "cors-origins", "*", "comma-separated CORS allowed origins; use * for public playback")
	pflag.BoolVar(&corsCredentials, "cors-credentials", false, "allow credentialed CORS requests; do not use with wildcard origins")
	pflag.BoolVarP(&showVersion, "version", "v", false, "print version and exit")
	pflag.Parse()
	if showVersion {
		fmt.Println(version)
		return
	}

	keys := splitCSV(apiKeys)
	cfg := server.Config{RequestTimeout: timeout, APIKeys: keys, RateLimitPerMinute: rateLimit, MaxConcurrentJobs: maxJobs, CacheRoot: cacheRoot, AllowedInputRoots: allowedRoots, CORS: server.CORSConfig{AllowedOrigins: splitCSVDefault(corsOrigins, []string{"*"}), AllowCredentials: corsCredentials}}
	if configPath != "" {
		fileCfg, err := server.LoadConfigFile(configPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "load config: %v\n", err)
			os.Exit(1)
		}
		raw, _ := server.LoadPlaybackConfig(configPath)
		cfg = fileCfg
		if raw != nil {
			if !pflag.CommandLine.Changed("addr") && raw.Server.Addr != "" {
				addr = raw.Server.Addr
			}
			if !pflag.CommandLine.Changed("debug") {
				debug = raw.Server.Debug
			}
		}
		if pflag.CommandLine.Changed("request-timeout") {
			cfg.RequestTimeout = timeout
		}
		if pflag.CommandLine.Changed("api-keys") {
			cfg.APIKeys = keys
		}
		if pflag.CommandLine.Changed("rate-limit") {
			cfg.RateLimitPerMinute = rateLimit
		}
		if pflag.CommandLine.Changed("max-jobs") {
			cfg.MaxConcurrentJobs = maxJobs
		}
		if pflag.CommandLine.Changed("cache-root") {
			cfg.CacheRoot = cacheRoot
		}
		if pflag.CommandLine.Changed("allow-input-root") {
			cfg.AllowedInputRoots = allowedRoots
		}
		if pflag.CommandLine.Changed("cors-origins") {
			cfg.CORS.AllowedOrigins = splitCSVDefault(corsOrigins, []string{"*"})
		}
		if pflag.CommandLine.Changed("cors-credentials") {
			cfg.CORS.AllowCredentials = corsCredentials
		}
	}
	level := slog.LevelInfo
	if debug {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	cfg.Logger = log
	app := server.New(cfg)
	h := app.Handler()
	srv := &http.Server{Addr: addr, Handler: h, ReadHeaderTimeout: 5 * time.Second}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	errc := make(chan error, 1)
	go func() { log.Info("transcode server listening", "addr", addr); errc <- srv.ListenAndServe() }()
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
		app.Close()
	case err := <-errc:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("server failed", "err", err)
			os.Exit(1)
		}
	}
}

func splitCSV(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func splitCSVDefault(s string, fallback []string) []string {
	out := splitCSV(s)
	if len(out) == 0 {
		return fallback
	}
	return out
}
