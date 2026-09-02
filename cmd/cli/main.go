// Command photo-organizer is the operator CLI.
//
// It is a thin HTTP client, not a second implementation. Everything it does
// goes through the same API the web UI uses, which means: no duplicated
// business logic, no second database connection to keep in sync, and it keeps
// working unchanged if the API ever moves off this machine.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

const usage = `photo-organizer -- local photo library organiser

Usage:
  photo-organizer [flags] <command> [args]

Commands:
  status                 Show API and database health
  libraries              List registered libraries
  scan <path>            Register a folder (if new) and scan it for photos
  photos <library-id>    List photos in a library
  version                Print the client version

Flags:
  -api string       Base URL of the API (default $API_BASE_URL or http://localhost:8080)
  -timeout dur      Request timeout (default 30s)
  -name string      Library name for 'scan' (default: the folder's name)
  -force            For 'scan': override a library stuck in the scanning state
  -wait             For 'scan': poll until the scan finishes
  -limit int        For 'photos': page size (default 20)
  -offset int       For 'photos': page offset

Paths given to 'scan' may be absolute or relative to the API's configured
photo root. The API refuses any path outside that root.
`

// version is overridden at build time with -ldflags "-X main.version=..."
var version = "dev"

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

// run parses arguments and dispatches to a subcommand.
//
// Go's flag package stops parsing at the first non-flag argument, so a naive
// single Parse() call silently ignores "scan ./photos -wait" -- the flag
// becomes a positional argument and the command returns instead of waiting.
// parseInterleaved below handles flags on either side of the positional ones,
// which is what anyone typing this actually expects.
func run(args []string, out io.Writer) error {
	defaultAPI := os.Getenv("API_BASE_URL")
	if defaultAPI == "" {
		defaultAPI = "http://localhost:8080"
	}

	fs := flag.NewFlagSet("photo-organizer", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() { fmt.Fprint(os.Stderr, usage) }

	apiBase := fs.String("api", defaultAPI, "base URL of the API")
	timeout := fs.Duration("timeout", 30*time.Second, "request timeout")
	name := fs.String("name", "", "library name for 'scan'")
	force := fs.Bool("force", false, "override a stuck scan")
	wait := fs.Bool("wait", false, "poll until the scan finishes")
	limit := fs.Int("limit", 20, "page size for 'photos'")
	offset := fs.Int("offset", 0, "page offset for 'photos'")

	positional, err := parseInterleaved(fs, args)
	if err != nil {
		return err
	}
	if len(positional) == 0 {
		fs.Usage()
		return errors.New("no command given")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	c := &client{base: strings.TrimRight(*apiBase, "/"), http: &http.Client{Timeout: *timeout}}

	cmd := positional[0]
	rest := positional[1:]

	switch cmd {
	case "status":
		return cmdStatus(ctx, c, out)
	case "libraries":
		return cmdLibraries(ctx, c, out)
	case "scan":
		if len(rest) < 1 {
			return errors.New("scan requires a path: photo-organizer scan <path>")
		}
		return cmdScan(ctx, c, out, rest[0], *name, *force, *wait)
	case "photos":
		if len(rest) < 1 {
			return errors.New("photos requires a library id: photo-organizer photos <library-id>")
		}
		return cmdPhotos(ctx, c, out, rest[0], *limit, *offset)
	case "version":
		fmt.Fprintf(out, "photo-organizer %s\n", version)
		return nil
	default:
		fs.Usage()
		return fmt.Errorf("unknown command %q", cmd)
	}
}

// parseInterleaved repeatedly parses flags, pulling out positional arguments as
// it encounters them, so flags and positionals may be given in any order.
//
// Each Parse call consumes flags until it hits a non-flag argument; that
// argument is collected and parsing resumes on what follows. Looping is what
// makes "-api X scan ./p -wait" and "scan ./p -wait -api X" equivalent.
func parseInterleaved(fs *flag.FlagSet, args []string) ([]string, error) {
	var positional []string
	remaining := args

	for {
		if err := fs.Parse(remaining); err != nil {
			return nil, err
		}
		if fs.NArg() == 0 {
			return positional, nil
		}
		positional = append(positional, fs.Arg(0))
		remaining = fs.Args()[1:]
	}
}

// --- HTTP client ----------------------------------------------------------

type client struct {
	base string
	http *http.Client
}

// do performs a request and decodes the JSON body into v. It returns the HTTP
// status alongside the error so callers can distinguish "the API said no" from
// "the API is unreachable" -- a distinction that matters a lot when the whole
// point of the command is diagnosing which one it is.
func (c *client) do(ctx context.Context, method, path string, body any, v any) (int, error) {
	var rdr io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return 0, err
		}
		rdr = bytes.NewReader(buf)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rdr)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return 0, fmt.Errorf("requesting %s: %w", c.base+path, err)
	}
	defer resp.Body.Close()

	// Cap the body: a misconfigured -api pointing at something that streams
	// forever should not exhaust memory.
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return resp.StatusCode, fmt.Errorf("reading %s: %w", path, err)
	}

	if resp.StatusCode >= 400 {
		return resp.StatusCode, apiErrorFrom(raw, resp.StatusCode)
	}
	if v != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, v); err != nil {
			return resp.StatusCode, fmt.Errorf("decoding %s (status %d): %w", path, resp.StatusCode, err)
		}
	}
	return resp.StatusCode, nil
}

