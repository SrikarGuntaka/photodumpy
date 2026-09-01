package config

import (
	"log/slog"
	"strings"
	"testing"
	"time"
)

// setEnv sets vars for one test and restores the environment afterwards.
func setEnv(t *testing.T, kv map[string]string) {
	t.Helper()
	// Clear everything Load reads so a developer's own shell environment cannot
	// change the result of a test.
	for _, k := range []string{
		"ENV", "LOG_LEVEL", "DATABASE_URL", "DB_MAX_CONNS", "DB_CONNECT_TIMEOUT",
		"HTTP_ADDR", "SHUTDOWN_GRACE", "PHOTO_ROOT", "THUMBNAIL_DIR", "API_BASE_URL",
	} {
		t.Setenv(k, "")
	}
	for k, v := range kv {
		t.Setenv(k, v)
	}
}

func TestLoadDefaults(t *testing.T) {
	setEnv(t, map[string]string{"DATABASE_URL": "postgres://x/y"})

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if cfg.HTTPAddr != ":8080" {
		t.Errorf("HTTPAddr = %q, want :8080", cfg.HTTPAddr)
	}
	if cfg.DBMaxConns != 10 {
		t.Errorf("DBMaxConns = %d, want 10", cfg.DBMaxConns)
	}
	if cfg.LogLevel != slog.LevelInfo {
		t.Errorf("LogLevel = %v, want info", cfg.LogLevel)
	}
	if cfg.ShutdownGrace != 30*time.Second {
		t.Errorf("ShutdownGrace = %v, want 30s", cfg.ShutdownGrace)
	}
}

func TestLoadRequiresDatabaseURL(t *testing.T) {
	setEnv(t, nil)

	if _, err := Load(); err == nil {
		t.Fatal("Load() succeeded without DATABASE_URL; want an error")
	} else if !strings.Contains(err.Error(), "DATABASE_URL") {
		t.Errorf("error = %v, want it to name DATABASE_URL", err)
	}
}

// A misconfiguration must surface as an error, never as a silently substituted
// default -- that is the failure mode this whole loader exists to prevent.
func TestLoadRejectsMalformedValues(t *testing.T) {
	tests := []struct {
		name    string
		env     map[string]string
		wantSub string
	}{
		{
			name:    "non-numeric max conns",
			env:     map[string]string{"DATABASE_URL": "postgres://x/y", "DB_MAX_CONNS": "many"},
			wantSub: "DB_MAX_CONNS",
		},
		{
			name:    "zero max conns",
			env:     map[string]string{"DATABASE_URL": "postgres://x/y", "DB_MAX_CONNS": "0"},
			wantSub: "DB_MAX_CONNS",
		},
		{
			name:    "bad duration",
			env:     map[string]string{"DATABASE_URL": "postgres://x/y", "SHUTDOWN_GRACE": "30 seconds"},
			wantSub: "SHUTDOWN_GRACE",
		},
		{
			name:    "negative duration",
			env:     map[string]string{"DATABASE_URL": "postgres://x/y", "DB_CONNECT_TIMEOUT": "-5s"},
			wantSub: "DB_CONNECT_TIMEOUT",
		},
		{
			name:    "unknown log level",
			env:     map[string]string{"DATABASE_URL": "postgres://x/y", "LOG_LEVEL": "verbose"},
			wantSub: "LOG_LEVEL",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			setEnv(t, tc.env)
			_, err := Load()
			if err == nil {
				t.Fatalf("Load() succeeded; want an error mentioning %s", tc.wantSub)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("error = %v, want it to mention %s", err, tc.wantSub)
			}
		})
	}
}

// Every bad value should be reported, not just the first one found.
func TestLoadReportsAllErrors(t *testing.T) {
	setEnv(t, map[string]string{
		"DB_MAX_CONNS":   "lots",
		"SHUTDOWN_GRACE": "soon",
	})

	_, err := Load()
	if err == nil {
		t.Fatal("Load() succeeded; want errors")
	}
	msg := err.Error()
	for _, want := range []string{"DATABASE_URL", "DB_MAX_CONNS", "SHUTDOWN_GRACE"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q does not mention %s", msg, want)
		}
	}
}

// PhotoRoot is a security boundary; it must be stored absolute and clean so
// later containment checks compare like with like.
func TestPhotoRootIsAbsoluteAndClean(t *testing.T) {
	dir := t.TempDir()
	setEnv(t, map[string]string{
		"DATABASE_URL": "postgres://x/y",
		"PHOTO_ROOT":   dir + "/sub/../",
	})

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if strings.Contains(cfg.PhotoRoot, "..") {
		t.Errorf("PhotoRoot = %q, want it cleaned of ..", cfg.PhotoRoot)
	}
	if strings.HasSuffix(cfg.PhotoRoot, "/") || strings.HasSuffix(cfg.PhotoRoot, `\`) {
		t.Errorf("PhotoRoot = %q, want no trailing separator", cfg.PhotoRoot)
	}
}

func TestParseLevel(t *testing.T) {
	tests := map[string]slog.Level{
		"debug":   slog.LevelDebug,
		"INFO":    slog.LevelInfo,
		"warn":    slog.LevelWarn,
		"warning": slog.LevelWarn,
		" error ": slog.LevelError,
		"":        slog.LevelInfo,
	}
	for in, want := range tests {
		got, err := parseLevel(in)
		if err != nil {
			t.Errorf("parseLevel(%q) error = %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("parseLevel(%q) = %v, want %v", in, got, want)
		}
	}
	if _, err := parseLevel("chatty"); err == nil {
		t.Error("parseLevel(\"chatty\") succeeded; want an error")
	}
}
