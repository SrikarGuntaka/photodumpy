// Package config loads and validates process configuration from the
// environment. Configuration is read exactly once, at startup, and any invalid
// value is a fatal error -- a process that starts with a nonsensical setting and
// misbehaves an hour later is far harder to debug than one that refuses to boot.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Config is the fully-resolved configuration for any of the binaries in this
// project. All three (api, worker, cli) share one struct: the set of knobs is
// small, and a single struct means a single place to look when something is
// configured oddly.
type Config struct {
	// Env is "development" or "production". It only affects log formatting.
	Env string
	// LogLevel is one of debug, info, warn, error.
	LogLevel slog.Level

	// DatabaseURL is a libpq-style connection string.
	DatabaseURL string
	// DBMaxConns bounds the connection pool. See DESIGN_DECISIONS.md -- this is
	// one of the three places concurrency is bounded.
	DBMaxConns int32
	// DBConnectTimeout bounds how long we wait for Postgres to become reachable
	// at startup. Postgres in Compose can take a few seconds to accept
	// connections even after the container is up, and a restarting database
	// should not kill the API.
	DBConnectTimeout time.Duration

	// HTTPAddr is the listen address for the API server, e.g. ":8080".
	HTTPAddr string
	// ShutdownGrace bounds how long we wait for in-flight work to finish after
	// a SIGTERM before giving up.
	ShutdownGrace time.Duration

	// PhotoRoot is the ONLY directory tree the process is permitted to read
	// photos from. Every library path is validated against it. This is a
	// security boundary, not a convenience: without it, the API would happily
	// scan any path a caller sends it.
	PhotoRoot string
	// ThumbnailDir is where generated thumbnails are written. It is the only
	// directory the process ever writes image data to; originals are never
	// touched.
	ThumbnailDir string

	// ProcessConcurrency bounds how many photos are decoded simultaneously
	// during metadata extraction. Zero means runtime.NumCPU(). This is one of
	// the three places concurrency is bounded -- see DESIGN_DECISIONS.md.
	ProcessConcurrency int

	// SimilarityThreshold is the Hamming distance below which two photos count
	// as near-duplicates. Zero means the calibrated default.
	SimilarityThreshold int

	// ClusterMaxGap is the silence between consecutive photos that ends an
	// event. Zero means the calibrated default.
	ClusterMaxGap time.Duration

	// ClusterMaxRadiusMeters is how far a photo may sit from its event's
	// anchor before it starts a new event. Zero means the calibrated default.
	ClusterMaxRadiusMeters float64

	// APIBaseURL is used by the CLI to reach the API.
	APIBaseURL string
}