func (c *client) get(ctx context.Context, path string, v any) (int, error) {
	return c.do(ctx, http.MethodGet, path, nil, v)
}

// apiErrorFrom turns the API's error envelope into a Go error, falling back to
// the raw body when the response is not the shape we expect (e.g. a proxy
// returned HTML).
func apiErrorFrom(raw []byte, status int) error {
	var env struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &env); err == nil && env.Error.Message != "" {
		return fmt.Errorf("%s (%s)", env.Error.Message, env.Error.Code)
	}
	snippet := strings.TrimSpace(string(raw))
	if len(snippet) > 200 {
		snippet = snippet[:200] + "..."
	}
	if snippet == "" {
		return fmt.Errorf("request failed with status %d", status)
	}
	return fmt.Errorf("request failed with status %d: %s", status, snippet)
}

// --- response types -------------------------------------------------------

type healthResponse struct {
	Status     string `json:"status"`
	UptimeSecs int64  `json:"uptime_seconds"`
}

type readyResponse struct {
	Status    string `json:"status"`
	Database  string `json:"database"`
	LatencyMS int64  `json:"database_latency_ms"`
	Detail    string `json:"detail"`
}

type library struct {
	ID                 string     `json:"id"`
	Name               string     `json:"name"`
	SourceKind         string     `json:"source_kind"`
	RootPath           string     `json:"root_path"`
	ScanState          string     `json:"scan_state"`
	PhotoCount         *int       `json:"photo_count"`
	LastScanStartedAt  *time.Time `json:"last_scan_started_at"`
	LastScanFinishedAt *time.Time `json:"last_scan_finished_at"`
}

type librariesResponse struct {
	Libraries []library `json:"libraries"`
}

type libraryDetailResponse struct {
	Library       library        `json:"library"`
	PhotosByState map[string]int `json:"photos_by_state"`
	ScanningHere  bool           `json:"scanning_here"`
}

type photo struct {
	ID               string  `json:"id"`
	RelativePath     string  `json:"relative_path"`
	OriginalFilename string  `json:"original_filename"`
	FileSizeBytes    int64   `json:"file_size_bytes"`
	DetectedFormat   *string `json:"detected_format"`
	State            string  `json:"state"`
}

type photosResponse struct {
	Photos []photo `json:"photos"`
	Total  int     `json:"total"`
	Limit  int     `json:"limit"`
	Offset int     `json:"offset"`
}

// --- commands -------------------------------------------------------------

func cmdStatus(ctx context.Context, c *client, out io.Writer) error {
	fmt.Fprintf(out, "API      %s\n", c.base)

	var health healthResponse
	if _, err := c.get(ctx, "/healthz", &health); err != nil {
		fmt.Fprintf(out, "Process  UNREACHABLE\n")
		return err
	}
	fmt.Fprintf(out, "Process  %s (up %s)\n", strings.ToUpper(health.Status), formatUptime(health.UptimeSecs))

	var ready readyResponse
	status, err := c.get(ctx, "/readyz", &ready)
	if err != nil && status == 0 {
		fmt.Fprintf(out, "Database UNKNOWN\n")
		return err
	}
	if status != http.StatusOK {
		fmt.Fprintf(out, "Database DOWN\n")
		return errors.New("API is not ready")
	}
	fmt.Fprintf(out, "Database UP (%dms)\n", ready.LatencyMS)

	var libs librariesResponse
	if _, err := c.get(ctx, "/api/libraries", &libs); err == nil {
		fmt.Fprintf(out, "Libraries %d\n", len(libs.Libraries))
	}
	return nil
}

func cmdLibraries(ctx context.Context, c *client, out io.Writer) error {
	var resp librariesResponse
	if _, err := c.get(ctx, "/api/libraries", &resp); err != nil {
		return err
	}

	if len(resp.Libraries) == 0 {
		fmt.Fprintln(out, "No libraries yet. Create one with:  photo-organizer scan <path>")
		return nil
	}

	fmt.Fprintf(out, "%-38s  %-20s  %-14s  %s\n", "ID", "NAME", "SCAN STATE", "ROOT")
	for _, l := range resp.Libraries {
		fmt.Fprintf(out, "%-38s  %-20s  %-14s  %s\n",
			l.ID, truncate(l.Name, 20), l.ScanState, l.RootPath)
	}
	return nil
}

