package api

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/srikarguntaka/photo-organizer/internal/config"
)

// webFixture lays out a minimal built UI, plus a secret file OUTSIDE the web
// directory that no request should ever be able to reach.
func webFixture(t *testing.T) (webDir string) {
	t.Helper()
	base := t.TempDir()
	webDir = filepath.Join(base, "web")

	files := map[string]string{
		"index.html":           "<!doctype html><title>app</title>",
		"assets/index-abc.js":  "console.log('app')",
		"assets/index-abc.css": "body{}",
		"favicon.svg":          "<svg/>",
	}
	for name, body := range files {
		p := filepath.Join(webDir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(base, "secret.txt"), []byte("SECRET"), 0o644); err != nil {
		t.Fatal(err)
	}
	return webDir
}

func get(t *testing.T, h http.Handler, target string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	// Set the path directly rather than via NewRequest, which would normalise
	// away the very traversal sequences these tests need to send.
	req.URL.Path = target
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestWebServesRealFiles(t *testing.T) {
	h := webHandler(webFixture(t))

	rec := get(t, h, "/assets/index-abc.js")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "console.log") {
		t.Fatalf("asset: %d %q", rec.Code, rec.Body.String())
	}
	// Hashed assets never change at a given URL, so they may be cached forever.
	if cc := rec.Header().Get("Cache-Control"); !strings.Contains(cc, "immutable") {
		t.Errorf("hashed asset Cache-Control = %q, want immutable", cc)
	}
}

func TestWebClientRoutesFallBackToIndex(t *testing.T) {
	h := webHandler(webFixture(t))

	for _, route := range []string{"/", "/libraries/abc", "/libraries/abc/review", "/photos"} {
		rec := get(t, h, route)
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "<title>app</title>") {
			t.Errorf("%s: %d %q, want index.html", route, rec.Code, rec.Body.String())
		}
		// index.html names the current build's assets; a cached copy would
		// keep loading an old build.
		if cc := rec.Header().Get("Cache-Control"); cc != "no-cache" {
			t.Errorf("%s: Cache-Control = %q, want no-cache", route, cc)
		}
	}
}

// The classic SPA bug: a missing JS file answered with index.html. The browser
// then reports a MIME error nowhere near the real cause.
func TestWebMissingAssetIs404NotIndex(t *testing.T) {
	h := webHandler(webFixture(t))

	for _, asset := range []string{"/assets/index-OLDBUILD.js", "/missing.css", "/logo.png"} {
		rec := get(t, h, asset)
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s: status %d, want 404", asset, rec.Code)
		}
		if strings.Contains(rec.Body.String(), "<title>app</title>") {
			t.Errorf("%s: served index.html in place of a missing asset", asset)
		}
	}
}

func TestWebCannotEscapeItsDirectory(t *testing.T) {
	h := webHandler(webFixture(t))

	for _, attack := range []string{
		"/../secret.txt",
		"/assets/../../secret.txt",
		"/..%2fsecret.txt",
		`/..\secret.txt`,
		"/./../secret.txt",
	} {
		rec := get(t, h, attack)
		if strings.Contains(rec.Body.String(), "SECRET") {
			t.Errorf("%s: leaked a file outside the web directory", attack)
		}
	}
}

func TestWebSetsSecurityHeaders(t *testing.T) {
	h := webHandler(webFixture(t))
	rec := get(t, h, "/")

	csp := rec.Header().Get("Content-Security-Policy")
	for _, directive := range []string{"default-src 'self'", "frame-ancestors 'none'", "object-src 'none'"} {
		if !strings.Contains(csp, directive) {
			t.Errorf("CSP %q is missing %q", csp, directive)
		}
	}
	// A strict CSP that quietly allowed inline script would be decorative.
	if strings.Contains(csp, "unsafe-inline") || strings.Contains(csp, "unsafe-eval") {
		t.Errorf("CSP permits unsafe script: %q", csp)
	}
	for header, want := range map[string]string{
		"X-Frame-Options":        "DENY",
		"X-Content-Type-Options": "nosniff",
		"Referrer-Policy":        "no-referrer",
	} {
		if got := rec.Header().Get(header); got != want {
			t.Errorf("%s = %q, want %q", header, got, want)
		}
	}
}

func TestWebRejectsWrites(t *testing.T) {
	h := webHandler(webFixture(t))
	req := httptest.NewRequest(http.MethodPost, "/libraries", strings.NewReader("x"))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST to a UI route: %d, want 405", rec.Code)
	}
}

// Through the real mux: an unknown API path must be JSON, never the UI, and
// real API routes must still win over the catch-all.
func TestUnknownAPIRouteIsJSONNotTheUI(t *testing.T) {
	cfg := &config.Config{WebDir: webFixture(t)}
	s := &Server{cfg: cfg, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	h := s.Handler()

	rec := get(t, h, "/api/definitely-not-an-endpoint")
	if rec.Code != http.StatusNotFound {
		t.Errorf("status %d, want 404", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type %q, want JSON", ct)
	}
	if strings.Contains(rec.Body.String(), "<title>app</title>") {
		t.Error("an unknown API route served the UI's index.html")
	}

	// And the UI is still reachable alongside it.
	if rec := get(t, h, "/libraries/abc"); !strings.Contains(rec.Body.String(), "<title>app</title>") {
		t.Errorf("UI route not served through the full mux: %d", rec.Code)
	}
}

func TestNoWebDirMeansNoUI(t *testing.T) {
	s := &Server{cfg: &config.Config{}, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	rec := get(t, s.Handler(), "/libraries/abc")
	if rec.Code != http.StatusNotFound {
		t.Errorf("with no WEB_DIR, a UI route returned %d, want 404", rec.Code)
	}
}
