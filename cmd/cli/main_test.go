package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
)

// fakeAPI stands in for the real server so CLI argument handling can be tested
// without Postgres. It records what was requested.
type fakeAPI struct {
	server    *httptest.Server
	scanCalls atomic.Int32
	getCalls  atomic.Int32
	// scanState is what GET /api/libraries/{id} reports.
	scanState atomic.Value // string
	// lastLimit/lastOffset record the pagination the CLI actually sent, so a
	// test can assert the flags reached the request rather than merely that
	// the command exited zero.
	lastLimit  atomic.Int32
	lastOffset atomic.Int32
}

func atoiOr(s string, def int) int {
	n, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return n
}

func newFakeAPI(t *testing.T) *fakeAPI {
	t.Helper()
	f := &fakeAPI{}
	f.scanState.Store("complete")

	mux := http.NewServeMux()

	mux.HandleFunc("POST /api/libraries", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "lib-123", "name": "test", "root_path": "/photos",
			"source_kind": "local_fs", "scan_state": "never_scanned",
		})
	})

	mux.HandleFunc("POST /api/libraries/{id}/scan", func(w http.ResponseWriter, r *http.Request) {
		f.scanCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(map[string]any{"library_id": "lib-123", "status": "scanning"})
	})

	mux.HandleFunc("GET /api/libraries/{id}/photos", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"photos": []map[string]any{
				{"id": "p1", "relative_path": "a.jpg", "original_filename": "a.jpg",
					"file_size_bytes": 1024, "detected_format": "jpeg", "state": "discovered"},
			},
			"total": 1,
			// Echo back as integers, matching the real API's shape. Query
			// values are strings, so they must be converted -- returning the
			// raw strings makes the CLI fail to decode, which is what the
			// first version of this fake did.
			"limit":  atoiOr(r.URL.Query().Get("limit"), 0),
			"offset": atoiOr(r.URL.Query().Get("offset"), 0),
		})
		f.lastLimit.Store(int32(atoiOr(r.URL.Query().Get("limit"), 0)))
		f.lastOffset.Store(int32(atoiOr(r.URL.Query().Get("offset"), 0)))
	})

	mux.HandleFunc("GET /api/libraries/{id}", func(w http.ResponseWriter, r *http.Request) {
		f.getCalls.Add(1)
		count := 42
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"library": map[string]any{
				"id": "lib-123", "name": "test", "root_path": "/photos",
				"scan_state": f.scanState.Load().(string), "photo_count": count,
			},
			"photos_by_state": map[string]int{"discovered": count},
			"scanning_here":   false,
		})
	})

	f.server = httptest.NewServer(mux)
	t.Cleanup(f.server.Close)
	return f
}

// Go's flag package stops parsing at the first non-flag argument. With a single
// FlagSet, "scan <path> -wait" silently ignores -wait -- the flag becomes a
// positional argument and the command returns immediately instead of polling.
// This is the regression test for that.
func TestScanFlagAfterPositionalArgumentIsHonoured(t *testing.T) {
	api := newFakeAPI(t)

	var out bytes.Buffer
	err := run([]string{"-api", api.server.URL, "scan", "/photos", "-wait"}, &out)
	if err != nil {
		t.Fatalf("run() error = %v", err)
	}

	if api.getCalls.Load() == 0 {
		t.Error("-wait after the path was ignored: the CLI never polled for progress")
	}
	if !strings.Contains(out.String(), "Scan complete") {
		t.Errorf("output did not report completion, so -wait did not take effect:\n%s", out.String())
	}
}

// The other ordering must keep working too.
func TestScanFlagBeforeSubcommandIsHonoured(t *testing.T) {
	api := newFakeAPI(t)

	var out bytes.Buffer
	if err := run([]string{"-api", api.server.URL, "-wait", "scan", "/photos"}, &out); err != nil {
		t.Fatalf("run() error = %v", err)
	}
	if api.getCalls.Load() == 0 {
		t.Error("-wait before the subcommand was ignored")
	}
}