// cmdScan registers the folder (idempotently) and starts a scan.
//
// Combining register+scan into one command matches how the tool is actually
// used -- "organise this folder" -- and it is safe precisely because both
// halves are idempotent.
func cmdScan(ctx context.Context, c *client, out io.Writer, path, name string, force, wait bool) error {
	var lib library
	status, err := c.do(ctx, http.MethodPost, "/api/libraries",
		map[string]string{"path": path, "name": name}, &lib)
	if err != nil {
		return err
	}
	if status == http.StatusCreated {
		fmt.Fprintf(out, "Created library %s\n", lib.ID)
	} else {
		fmt.Fprintf(out, "Using existing library %s\n", lib.ID)
	}
	fmt.Fprintf(out, "Root  %s\n\n", lib.RootPath)

	scanPath := "/api/libraries/" + lib.ID + "/scan"
	if force {
		scanPath += "?force=true"
	}
	if _, err := c.do(ctx, http.MethodPost, scanPath, nil, nil); err != nil {
		return err
	}
	fmt.Fprintln(out, "Scan started.")

	if !wait {
		fmt.Fprintf(out, "\nPoll progress with:  photo-organizer photos %s\n", lib.ID)
		return nil
	}

	return pollScan(ctx, c, out, lib.ID)
}

// pollScan waits for a scan to finish, reporting progress as it goes.
//
// Polls the database-backed count rather than streaming from the scanner:
// the count is the real state, and it stays correct even if the scan is
// running in a different process than the one being polled.
func pollScan(ctx context.Context, c *client, out io.Writer, libraryID string) error {
	const interval = 500 * time.Millisecond
	lastCount := -1

	for {
		var detail libraryDetailResponse
		if _, err := c.get(ctx, "/api/libraries/"+libraryID, &detail); err != nil {
			return err
		}

		count := 0
		if detail.Library.PhotoCount != nil {
			count = *detail.Library.PhotoCount
		}
		if count != lastCount {
			fmt.Fprintf(out, "\r  %d photos discovered...", count)
			lastCount = count
		}

		if detail.Library.ScanState == "complete" && !detail.ScanningHere {
			fmt.Fprintf(out, "\r  %d photos discovered. Scan complete.\n", count)
			return nil
		}

		select {
		case <-ctx.Done():
			fmt.Fprintln(out)
			return ctx.Err()
		case <-time.After(interval):
		}
	}
}

func cmdPhotos(ctx context.Context, c *client, out io.Writer, libraryID string, limit, offset int) error {
	var detail libraryDetailResponse
	if _, err := c.get(ctx, "/api/libraries/"+libraryID, &detail); err != nil {
		return err
	}

	fmt.Fprintf(out, "Library    %s\n", detail.Library.Name)
	fmt.Fprintf(out, "Root       %s\n", detail.Library.RootPath)
	fmt.Fprintf(out, "Scan       %s\n", detail.Library.ScanState)
	if detail.Library.PhotoCount != nil {
		fmt.Fprintf(out, "Photos     %d\n", *detail.Library.PhotoCount)
	}
	if len(detail.PhotosByState) > 0 {
		fmt.Fprintf(out, "By state   ")
		first := true
		for _, st := range []string{"discovered", "processing", "ready", "missing", "failed"} {
			if n, ok := detail.PhotosByState[st]; ok {
				if !first {
					fmt.Fprintf(out, ", ")
				}
				fmt.Fprintf(out, "%s=%d", st, n)
				first = false
			}
		}
		fmt.Fprintln(out)
	}
	fmt.Fprintln(out)

	var resp photosResponse
	url := fmt.Sprintf("/api/libraries/%s/photos?limit=%d&offset=%d", libraryID, limit, offset)
	if _, err := c.get(ctx, url, &resp); err != nil {
		return err
	}

	if len(resp.Photos) == 0 {
		fmt.Fprintln(out, "No photos. Run a scan first:  photo-organizer scan <path>")
		return nil
	}

	fmt.Fprintf(out, "%-10s  %-8s  %-12s  %s\n", "SIZE", "FORMAT", "STATE", "PATH")
	for _, p := range resp.Photos {
		format := "-"
		if p.DetectedFormat != nil {
			format = *p.DetectedFormat
		}
		fmt.Fprintf(out, "%-10s  %-8s  %-12s  %s\n",
			humanBytes(p.FileSizeBytes), format, p.State, p.RelativePath)
	}

	shown := offset + len(resp.Photos)
	fmt.Fprintf(out, "\nShowing %d-%d of %d\n", offset+1, shown, resp.Total)
	if shown < resp.Total {
		fmt.Fprintf(out, "Next page:  photo-organizer photos %s -offset %d\n", libraryID, shown)
	}
	return nil
}

// --- formatting -----------------------------------------------------------

func formatUptime(secs int64) string {
	d := time.Duration(secs) * time.Second
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", secs)
	case d < time.Hour:
		return fmt.Sprintf("%dm%ds", secs/60, secs%60)
	default:
		return fmt.Sprintf("%dh%dm", secs/3600, (secs%3600)/60)
	}
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for x := n / unit; x >= unit; x /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	if max <= 3 {
		return s[:max]
	}
	return s[:max-3] + "..."
}