// Load reads configuration from the environment and validates it.
func Load() (*Config, error) {
	l := &loader{}
	cfg := &Config{
		Env:                    l.String("ENV", "development"),
		DatabaseURL:            l.String("DATABASE_URL", ""),
		DBMaxConns:             int32(l.Int("DB_MAX_CONNS", 10)),
		DBConnectTimeout:       l.Duration("DB_CONNECT_TIMEOUT", 30*time.Second),
		HTTPAddr:               l.String("HTTP_ADDR", ":8080"),
		ShutdownGrace:          l.Duration("SHUTDOWN_GRACE", 30*time.Second),
		PhotoRoot:              l.String("PHOTO_ROOT", "/photos"),
		ThumbnailDir:           l.String("THUMBNAIL_DIR", "/var/lib/photo-organizer/thumbnails"),
		ProcessConcurrency:     l.Int("PROCESS_CONCURRENCY", 0),
		SimilarityThreshold:    l.Int("SIMILARITY_THRESHOLD", 0),
		ClusterMaxGap:          l.Duration("CLUSTER_MAX_GAP", 0),
		ClusterMaxRadiusMeters: l.Float("CLUSTER_MAX_RADIUS_METERS", 0),
		APIBaseURL:             l.String("API_BASE_URL", "http://localhost:8080"),
	}

	lvl, err := parseLevel(l.String("LOG_LEVEL", "info"))
	if err != nil {
		l.errs = append(l.errs, err)
	}
	cfg.LogLevel = lvl

	if cfg.DatabaseURL == "" {
		l.errs = append(l.errs, fmt.Errorf("config: DATABASE_URL is required"))
	}
	if cfg.DBMaxConns < 1 {
		l.errs = append(l.errs, fmt.Errorf("config: DB_MAX_CONNS must be >= 1, got %d", cfg.DBMaxConns))
	}
	if cfg.HTTPAddr == "" {
		l.errs = append(l.errs, fmt.Errorf("config: HTTP_ADDR must not be empty"))
	}
	if cfg.SimilarityThreshold < 0 {
		l.errs = append(l.errs, fmt.Errorf("config: SIMILARITY_THRESHOLD must be >= 0, got %d", cfg.SimilarityThreshold))
	}
	if cfg.ClusterMaxGap < 0 {
		l.errs = append(l.errs, fmt.Errorf("config: CLUSTER_MAX_GAP must be >= 0, got %s", cfg.ClusterMaxGap))
	}
	if cfg.ClusterMaxRadiusMeters < 0 {
		l.errs = append(l.errs, fmt.Errorf("config: CLUSTER_MAX_RADIUS_METERS must be >= 0, got %v", cfg.ClusterMaxRadiusMeters))
	}
	if cfg.ProcessConcurrency < 0 {
		l.errs = append(l.errs, fmt.Errorf("config: PROCESS_CONCURRENCY must be >= 0, got %d", cfg.ProcessConcurrency))
	}
	if len(l.errs) > 0 {
		return nil, errors.Join(l.errs...)
	}

	// Resolve PhotoRoot to an absolute, symlink-free path once, at startup.
	// Every later path check compares against this resolved value, so a symlink
	// planted inside the root cannot be used to escape it.
	if cfg.PhotoRoot != "" {
		abs, err := filepath.Abs(cfg.PhotoRoot)
		if err != nil {
			return nil, fmt.Errorf("config: PHOTO_ROOT %q is not resolvable: %w", cfg.PhotoRoot, err)
		}
		// EvalSymlinks fails if the directory does not exist yet. That is not
		// fatal at config time (the CLI does not need it), so fall back to the
		// absolute path and let the code that actually reads photos report the
		// missing directory with proper context.
		if resolved, err := filepath.EvalSymlinks(abs); err == nil {
			abs = resolved
		}
		cfg.PhotoRoot = filepath.Clean(abs)
	}

	return cfg, nil
}

// NewLogger builds the structured logger for a process. Development gets
// human-readable text; anything else gets JSON so logs stay machine-parseable.
func (c *Config) NewLogger(component string) *slog.Logger {
	opts := &slog.HandlerOptions{Level: c.LogLevel}

	var h slog.Handler
	if c.Env == "development" {
		h = slog.NewTextHandler(os.Stdout, opts)
	} else {
		h = slog.NewJSONHandler(os.Stdout, opts)
	}
	return slog.New(h).With("component", component)
}

func parseLevel(s string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug, nil
	case "info", "":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("config: LOG_LEVEL %q is not one of debug|info|warn|error", s)
	}
}

// loader collects parse errors so Load can report every bad environment
// variable at once instead of failing on whichever it happened to read first.
type loader struct{ errs []error }

func (l *loader) String(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

func (l *loader) Int(key string, def int) int {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		l.errs = append(l.errs, fmt.Errorf("config: %s=%q is not an integer", key, v))
		return def
	}
	return n
}

func (l *loader) Float(key string, def float64) float64 {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		l.errs = append(l.errs, fmt.Errorf("config: %s=%q is not a number", key, v))
		return def
	}
	return f
}

func (l *loader) Duration(key string, def time.Duration) time.Duration {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		l.errs = append(l.errs, fmt.Errorf("config: %s=%q is not a duration (try 30s, 5m)", key, v))
		return def
	}
	if d <= 0 {
		l.errs = append(l.errs, fmt.Errorf("config: %s=%q must be positive", key, v))
		return def
	}
	return d
}