// Without -wait the command must return immediately and not poll.
func TestScanWithoutWaitDoesNotPoll(t *testing.T) {
	api := newFakeAPI(t)

	var out bytes.Buffer
	if err := run([]string{"-api", api.server.URL, "scan", "/photos"}, &out); err != nil {
		t.Fatalf("run() error = %v", err)
	}
	if api.scanCalls.Load() != 1 {
		t.Errorf("scan endpoint called %d times, want 1", api.scanCalls.Load())
	}
	if !strings.Contains(out.String(), "Poll progress with") {
		t.Errorf("expected the non-waiting hint in output:\n%s", out.String())
	}
}

func TestPhotosPaginationFlagsAfterPositional(t *testing.T) {
	api := newFakeAPI(t)

	var out bytes.Buffer
	// -limit and -offset after the library id must be parsed, not swallowed.
	if err := run([]string{"-api", api.server.URL, "photos", "lib-123", "-limit", "5", "-offset", "10"}, &out); err != nil {
		t.Fatalf("run() error = %v", err)
	}
	if api.getCalls.Load() == 0 {
		t.Error("photos command did not query the library")
	}
	if got := api.lastLimit.Load(); got != 5 {
		t.Errorf("request used limit=%d, want 5 -- the flag after the positional was ignored", got)
	}
	if got := api.lastOffset.Load(); got != 10 {
		t.Errorf("request used offset=%d, want 10 -- the flag after the positional was ignored", got)
	}
}

func TestUnknownCommandIsAnError(t *testing.T) {
	var out bytes.Buffer
	err := run([]string{"frobnicate"}, &out)
	if err == nil {
		t.Fatal("unknown command was accepted")
	}
	if !strings.Contains(err.Error(), "frobnicate") {
		t.Errorf("error = %v, want it to name the bad command", err)
	}
}

func TestNoCommandIsAnError(t *testing.T) {
	var out bytes.Buffer
	if err := run(nil, &out); err == nil {
		t.Error("running with no command succeeded; want an error")
	}
}

func TestMissingRequiredArgumentIsAnError(t *testing.T) {
	tests := [][]string{
		{"scan"},
		{"photos"},
	}
	for _, args := range tests {
		var out bytes.Buffer
		if err := run(args, &out); err == nil {
			t.Errorf("run(%v) succeeded; want an error about the missing argument", args)
		}
	}
}

func TestVersionCommand(t *testing.T) {
	var out bytes.Buffer
	if err := run([]string{"version"}, &out); err != nil {
		t.Fatalf("run() error = %v", err)
	}
	if !strings.Contains(out.String(), "photo-organizer") {
		t.Errorf("version output = %q", out.String())
	}
}

// An API error must surface its message, not a bare status code.
func TestAPIErrorMessageIsSurfaced(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]string{
				"code":    "path_outside_root",
				"message": "path must be inside the configured photo root",
			},
		})
	}))
	t.Cleanup(srv.Close)

	var out bytes.Buffer
	err := run([]string{"-api", srv.URL, "scan", "/etc"}, &out)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "path must be inside the configured photo root") {
		t.Errorf("error = %v, want the API's message", err)
	}
	if !strings.Contains(err.Error(), "path_outside_root") {
		t.Errorf("error = %v, want the API's error code", err)
	}
}

func TestHumanBytes(t *testing.T) {
	cases := map[int64]string{
		0:       "0 B",
		512:     "512 B",
		1024:    "1.0 KB",
		1536:    "1.5 KB",
		1 << 20: "1.0 MB",
		1 << 30: "1.0 GB",
	}
	for in, want := range cases {
		if got := humanBytes(in); got != want {
			t.Errorf("humanBytes(%d) = %q, want %q", in, got, want)
		}
	}
}
