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
	"sort"
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
  process <library-id>   Extract metadata (EXIF, GPS, dimensions) for a library
  hash <library-id>      Compute SHA-256 hashes and find exact duplicates
  duplicates <lib-id>    Show exact-duplicate groups (suggestions only)
  similar <library-id>   Show near-duplicate groups (suggestions only)
  queue <library-id>     Enqueue the pipeline as jobs for the worker pool
  jobs <library-id>      Show queue state
  workers                Show the worker fleet
  photos <library-id>    List photos in a library
  version                Print the client version

Flags:
  -api string       Base URL of the API (default $API_BASE_URL or http://localhost:8080)
  -timeout dur      Request timeout (default 30s)
  -name string      Library name for 'scan' (default: the folder's name)
  -force            For 'scan': override a library stuck in the scanning state
  -wait             For 'scan'/'process': poll until the work finishes
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
	case "process":
		if len(rest) < 1 {
			return errors.New("process requires a library id: photo-organizer process <library-id>")
		}
		return cmdProcess(ctx, c, out, rest[0], *wait)
	case "hash":
		if len(rest) < 1 {
			return errors.New("hash requires a library id: photo-organizer hash <library-id>")
		}
		return cmdHash(ctx, c, out, rest[0], *wait)
	case "duplicates":
		if len(rest) < 1 {
			return errors.New("duplicates requires a library id: photo-organizer duplicates <library-id>")
		}
		return cmdDuplicates(ctx, c, out, rest[0], *limit, *offset)
	case "similar":
		if len(rest) < 1 {
			return errors.New("similar requires a library id: photo-organizer similar <library-id>")
		}
		return cmdSimilar(ctx, c, out, rest[0], *limit, *offset)
	case "queue":
		if len(rest) < 1 {
			return errors.New("queue requires a library id: photo-organizer queue <library-id>")
		}
		return cmdQueue(ctx, c, out, rest[0], *wait)
	case "jobs":
		if len(rest) < 1 {
			return errors.New("jobs requires a library id: photo-organizer jobs <library-id>")
		}
		return cmdJobs(ctx, c, out, rest[0])
	case "workers":
		return cmdWorkers(ctx, c, out)
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

type metadataSummary struct {
	Total        int `json:"total"`
	Extracted    int `json:"extracted"`
	Pending      int `json:"pending"`
	WithEXIFDate int `json:"with_exif_date"`
	WithFileDate int `json:"with_file_date"`
	WithNoDate   int `json:"with_no_date"`
	WithGPS      int `json:"with_gps"`
	Failed       int `json:"failed"`
}

type duplicateSummary struct {
	Groups           int   `json:"groups"`
	DuplicateFiles   int   `json:"duplicate_files"`
	ReclaimableBytes int64 `json:"reclaimable_bytes"`
	Hashed           int   `json:"hashed"`
	PendingHash      int   `json:"pending_hash"`
}

type duplicateMember struct {
	PhotoID       string `json:"photo_id"`
	RelativePath  string `json:"relative_path"`
	FileSizeBytes int64  `json:"file_size_bytes"`
	SuggestedKeep bool   `json:"suggested_keep"`
}

type duplicateGroup struct {
	ID               string            `json:"id"`
	SHA256           string            `json:"sha256"`
	PhotoCount       int               `json:"photo_count"`
	TotalBytes       int64             `json:"total_bytes"`
	ReclaimableBytes int64             `json:"reclaimable_bytes"`
	Photos           []duplicateMember `json:"photos"`
}

type duplicatesResponse struct {
	Groups  []duplicateGroup `json:"groups"`
	Summary duplicateSummary `json:"summary"`
}

type similarSummary struct {
	Groups           int   `json:"groups"`
	SimilarPhotos    int   `json:"similar_photos"`
	ReclaimableBytes int64 `json:"reclaimable_bytes"`
	ChainedGroups    int   `json:"chained_groups"`
	PHashed          int   `json:"phashed"`
	PendingPHash     int   `json:"pending_phash"`
}

type similarMember struct {
	PhotoID       string `json:"photo_id"`
	RelativePath  string `json:"relative_path"`
	FileSizeBytes int64  `json:"file_size_bytes"`
	Width         *int   `json:"width"`
	Height        *int   `json:"height"`
	Distance      int    `json:"distance"`
	Rank          int    `json:"rank"`
	SuggestedKeep bool   `json:"suggested_keep"`
}

type similarGroup struct {
	ID               string          `json:"id"`
	PhotoCount       int             `json:"photo_count"`
	Threshold        int             `json:"threshold"`
	MaxDistance      int             `json:"max_distance"`
	ReclaimableBytes int64           `json:"reclaimable_bytes"`
	Chained          bool            `json:"chained"`
	Photos           []similarMember `json:"photos"`
}

type similarResponse struct {
	Groups  []similarGroup `json:"groups"`
	Summary similarSummary `json:"summary"`
}

type jobCounts struct {
	Pending   int `json:"pending"`
	Running   int `json:"running"`
	Succeeded int `json:"succeeded"`
	Dead      int `json:"dead"`
	Total     int `json:"total"`
}

type queueResponse struct {
	LibraryID string    `json:"library_id"`
	Enqueued  int       `json:"enqueued"`
	Queue     jobCounts `json:"queue"`
}

type jobsResponse struct {
	Total  jobCounts            `json:"total"`
	ByType map[string]jobCounts `json:"by_type"`
}

type workerRow struct {
	ID              string    `json:"id"`
	Hostname        string    `json:"hostname"`
	PID             int       `json:"pid"`
	Status          string    `json:"status"`
	Concurrency     int       `json:"concurrency"`
	RunningJobs     int       `json:"running_jobs"`
	LastHeartbeatAt time.Time `json:"last_heartbeat_at"`
}

type workersResponse struct {
	Workers []workerRow `json:"workers"`
	Summary struct {
		Active      int `json:"active"`
		TotalSlots  int `json:"total_slots"`
		RunningJobs int `json:"running_jobs"`
	} `json:"summary"`
}

type libraryDetailResponse struct {
	Library        library          `json:"library"`
	PhotosByState  map[string]int   `json:"photos_by_state"`
	Metadata       metadataSummary  `json:"metadata"`
	Duplicates     duplicateSummary `json:"duplicates"`
	ScanningHere   bool             `json:"scanning_here"`
	ExtractingHere bool             `json:"extracting_here"`
	HashingHere    bool             `json:"hashing_here"`
}

type photo struct {
	ID               string     `json:"id"`
	RelativePath     string     `json:"relative_path"`
	OriginalFilename string     `json:"original_filename"`
	FileSizeBytes    int64      `json:"file_size_bytes"`
	DetectedFormat   *string    `json:"detected_format"`
	State            string     `json:"state"`
	Width            *int       `json:"width"`
	Height           *int       `json:"height"`
	CapturedAt       *time.Time `json:"captured_at"`
	CapturedAtSource *string    `json:"captured_at_source"`
	Latitude         *float64   `json:"latitude"`
	Longitude        *float64   `json:"longitude"`
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

// cmdProcess starts metadata extraction and optionally waits for it.
func cmdProcess(ctx context.Context, c *client, out io.Writer, libraryID string, wait bool) error {
	if _, err := c.do(ctx, http.MethodPost, "/api/libraries/"+libraryID+"/metadata", nil, nil); err != nil {
		return err
	}
	fmt.Fprintln(out, "Metadata extraction started.")

	if !wait {
		fmt.Fprintf(out, "\nPoll progress with:  photo-organizer photos %s\n", libraryID)
		return nil
	}
	return pollProcess(ctx, c, out, libraryID)
}

// pollProcess reports extraction progress from the database-backed counts,
// which stay correct even if the work is running in another process.
func pollProcess(ctx context.Context, c *client, out io.Writer, libraryID string) error {
	const interval = 400 * time.Millisecond
	last := -1

	for {
		var detail libraryDetailResponse
		if _, err := c.get(ctx, "/api/libraries/"+libraryID, &detail); err != nil {
			return err
		}

		m := detail.Metadata
		if m.Extracted != last {
			fmt.Fprintf(out, "\r  %d / %d extracted...", m.Extracted, m.Total)
			last = m.Extracted
		}

		if m.Pending == 0 && !detail.ExtractingHere {
			fmt.Fprintf(out, "\r  %d / %d extracted. Done.\n\n", m.Extracted, m.Total)
			printMetadataSummary(out, m)
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

func printMetadataSummary(out io.Writer, m metadataSummary) {
	fmt.Fprintf(out, "  EXIF timestamp        %d\n", m.WithEXIFDate)
	fmt.Fprintf(out, "  filesystem timestamp  %d\n", m.WithFileDate)
	fmt.Fprintf(out, "  no timestamp          %d\n", m.WithNoDate)
	fmt.Fprintf(out, "  GPS coordinates       %d\n", m.WithGPS)
	if m.Failed > 0 {
		fmt.Fprintf(out, "  failed to decode      %d\n", m.Failed)
	}
}

// cmdSimilar lists near-duplicate groups.
func cmdSimilar(ctx context.Context, c *client, out io.Writer, libraryID string, limit, offset int) error {
	var resp similarResponse
	url := fmt.Sprintf("/api/libraries/%s/similar?limit=%d&offset=%d", libraryID, limit, offset)
	if _, err := c.get(ctx, url, &resp); err != nil {
		return err
	}

	s := resp.Summary
	fmt.Fprintf(out, "Similar groups     %d\n", s.Groups)
	fmt.Fprintf(out, "Photos involved    %d\n", s.SimilarPhotos)
	fmt.Fprintf(out, "Reclaimable        %s\n", humanBytes(s.ReclaimableBytes))
	if s.ChainedGroups > 0 {
		fmt.Fprintf(out, "Chained groups     %d  (wider than the threshold; worth reviewing)\n", s.ChainedGroups)
	}
	if s.PendingPHash > 0 {
		fmt.Fprintf(out, "\n%d photos are not perceptually hashed yet -- results are incomplete.\n", s.PendingPHash)
	}
	fmt.Fprintln(out)

	if len(resp.Groups) == 0 {
		if s.PHashed == 0 {
			fmt.Fprintf(out, "Nothing hashed yet. Run:  photo-organizer queue %s -wait\n", libraryID)
		} else {
			fmt.Fprintln(out, "No near-duplicates found.")
		}
		return nil
	}

	for i, g := range resp.Groups {
		marker := ""
		if g.Chained {
			marker = fmt.Sprintf("  [CHAINED: widest pair %d exceeds threshold %d]",
				g.MaxDistance, g.Threshold)
		}
		fmt.Fprintf(out, "Group %d  (%d photos, %s reclaimable)%s\n",
			offset+i+1, g.PhotoCount, humanBytes(g.ReclaimableBytes), marker)

		for _, p := range g.Photos {
			label := "similar"
			if p.SuggestedKeep {
				label = "KEEP   "
			}
			dims := "-"
			if p.Width != nil && p.Height != nil {
				dims = fmt.Sprintf("%dx%d", *p.Width, *p.Height)
			}
			fmt.Fprintf(out, "  %s  dist %2d  %-10s  %-11s  %s\n",
				label, p.Distance, humanBytes(p.FileSizeBytes), dims, p.RelativePath)
		}
		fmt.Fprintln(out)
	}

	fmt.Fprintln(out, "These are suggestions. No files have been modified, moved or deleted.")
	fmt.Fprintln(out, "dist = Hamming distance from the KEEP photo, out of 64 bits.")
	fmt.Fprintln(out, "KEEP prefers higher resolution, then a larger file at equal resolution.")
	return nil
}

// cmdQueue enqueues the pipeline as jobs and optionally watches it drain.
func cmdQueue(ctx context.Context, c *client, out io.Writer, libraryID string, wait bool) error {
	var resp queueResponse
	if _, err := c.do(ctx, http.MethodPost, "/api/libraries/"+libraryID+"/process", nil, &resp); err != nil {
		return err
	}
	fmt.Fprintf(out, "Enqueued %d new jobs.\n", resp.Enqueued)
	if resp.Enqueued == 0 && resp.Queue.Total > 0 {
		fmt.Fprintln(out, "(Nothing new -- the work is already queued or done.)")
	}
	fmt.Fprintln(out)

	if !wait {
		fmt.Fprintf(out, "Watch it with:  photo-organizer jobs %s\n", libraryID)
		return nil
	}

	const interval = 500 * time.Millisecond
	last := -1
	for {
		var jr jobsResponse
		if _, err := c.get(ctx, "/api/libraries/"+libraryID+"/jobs", &jr); err != nil {
			return err
		}
		t := jr.Total
		done := t.Succeeded + t.Dead

		if done != last {
			fmt.Fprintf(out, "\r  %d/%d done  (%d running, %d pending)   ",
				done, t.Total, t.Running, t.Pending)
			last = done
		}

		if t.Pending == 0 && t.Running == 0 && t.Total > 0 {
			fmt.Fprintf(out, "\r  %d/%d done  (%d succeeded, %d dead)        \n\n",
				done, t.Total, t.Succeeded, t.Dead)
			printJobTable(out, jr.ByType)
			if t.Dead > 0 {
				fmt.Fprintf(out, "\n%d job(s) exhausted their retries. Inspect with:\n"+
					"  docker compose exec postgres psql -U photo -d photoorganizer "+
					"-c \"SELECT job_type, last_error, count(*) FROM jobs WHERE status='dead' "+
					"GROUP BY 1,2\"\n", t.Dead)
			}
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

func cmdJobs(ctx context.Context, c *client, out io.Writer, libraryID string) error {
	var jr jobsResponse
	if _, err := c.get(ctx, "/api/libraries/"+libraryID+"/jobs", &jr); err != nil {
		return err
	}
	if jr.Total.Total == 0 {
		fmt.Fprintf(out, "No jobs queued. Create some with:  photo-organizer queue %s\n", libraryID)
		return nil
	}
	printJobTable(out, jr.ByType)

	t := jr.Total
	fmt.Fprintf(out, "\n%-22s  %6d  %6d  %6d  %6d  %6d\n",
		"TOTAL", t.Pending, t.Running, t.Succeeded, t.Dead, t.Total)
	return nil
}

func printJobTable(out io.Writer, byType map[string]jobCounts) {
	fmt.Fprintf(out, "%-22s  %6s  %6s  %6s  %6s  %6s\n",
		"JOB TYPE", "PEND", "RUN", "OK", "DEAD", "TOTAL")

	// Stable order: Go map iteration is randomised, and a table that reshuffles
	// between polls is unreadable.
	names := make([]string, 0, len(byType))
	for name := range byType {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		c := byType[name]
		fmt.Fprintf(out, "%-22s  %6d  %6d  %6d  %6d  %6d\n",
			name, c.Pending, c.Running, c.Succeeded, c.Dead, c.Total)
	}
}

func cmdWorkers(ctx context.Context, c *client, out io.Writer) error {
	var resp workersResponse
	if _, err := c.get(ctx, "/api/workers", &resp); err != nil {
		return err
	}

	if len(resp.Workers) == 0 {
		fmt.Fprintln(out, "No workers have ever registered.")
		fmt.Fprintln(out, "Start some with:  docker compose up -d --scale worker=4")
		return nil
	}

	fmt.Fprintf(out, "Active %d   Slots %d   Running jobs %d\n\n",
		resp.Summary.Active, resp.Summary.TotalSlots, resp.Summary.RunningJobs)

	fmt.Fprintf(out, "%-10s  %-16s  %-7s  %5s  %5s  %s\n",
		"STATUS", "HOSTNAME", "PID", "SLOTS", "JOBS", "LAST SEEN")
	for _, w := range resp.Workers {
		fmt.Fprintf(out, "%-10s  %-16s  %-7d  %5d  %5d  %s\n",
			w.Status, truncate(w.Hostname, 16), w.PID, w.Concurrency, w.RunningJobs,
			humanAgo(w.LastHeartbeatAt))
	}

	fmt.Fprintln(out, "\nstopped = shut down cleanly and handed its work back")
	fmt.Fprintln(out, "dead    = stopped heartbeating; its leases were reclaimed by the reaper")
	return nil
}

func humanAgo(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < 2*time.Second:
		return "just now"
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	default:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
}

// cmdHash starts content hashing and optionally waits for it.
func cmdHash(ctx context.Context, c *client, out io.Writer, libraryID string, wait bool) error {
	if _, err := c.do(ctx, http.MethodPost, "/api/libraries/"+libraryID+"/hash", nil, nil); err != nil {
		return err
	}
	fmt.Fprintln(out, "Hashing started.")

	if !wait {
		fmt.Fprintf(out, "\nPoll progress with:  photo-organizer duplicates %s\n", libraryID)
		return nil
	}

	const interval = 400 * time.Millisecond
	last := -1
	for {
		var detail libraryDetailResponse
		if _, err := c.get(ctx, "/api/libraries/"+libraryID, &detail); err != nil {
			return err
		}
		d := detail.Duplicates

		if d.Hashed != last {
			fmt.Fprintf(out, "\r  %d hashed, %d pending...", d.Hashed, d.PendingHash)
			last = d.Hashed
		}

		if d.PendingHash == 0 && !detail.HashingHere {
			fmt.Fprintf(out, "\r  %d photos hashed. Done.\n\n", d.Hashed)
			fmt.Fprintf(out, "  duplicate groups      %d\n", d.Groups)
			fmt.Fprintf(out, "  duplicate files       %d\n", d.DuplicateFiles)
			fmt.Fprintf(out, "  reclaimable           %s\n", humanBytes(d.ReclaimableBytes))
			if d.Groups > 0 {
				fmt.Fprintf(out, "\nReview them with:  photo-organizer duplicates %s\n", libraryID)
			}
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

// cmdDuplicates lists exact-duplicate groups.
func cmdDuplicates(ctx context.Context, c *client, out io.Writer, libraryID string, limit, offset int) error {
	var resp duplicatesResponse
	url := fmt.Sprintf("/api/libraries/%s/duplicates?limit=%d&offset=%d", libraryID, limit, offset)
	if _, err := c.get(ctx, url, &resp); err != nil {
		return err
	}

	s := resp.Summary
	fmt.Fprintf(out, "Duplicate groups   %d\n", s.Groups)
	fmt.Fprintf(out, "Duplicate files    %d\n", s.DuplicateFiles)
	fmt.Fprintf(out, "Reclaimable        %s\n", humanBytes(s.ReclaimableBytes))
	if s.PendingHash > 0 {
		fmt.Fprintf(out, "\n%d photos are not hashed yet -- run 'hash' first for a complete picture.\n", s.PendingHash)
	}
	fmt.Fprintln(out)

	if len(resp.Groups) == 0 {
		if s.Hashed == 0 {
			fmt.Fprintf(out, "Nothing hashed yet. Run:  photo-organizer hash %s -wait\n", libraryID)
		} else {
			fmt.Fprintln(out, "No exact duplicates found.")
		}
		return nil
	}

	for i, g := range resp.Groups {
		fmt.Fprintf(out, "Group %d  %s  (%d copies, %s reclaimable)\n",
			offset+i+1, g.SHA256[:12], g.PhotoCount, humanBytes(g.ReclaimableBytes))
		for _, p := range g.Photos {
			marker := "  duplicate"
			if p.SuggestedKeep {
				marker = "  KEEP     "
			}
			fmt.Fprintf(out, "  %s  %-10s  %s\n", marker, humanBytes(p.FileSizeBytes), p.RelativePath)
		}
		fmt.Fprintln(out)
	}

	// Repeated in the CLI as well as the API payload. A user acting on these
	// suggestions is about to delete their own photos by hand, and it should be
	// unambiguous that this tool has not touched anything.
	fmt.Fprintln(out, "These are suggestions. No files have been modified, moved or deleted.")
	fmt.Fprintln(out, "KEEP marks the copy with the shortest, least-nested path -- for exact")
	fmt.Fprintln(out, "duplicates every copy is byte-identical, so this picks a path, not a photo.")
	return nil
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

	fmt.Fprintf(out, "%-10s  %-5s  %-9s  %-18s  %-14s  %s\n",
		"SIZE", "FMT", "DIMS", "CAPTURED", "GPS", "PATH")
	for _, p := range resp.Photos {
		format := "-"
		if p.DetectedFormat != nil {
			format = *p.DetectedFormat
		}

		dims := "-"
		if p.Width != nil && p.Height != nil {
			dims = fmt.Sprintf("%dx%d", *p.Width, *p.Height)
		}

		// The timestamp carries its provenance, so a date that came from file
		// mtime is never mistaken for one the camera actually recorded.
		captured := "-"
		if p.CapturedAt != nil {
			mark := "?"
			if p.CapturedAtSource != nil {
				switch *p.CapturedAtSource {
				case "exif":
					mark = "E"
				case "filesystem":
					mark = "F"
				}
			}
			captured = p.CapturedAt.Format("2006-01-02 15:04") + " " + mark
		}

		gps := "-"
		if p.Latitude != nil && p.Longitude != nil {
			gps = fmt.Sprintf("%.3f,%.3f", *p.Latitude, *p.Longitude)
		}

		fmt.Fprintf(out, "%-10s  %-5s  %-9s  %-18s  %-14s  %s\n",
			humanBytes(p.FileSizeBytes), format, dims, captured, gps, p.RelativePath)
	}
	fmt.Fprintf(out, "\n  CAPTURED:  E = from EXIF,  F = from file mtime (less reliable)\n")

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
