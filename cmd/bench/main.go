// Command bench measures the pipeline end to end against the running Docker
// Compose stack, and demonstrates crash recovery reproducibly.
//
//	go run ./cmd/bench pipeline   throughput as the worker pool scales
//	go run ./cmd/bench crash      SIGKILL a worker mid-flight; prove nothing is lost
//	go run ./cmd/bench clean      remove the benchmark corpus and its library
//
// A Go program rather than a shell script so it runs the same from PowerShell,
// bash or cmd -- this project is developed on Windows and runs on Linux.
//
// EVERY NUMBER IT PRINTS IS MEASURED ON THE MACHINE IT RUNS ON, and the report
// records that machine. Nothing is extrapolated, and a run whose results do not
// match the corpus's ground truth is reported as failed rather than timed: a
// fast pipeline that produces the wrong groups is not a result.
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
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/srikarguntaka/photo-organizer/internal/fixtures"
)

const usage = `bench -- measure the photo-organizer pipeline against the running stack

Usage:
  go run ./cmd/bench <command> [flags]

Commands:
  pipeline   Process a generated corpus with 1, 2, 4... workers and time it
  crash      Kill a worker holding leases mid-run and verify full recovery
  clean      Delete the benchmark corpus and its library

Requires the stack to be up (docker compose up -d) and the docker CLI on PATH.
The corpus is written under the host photo directory (default ./sample-photos)
in a _bench subdirectory, so the containers see it at /photos/_bench.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	var err error
	switch os.Args[1] {
	case "pipeline":
		err = cmdPipeline(ctx, os.Args[2:])
	case "crash":
		err = cmdCrash(ctx, os.Args[2:])
	case "clean":
		err = cmdClean(ctx, os.Args[2:])
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "\nbench: %v\n", err)
		os.Exit(1)
	}
}

// ---------------------------------------------------------------------------
// shared configuration
// ---------------------------------------------------------------------------

type env struct {
	api       string
	photosDir string // host directory mounted at /photos
	pgUser    string
	pgDB      string
	http      *http.Client
}

// benchSubdir is where the corpus lives, relative to the photo root. One fixed
// name, so a benchmark can never be pointed at a user's real photos by mistake:
// the tool only ever writes to, scans and deletes this one directory.
const benchSubdir = "_bench"

func (e *env) containerPath() string { return "/photos/" + benchSubdir }
func (e *env) hostPath() string      { return filepath.Join(e.photosDir, benchSubdir) }

type corpusFlags struct {
	scenes, width, height int
	seed                  int64
}

func commonFlags(fs *flag.FlagSet) (*env, *corpusFlags) {
	e := &env{http: &http.Client{Timeout: 30 * time.Second}}
	c := &corpusFlags{}
	fs.StringVar(&e.api, "api", envOr("API_BASE_URL", "http://localhost:8080"), "API base URL")
	fs.StringVar(&e.photosDir, "photos-dir", envOr("HOST_PHOTOS_DIR", "./sample-photos"), "host directory mounted at /photos")
	fs.StringVar(&e.pgUser, "pg-user", envOr("POSTGRES_USER", "photo"), "Postgres user")
	fs.StringVar(&e.pgDB, "pg-db", envOr("POSTGRES_DB", "photoorganizer"), "Postgres database")
	// 40 scenes is 68 images and 343 jobs: enough that per-run fixed costs are
	// a small share of the wall time, small enough that a single-worker run
	// finishes in about a minute and a half on the reference machine.
	fs.IntVar(&c.scenes, "scenes", 40, "distinct base scenes (yields fewer than 2 images per scene)")
	// 4032x3024 is a 12MP phone photo. JPEG decode dominates per-job cost at
	// that size, which is the regime a real library is in.
	fs.IntVar(&c.width, "width", 4032, "image width")
	fs.IntVar(&c.height, "height", 3024, "image height")
	fs.Int64Var(&c.seed, "seed", 1, "corpus seed")
	return e, c
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// ---------------------------------------------------------------------------
// corpus
// ---------------------------------------------------------------------------

type corpusParams struct {
	Scenes int   `json:"scenes"`
	Width  int   `json:"width"`
	Height int   `json:"height"`
	Seed   int64 `json:"seed"`
}

// ensureCorpus generates the benchmark corpus, or reuses one already on disk
// with identical parameters. 12MP generation takes minutes, and regenerating
// between runs would only add noise.
func ensureCorpus(e *env, c *corpusFlags) (*fixtures.Manifest, error) {
	dir := e.hostPath()
	want := corpusParams{Scenes: c.scenes, Width: c.width, Height: c.height, Seed: c.seed}
	paramsFile := filepath.Join(dir, ".bench.json")

	if b, err := os.ReadFile(paramsFile); err == nil {
		var got corpusParams
		if json.Unmarshal(b, &got) == nil && got == want {
			if mb, err := os.ReadFile(filepath.Join(dir, "MANIFEST.json")); err == nil {
				var m fixtures.Manifest
				if err := json.Unmarshal(mb, &m); err == nil {
					logf("reusing corpus at %s (%d images, %dx%d)", dir, m.Summary.SupportedImages, c.width, c.height)
					return &m, nil
				}
			}
		}
	}

	logf("generating corpus: %d scenes at %dx%d into %s ...", c.scenes, c.width, c.height, dir)
	if err := os.RemoveAll(dir); err != nil {
		return nil, err
	}
	start := time.Now()
	m, err := fixtures.Generate(fixtures.Options{
		Root: dir, Seed: c.seed, Scenes: c.scenes, Width: c.width, Height: c.height,
	})
	if err != nil {
		return nil, err
	}
	pb, _ := json.Marshal(want)
	if err := os.WriteFile(paramsFile, pb, 0o644); err != nil {
		return nil, err
	}
	logf("generated %d images in %s", m.Summary.SupportedImages, time.Since(start).Round(time.Second))
	return m, nil
}

func corpusBytes(dir string) int64 {
	var n int64
	_ = filepath.WalkDir(dir, func(_ string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			if info, err := d.Info(); err == nil {
				n += info.Size()
			}
		}
		return nil
	})
	return n
}

// ---------------------------------------------------------------------------
// process helpers: docker, psql, HTTP
// ---------------------------------------------------------------------------

// run executes a command and returns its STDOUT only.
//
// Stderr is kept separate and surfaced only on failure. Merging the two would
// let a warning -- docker compose prints them for unset variables, psql for
// notices -- be parsed as a query result row, silently corrupting a number
// in the report.
func run(ctx context.Context, extraEnv []string, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = append(os.Environ(), extraEnv...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return stdout.String(), fmt.Errorf("%s %s: %w\n%s%s", name, strings.Join(args, " "), err,
			stdout.String(), stderr.String())
	}
	return stdout.String(), nil
}

func compose(ctx context.Context, extraEnv []string, args ...string) (string, error) {
	return run(ctx, extraEnv, "docker", append([]string{"compose"}, args...)...)
}

// sql runs a query through psql inside the postgres container and returns rows
// of "|"-separated fields.
//
// Through `docker compose exec` rather than a direct connection: the host's
// port 5432 is frequently taken by a native Postgres install (it is on the
// machine this was written on), and a direct connection would silently query
// the wrong server.
//
// Every value interpolated into a query here is either a constant or has been
// validated -- see uuidRE -- so there is no user input to escape.
func (e *env) sql(ctx context.Context, query string) ([][]string, error) {
	out, err := compose(ctx, nil, "exec", "-T", "postgres",
		"psql", "-U", e.pgUser, "-d", e.pgDB, "-At", "-F", "|", "-v", "ON_ERROR_STOP=1", "-c", query)
	if err != nil {
		return nil, err
	}
	var rows [][]string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			continue
		}
		rows = append(rows, strings.Split(line, "|"))
	}
	return rows, nil
}

var uuidRE = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

func mustUUID(s string) string {
	if !uuidRE.MatchString(s) {
		panic(fmt.Sprintf("refusing to interpolate non-uuid %q into SQL", s))
	}
	return s
}

func (e *env) get(ctx context.Context, path string, v any) error {
	return e.do(ctx, http.MethodGet, path, nil, v)
}

func (e *env) post(ctx context.Context, path string, body any, v any) error {
	return e.do(ctx, http.MethodPost, path, body, v)
}

func (e *env) do(ctx context.Context, method, path string, body, v any) error {
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(e.api, "/")+path, r)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := e.http.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w (is the stack up?)", method, path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return fmt.Errorf("%s %s: HTTP %d: %s", method, path, resp.StatusCode, strings.TrimSpace(string(b)))
	}
	if v != nil {
		return json.Unmarshal(b, v)
	}
	return nil
}

func logf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "[%s] %s\n", time.Now().Format("15:04:05"), fmt.Sprintf(format, args...))
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}

// ---------------------------------------------------------------------------
// stack operations
// ---------------------------------------------------------------------------

type workersResp struct {
	Workers []struct {
		ID          string `json:"id"`
		Hostname    string `json:"hostname"`
		Status      string `json:"status"`
		Concurrency int    `json:"concurrency"`
	} `json:"workers"`
}

func runningWorkerContainers(ctx context.Context) (int, error) {
	out, err := compose(ctx, nil, "ps", "-q", "worker")
	if err != nil {
		return 0, err
	}
	n := 0
	for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
		if strings.TrimSpace(l) != "" {
			n++
		}
	}
	return n, nil
}

// containerState is what matters about a worker container for detecting that
// it died during a measurement.
type containerState struct {
	running   bool
	restarts  int
	oomKilled bool
}

func workerFleetState(ctx context.Context) (map[string]containerState, error) {
	out, err := compose(ctx, nil, "ps", "-a", "-q", "worker")
	if err != nil {
		return nil, err
	}
	state := map[string]containerState{}
	for _, id := range strings.Fields(out) {
		o, err := run(ctx, nil, "docker", "inspect", "-f", "{{.State.Running}} {{.RestartCount}} {{.State.OOMKilled}}", id)
		if err != nil {
			return nil, err
		}
		f := strings.Fields(o)
		if len(f) != 3 {
			continue
		}
		n, _ := strconv.Atoi(f[1])
		state[id] = containerState{running: f[0] == "true", restarts: n, oomKilled: f[2] == "true"}
	}
	return state, nil
}

// fleetChanged describes any worker that stopped, restarted or was OOM-killed
// between two snapshots, or returns "".
func fleetChanged(before, after map[string]containerState) string {
	var msgs []string
	for id, b := range before {
		if !b.running {
			continue
		}
		a, ok := after[id]
		short := id
		if len(short) > 12 {
			short = short[:12]
		}
		switch {
		case !ok || !a.running:
			msgs = append(msgs, "worker "+short+" stopped during the run")
		case a.oomKilled && !b.oomKilled:
			msgs = append(msgs, "worker "+short+" was OOM-killed during the run")
		case a.restarts != b.restarts:
			msgs = append(msgs, "worker "+short+" restarted during the run")
		}
	}
	return strings.Join(msgs, "; ")
}

// scaleWorkers sets the worker replica count and per-worker concurrency, then
// waits until exactly that fleet is heartbeating. Timing a run that starts
// before the last replica has registered would under-report its throughput.
func (e *env) scaleWorkers(ctx context.Context, n, concurrency int) error {
	extra := []string{}
	if concurrency > 0 {
		extra = append(extra, fmt.Sprintf("PROCESS_CONCURRENCY=%d", concurrency))
	}
	if _, err := compose(ctx, extra, "up", "-d", "--no-deps", "--scale", fmt.Sprintf("worker=%d", n), "worker"); err != nil {
		return err
	}

	deadline := time.Now().Add(3 * time.Minute)
	for time.Now().Before(deadline) {
		var w workersResp
		if err := e.get(ctx, "/api/workers", &w); err == nil {
			active, matching := 0, 0
			for _, x := range w.Workers {
				if x.Status == "active" {
					active++
					if concurrency <= 0 || x.Concurrency == concurrency {
						matching++
					}
				}
			}
			if active == n && matching == n {
				return nil
			}
		}
		if err := sleepCtx(ctx, time.Second); err != nil {
			return err
		}
	}
	return fmt.Errorf("worker fleet did not settle at %d active workers", n)
}

// resetLibrary removes the benchmark library. Photos, groups, clusters, jobs
// and job executions all cascade from it, so each run starts from nothing.
func (e *env) resetLibrary(ctx context.Context) error {
	_, err := e.sql(ctx, fmt.Sprintf("DELETE FROM libraries WHERE root_path = '%s'", e.containerPath()))
	return err
}

type libraryResp struct {
	Library struct {
		ID         string `json:"id"`
		ScanState  string `json:"scan_state"`
		PhotoCount *int   `json:"photo_count"`
	} `json:"library"`
	Duplicates struct {
		Groups int `json:"groups"`
	} `json:"duplicates"`
	Similar struct {
		Groups int `json:"groups"`
	} `json:"similar"`
	Clusters struct {
		Clusters int `json:"clusters"`
	} `json:"clusters"`
}

func (e *env) createAndScan(ctx context.Context) (string, time.Duration, error) {
	var lib struct {
		ID string `json:"id"`
	}
	if err := e.post(ctx, "/api/libraries", map[string]string{"name": "bench", "path": e.containerPath()}, &lib); err != nil {
		return "", 0, err
	}
	id := mustUUID(lib.ID)

	start := time.Now()
	if err := e.post(ctx, "/api/libraries/"+id+"/scan", map[string]any{}, nil); err != nil {
		return "", 0, err
	}
	for {
		var l libraryResp
		if err := e.get(ctx, "/api/libraries/"+id, &l); err != nil {
			return "", 0, err
		}
		if l.Library.ScanState == "complete" {
			return id, time.Since(start), nil
		}
		if err := sleepCtx(ctx, 100*time.Millisecond); err != nil {
			return "", 0, err
		}
	}
}

type jobCounts struct {
	Pending   int `json:"pending"`
	Running   int `json:"running"`
	Succeeded int `json:"succeeded"`
	Dead      int `json:"dead"`
	Total     int `json:"total"`
}

type jobsResp struct {
	ByType map[string]jobCounts `json:"by_type"`
	Total  jobCounts            `json:"total"`
}

// waitDrained polls the queue until nothing is pending or running.
func (e *env) waitDrained(ctx context.Context, libID string, timeout time.Duration, onTick func(jobsResp)) (jobsResp, error) {
	deadline := time.Now().Add(timeout)
	for {
		var j jobsResp
		if err := e.get(ctx, "/api/libraries/"+libID+"/jobs", &j); err != nil {
			return j, err
		}
		if onTick != nil {
			onTick(j)
		}
		if j.Total.Total > 0 && j.Total.Pending+j.Total.Running == 0 {
			return j, nil
		}
		if time.Now().After(deadline) {
			return j, fmt.Errorf("queue did not drain within %s (pending %d, running %d)",
				timeout, j.Total.Pending, j.Total.Running)
		}
		if err := sleepCtx(ctx, 200*time.Millisecond); err != nil {
			return j, err
		}
	}
}

// verify checks a finished run against the corpus's ground truth. A benchmark
// that produced wrong answers quickly has measured nothing worth reporting.
func (e *env) verify(ctx context.Context, libID string, m *fixtures.Manifest, jobs jobsResp) []string {
	var problems []string
	var l libraryResp
	if err := e.get(ctx, "/api/libraries/"+libID, &l); err != nil {
		return []string{err.Error()}
	}
	check := func(name string, got, want int) {
		if got != want {
			problems = append(problems, fmt.Sprintf("%s: got %d, ground truth %d", name, got, want))
		}
	}
	if l.Library.PhotoCount != nil {
		check("photos discovered", *l.Library.PhotoCount, m.Summary.SupportedImages)
	}
	check("exact-duplicate groups", l.Duplicates.Groups, m.Summary.ExactDuplicateGroups)
	check("near-duplicate groups", l.Similar.Groups, m.Summary.NearDuplicateGroups)
	check("dead jobs", jobs.Total.Dead, 0)

	// Every readable photo must have been through every per-photo stage.
	rows, err := e.sql(ctx, fmt.Sprintf(`SELECT count(*) FROM photos
		WHERE library_id = '%s' AND state <> 'failed' AND (
		  metadata_extracted_at IS NULL OR hashed_at IS NULL OR phashed_at IS NULL
		  OR quality_analyzed_at IS NULL OR thumbnailed_at IS NULL)`, mustUUID(libID)))
	if err != nil {
		problems = append(problems, err.Error())
	} else if len(rows) == 1 && rows[0][0] != "0" {
		problems = append(problems, fmt.Sprintf("%s photos missed at least one stage", rows[0][0]))
	}
	return problems
}

type stageStat struct {
	jobType          string
	n                int
	p50, p95, meanMS float64
	deferrals        int
}

// stageStats reads per-execution durations from job_executions -- the queue's
// own record of when each attempt started and finished, written by the worker
// that ran it.
func (e *env) stageStats(ctx context.Context, libID string) ([]stageStat, error) {
	id := mustUUID(libID)
	rows, err := e.sql(ctx, fmt.Sprintf(`
		SELECT j.job_type,
		       count(*),
		       round(percentile_cont(0.5)  WITHIN GROUP (ORDER BY extract(epoch FROM e.finished_at - e.started_at) * 1000)::numeric, 1),
		       round(percentile_cont(0.95) WITHIN GROUP (ORDER BY extract(epoch FROM e.finished_at - e.started_at) * 1000)::numeric, 1),
		       round(avg(extract(epoch FROM e.finished_at - e.started_at) * 1000)::numeric, 1)
		FROM job_executions e JOIN jobs j ON j.id = e.job_id
		WHERE j.library_id = '%s' AND e.outcome = 'succeeded'
		GROUP BY j.job_type ORDER BY j.job_type`, id))
	if err != nil {
		return nil, err
	}
	deferred := map[string]int{}
	drows, err := e.sql(ctx, fmt.Sprintf(`
		SELECT j.job_type, count(*) FROM job_executions e JOIN jobs j ON j.id = e.job_id
		WHERE j.library_id = '%s' AND e.outcome = 'deferred' GROUP BY 1`, id))
	if err != nil {
		return nil, err
	}
	for _, r := range drows {
		deferred[r[0]], _ = strconv.Atoi(r[1])
	}

	var out []stageStat
	for _, r := range rows {
		if len(r) < 5 {
			continue
		}
		s := stageStat{jobType: r[0], deferrals: deferred[r[0]]}
		s.n, _ = strconv.Atoi(r[1])
		s.p50, _ = strconv.ParseFloat(r[2], 64)
		s.p95, _ = strconv.ParseFloat(r[3], 64)
		s.meanMS, _ = strconv.ParseFloat(r[4], 64)
		out = append(out, s)
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// environment capture
// ---------------------------------------------------------------------------

func describeEnvironment(ctx context.Context) []string {
	lines := []string{
		fmt.Sprintf("Date: %s", time.Now().UTC().Format("2006-01-02 15:04 UTC")),
		fmt.Sprintf("Host: %s/%s, %d logical CPUs", runtime.GOOS, runtime.GOARCH, runtime.NumCPU()),
	}
	if out, err := run(ctx, nil, "docker", "info", "--format", "{{.NCPU}}|{{.MemTotal}}|{{.OperatingSystem}}|{{.ServerVersion}}"); err == nil {
		f := strings.Split(strings.TrimSpace(out), "|")
		if len(f) == 4 {
			mem, _ := strconv.ParseInt(f[1], 10, 64)
			lines = append(lines, fmt.Sprintf("Docker: %s, engine %s, %s CPUs and %.1f GiB available to containers",
				f[2], f[3], f[0], float64(mem)/(1<<30)))
		}
	}
	if out, err := run(ctx, nil, "git", "rev-parse", "--short", "HEAD"); err == nil {
		commit := strings.TrimSpace(out)
		// benchmarks/ is excluded: it is where these reports are written, so a
		// report from an earlier command in the same session would otherwise
		// mark every later run as coming from a modified tree.
		if st, err := run(ctx, nil, "git", "status", "--porcelain", "--", ".", ":(exclude)benchmarks"); err == nil && strings.TrimSpace(st) != "" {
			commit += " (with uncommitted changes)"
		}
		lines = append(lines, "Commit: "+commit)
	}
	return lines
}

func writeReport(path, content string) error {
	if path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return err
	}
	logf("report written to %s", path)
	return nil
}

func median(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	s := append([]float64(nil), xs...)
	sort.Float64s(s)
	if len(s)%2 == 1 {
		return s[len(s)/2]
	}
	return (s[len(s)/2-1] + s[len(s)/2]) / 2
}

func minMax(xs []float64) (float64, float64) {
	lo, hi := xs[0], xs[0]
	for _, x := range xs {
		lo, hi = min(lo, x), max(hi, x)
	}
	return lo, hi
}

// ---------------------------------------------------------------------------
// pipeline
// ---------------------------------------------------------------------------

type runResult struct {
	workers, concurrency int
	scan, wall           time.Duration
	jobs                 int
	photos               int
	stats                []stageStat
	problems             []string
}

func cmdPipeline(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("pipeline", flag.ExitOnError)
	e, c := commonFlags(fs)
	workersFlag := fs.String("workers", "1,2,4,8", "comma-separated worker counts to measure")
	concurrency := fs.Int("concurrency", 2, "jobs each worker runs at once (PROCESS_CONCURRENCY)")
	repeat := fs.Int("repeat", 3, "runs per worker count; the median is reported")
	warmup := fs.Int("warmup", 1, "unrecorded runs after each rescale, before timing")
	timeout := fs.Duration("timeout", 30*time.Minute, "per-run limit")
	out := fs.String("out", "benchmarks/pipeline.md", "report path (empty to skip writing)")
	_ = fs.Parse(args)

	var counts []int
	for _, s := range strings.Split(*workersFlag, ",") {
		n, err := strconv.Atoi(strings.TrimSpace(s))
		if err != nil || n < 1 {
			return fmt.Errorf("bad -workers value %q", s)
		}
		counts = append(counts, n)
	}

	m, err := ensureCorpus(e, c)
	if err != nil {
		return err
	}
	originalWorkers, _ := runningWorkerContainers(ctx)
	defer func() {
		if originalWorkers > 0 {
			logf("restoring %d worker(s) with default concurrency", originalWorkers)
			_, _ = compose(context.Background(), nil, "up", "-d", "--no-deps", "--scale", fmt.Sprintf("worker=%d", originalWorkers), "worker")
		}
	}()

	results := map[int][]runResult{}
	for _, n := range counts {
		logf("scaling to %d worker(s) x %d concurrency", n, *concurrency)
		if err := e.scaleWorkers(ctx, n, *concurrency); err != nil {
			return err
		}
		// Warm-up runs are executed in full and thrown away. A freshly created
		// fleet pays one-off costs -- database connections, the file cache on
		// the bind mount, Go runtime warm-up -- that a smoke test showed can
		// push a single job from ~60 ms to over a second. Those belong to
		// starting a worker, not to processing a photo.
		for r := 1 - *warmup; r <= *repeat; r++ {
			if err := e.resetLibrary(ctx); err != nil {
				return err
			}
			libID, scanDur, err := e.createAndScan(ctx)
			if err != nil {
				return err
			}

			var enq struct {
				Enqueued int `json:"enqueued"`
			}
			start := time.Now()
			if err := e.post(ctx, "/api/libraries/"+libID+"/process", map[string]any{}, &enq); err != nil {
				return err
			}
			fleetBefore, err := workerFleetState(ctx)
			if err != nil {
				return err
			}
			jobs, err := e.waitDrained(ctx, libID, *timeout, nil)
			wall := time.Since(start)
			if err != nil {
				return err
			}

			stats, err := e.stageStats(ctx, libID)
			if err != nil {
				return err
			}
			res := runResult{
				workers: n, concurrency: *concurrency, scan: scanDur, wall: wall,
				jobs: jobs.Total.Total, photos: m.Summary.SupportedImages, stats: stats,
				problems: e.verify(ctx, libID, m, jobs),
			}
			// A worker that died mid-run -- most likely out of memory at high
			// fan-out -- is recovered by the lease mechanism, so the results
			// still verify. But the wall time then includes a 60 s lease
			// expiry and measures recovery, not throughput. Such a run is
			// reported as invalid rather than silently averaged in.
			if fleetAfter, err := workerFleetState(ctx); err != nil {
				return err
			} else if msg := fleetChanged(fleetBefore, fleetAfter); msg != "" {
				res.problems = append(res.problems, msg)
			}
			status := "verified"
			if len(res.problems) > 0 {
				status = "FAILED VERIFICATION: " + strings.Join(res.problems, "; ")
			}
			label := fmt.Sprintf("run %d/%d", r, *repeat)
			if r <= 0 {
				label = "warm-up (not recorded)"
			} else {
				results[n] = append(results[n], res)
			}
			logf("workers=%d %s: %d jobs in %s (%.2f photos/s) -- %s",
				n, label, res.jobs, wall.Round(10*time.Millisecond),
				float64(res.photos)/wall.Seconds(), status)
			// A warm-up that produced wrong results is still a failure worth
			// stopping for: the timed runs would be measuring a broken pipeline.
			if r <= 0 && len(res.problems) > 0 {
				return fmt.Errorf("warm-up failed verification: %s", strings.Join(res.problems, "; "))
			}
		}
	}

	report := renderPipeline(ctx, e, c, m, counts, *concurrency, *repeat, *warmup, results)
	fmt.Println(report)
	return writeReport(*out, report)
}

func renderPipeline(ctx context.Context, e *env, c *corpusFlags, m *fixtures.Manifest, counts []int,
	concurrency, repeat, warmup int, results map[int][]runResult) string {

	var b strings.Builder
	fmt.Fprintf(&b, "# Pipeline benchmark\n\n")
	fmt.Fprintf(&b, "Generated by `go run ./cmd/bench pipeline`. Every number below was measured on the machine described; none is estimated.\n\n")
	fmt.Fprintf(&b, "## Setup\n\n")
	for _, l := range describeEnvironment(ctx) {
		fmt.Fprintf(&b, "- %s\n", l)
	}
	mb := float64(corpusBytes(e.hostPath())) / (1 << 20)
	fmt.Fprintf(&b, "- Corpus: %d images (%d scenes) at %dx%d, %.0f MiB on disk, synthetic (seed %d)\n",
		m.Summary.SupportedImages, c.scenes, c.width, c.height, mb, c.seed)
	fmt.Fprintf(&b, "- Each worker runs %d jobs at once; after each rescale, %d unrecorded warm-up run(s), then %d timed runs with the median reported\n\n", concurrency, warmup, repeat)

	fmt.Fprintf(&b, "## Throughput as workers are added\n\n")
	fmt.Fprintf(&b, "Wall time runs from `POST /process` until the queue is empty: every per-photo job for every image, plus the three aggregate stages. Scanning is timed separately.\n\n")
	fmt.Fprintf(&b, "| Workers | Job slots | Wall time (median) | Range | Photos/s | Speedup vs 1 worker | Jobs | Results |\n")
	fmt.Fprintf(&b, "|---:|---:|---:|---|---:|---:|---:|---|\n")

	var base float64
	for _, n := range counts {
		rs := results[n]
		if len(rs) == 0 {
			continue
		}
		walls := make([]float64, len(rs))
		ok := true
		for i, r := range rs {
			walls[i] = r.wall.Seconds()
			if len(r.problems) > 0 {
				ok = false
			}
		}
		med := median(walls)
		lo, hi := minMax(walls)
		if base == 0 {
			base = med
		}
		verdict := "match ground truth"
		if !ok {
			verdict = "**failed verification**"
		}
		speedup := "—"
		if counts[0] == 1 {
			speedup = fmt.Sprintf("%.2fx", base/med)
		}
		fmt.Fprintf(&b, "| %d | %d | %.1f s | %.1f–%.1f s | %.2f | %s | %d | %s |\n",
			n, n*concurrency, med, lo, hi, float64(rs[0].photos)/med, speedup, rs[0].jobs, verdict)
	}

	latencyFor := []int{counts[0]}
	if last := counts[len(counts)-1]; last != counts[0] {
		latencyFor = append(latencyFor, last)
	}
	for _, n := range latencyFor {
		rs := results[n]
		if len(rs) == 0 {
			continue
		}
		last := rs[len(rs)-1]
		fmt.Fprintf(&b, "\n## Per-job latency, %d worker(s) (last run)\n\n", n)
		fmt.Fprintf(&b, "Time from claim to completion for each successful execution, from the queue's own `job_executions` records.\n\n")
		fmt.Fprintf(&b, "| Stage | Executions | p50 | p95 | Mean | Deferrals |\n|---|---:|---:|---:|---:|---:|\n")
		for _, s := range last.stats {
			fmt.Fprintf(&b, "| `%s` | %d | %.0f ms | %.0f ms | %.0f ms | %d |\n", s.jobType, s.n, s.p50, s.p95, s.meanMS, s.deferrals)
		}
	}

	var scans []float64
	for _, n := range counts {
		for _, r := range results[n] {
			scans = append(scans, r.scan.Seconds()*1000)
		}
	}
	if len(scans) > 0 {
		lo, hi := minMax(scans)
		fmt.Fprintf(&b, "\nScanning %d files took a median of %.0f ms (range %.0f–%.0f ms).\n", m.Summary.SupportedImages, median(scans), lo, hi)
	}

	for _, n := range counts {
		for i, r := range results[n] {
			if len(r.problems) > 0 {
				fmt.Fprintf(&b, "\n**Run %d with %d workers failed verification:** %s\n", i+1, n, strings.Join(r.problems, "; "))
			}
		}
	}
	return b.String()
}

// ---------------------------------------------------------------------------
// crash
// ---------------------------------------------------------------------------

func cmdCrash(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("crash", flag.ExitOnError)
	e, c := commonFlags(fs)
	workers := fs.Int("workers", 3, "workers in the fleet; one is killed")
	// 4 rather than the pipeline benchmark's 2: the kill can interrupt at most
	// this many executions, and one interrupted job is a thin demonstration.
	// 3 workers x 4 is 12 slots, within the reference machine's 12
	// performance-core threads.
	concurrency := fs.Int("concurrency", 4, "jobs each worker runs at once")
	killAt := fs.Float64("kill-at", 0.25, "kill once this fraction of jobs has succeeded")
	timeout := fs.Duration("timeout", 30*time.Minute, "overall limit")
	out := fs.String("out", "benchmarks/crash-recovery.md", "report path (empty to skip writing)")
	_ = fs.Parse(args)

	if *workers < 2 {
		return errors.New("-workers must be at least 2: someone has to survive to recover the work")
	}

	m, err := ensureCorpus(e, c)
	if err != nil {
		return err
	}
	originalWorkers, _ := runningWorkerContainers(ctx)
	defer func() {
		if originalWorkers > 0 {
			logf("restoring %d worker(s) with default concurrency", originalWorkers)
			_, _ = compose(context.Background(), nil, "up", "-d", "--no-deps", "--scale", fmt.Sprintf("worker=%d", originalWorkers), "worker")
		}
	}()

	logf("scaling to %d workers x %d concurrency", *workers, *concurrency)
	if err := e.scaleWorkers(ctx, *workers, *concurrency); err != nil {
		return err
	}
	if err := e.resetLibrary(ctx); err != nil {
		return err
	}
	libID, _, err := e.createAndScan(ctx)
	if err != nil {
		return err
	}
	if err := e.post(ctx, "/api/libraries/"+libID+"/process", map[string]any{}, nil); err != nil {
		return err
	}
	start := time.Now()

	// Wait for the kill point: a meaningful share of work done, and work in
	// flight to interrupt.
	logf("waiting until %.0f%% of jobs have succeeded ...", *killAt*100)
	var total int
	for {
		var j jobsResp
		if err := e.get(ctx, "/api/libraries/"+libID+"/jobs", &j); err != nil {
			return err
		}
		total = j.Total.Total
		if j.Total.Pending+j.Total.Running == 0 {
			return errors.New("the pipeline finished before the kill point; use more -scenes or a lower -kill-at")
		}
		if float64(j.Total.Succeeded) >= *killAt*float64(total) && j.Total.Running > 0 {
			break
		}
		if err := sleepCtx(ctx, 100*time.Millisecond); err != nil {
			return err
		}
	}

	// The victim is whichever worker currently holds the most leases, so the
	// kill interrupts as much in-flight work as possible.
	id := mustUUID(libID)
	vrows, err := e.sql(ctx, fmt.Sprintf(`
		SELECT w.id, w.hostname, count(*) FROM jobs j JOIN workers w ON w.id = j.leased_by
		WHERE j.library_id = '%s' AND j.status = 'running'
		GROUP BY w.id, w.hostname ORDER BY count(*) DESC LIMIT 1`, id))
	if err != nil {
		return err
	}
	if len(vrows) == 0 || len(vrows[0]) < 3 {
		return errors.New("no worker held a lease at the kill point; try again")
	}
	victimID, container := mustUUID(vrows[0][0]), vrows[0][1]
	if !regexp.MustCompile(`^[0-9a-f]{12}$`).MatchString(container) {
		return fmt.Errorf("worker hostname %q is not a container id; is it running under compose?", container)
	}

	// Kill time is taken from the DATABASE clock, the same clock that stamps
	// every job execution, so recovery latency is a subtraction between two
	// readings of one clock rather than across a host/container boundary.
	trows, err := e.sql(ctx, "SELECT extract(epoch FROM clock_timestamp())")
	if err != nil {
		return err
	}
	killedAt, _ := strconv.ParseFloat(trows[0][0], 64)

	// SIGKILL: no shutdown hook runs, no lease is released, nothing is flushed.
	// This is a crash, not a stop.
	logf("SIGKILL worker %s (container %s)", victimID[:8], container)
	if _, err := run(ctx, nil, "docker", "kill", "--signal", "KILL", container); err != nil {
		return err
	}

	final, err := e.waitDrained(ctx, libID, *timeout, nil)
	if err != nil {
		return err
	}
	wall := time.Since(start)
	logf("queue drained %s after processing began", wall.Round(time.Second))

	// WHICH WORK WAS INTERRUPTED is read from the record afterwards, not
	// predicted beforehand.
	//
	// The first version listed the victim's leases, then killed it. Between
	// the two sat two `docker compose exec` round trips -- about half a
	// second -- in which the victim finished the listed jobs and claimed new
	// ones. The report then said nothing had been recovered, while the run's
	// extra 60 s of wall time said a lease had expired: the tool had tracked
	// the wrong jobs.
	//
	// The job_executions table has no such race. An execution the victim
	// started and never finished is exactly one the kill interrupted -- the
	// reaper marks it 'abandoned' when it reclaims the lease.
	frows, err := e.sql(ctx, fmt.Sprintf(`
		SELECT j.id, j.status,
		       COALESCE((SELECT extract(epoch FROM min(y.started_at)) FROM job_executions y
		                 WHERE y.job_id = j.id AND y.worker_id <> x.worker_id
		                   AND y.started_at > x.started_at), -1),
		       (SELECT count(*) FROM job_executions y
		        WHERE y.job_id = j.id AND y.worker_id <> x.worker_id AND y.outcome = 'succeeded')
		FROM job_executions x JOIN jobs j ON j.id = x.job_id
		WHERE j.library_id = '%s' AND x.worker_id = '%s'
		  AND (x.outcome IS NULL OR x.outcome = 'abandoned')
		ORDER BY j.id`, id, victimID))
	if err != nil {
		return err
	}

	problems := e.verify(ctx, libID, m, final)

	var latencies []float64
	interrupted, recovered, lost := len(frows), 0, 0
	for _, r := range frows {
		if len(r) < 4 {
			continue
		}
		n, _ := strconv.Atoi(r[3])
		if n > 0 && r[1] == "succeeded" {
			recovered++
			if at, _ := strconv.ParseFloat(r[2], 64); at > 0 {
				latencies = append(latencies, at-killedAt)
			}
		} else {
			lost++
		}
	}
	if interrupted == 0 {
		problems = append(problems, "the kill interrupted no execution, so nothing was demonstrated; run again")
	}
	if lost > 0 {
		problems = append(problems, fmt.Sprintf("%d of the killed worker's interrupted jobs were never completed", lost))
	}

	restarted := "no"
	if outp, err := run(ctx, nil, "docker", "inspect", "-f", "{{.State.Running}} {{.RestartCount}}", container); err == nil {
		f := strings.Fields(outp)
		if len(f) == 2 && f[0] == "true" {
			restarted = "yes, by Docker's restart policy (as a new worker; it does not resume the old one's leases)"
		}
	}

	var b strings.Builder
	fmt.Fprintf(&b, "# Crash recovery\n\n")
	fmt.Fprintf(&b, "Generated by `go run ./cmd/bench crash`. A worker holding leases is killed with SIGKILL partway through a run; the report shows what happened to its work. Every number was measured.\n\n")
	fmt.Fprintf(&b, "## Setup\n\n")
	for _, l := range describeEnvironment(ctx) {
		fmt.Fprintf(&b, "- %s\n", l)
	}
	fmt.Fprintf(&b, "- Corpus: %d images at %dx%d (synthetic, seed %d), %d jobs\n", m.Summary.SupportedImages, c.width, c.height, c.seed, total)
	fmt.Fprintf(&b, "- Fleet: %d workers x %d concurrency; lease 60 s, renewed every 5 s; reaper runs every 15 s\n\n", *workers, *concurrency)

	fmt.Fprintf(&b, "## What happened\n\n")
	fmt.Fprintf(&b, "| | |\n|---|---|\n")
	fmt.Fprintf(&b, "| Killed at | %.0f%% of jobs succeeded |\n", *killAt*100)
	fmt.Fprintf(&b, "| Executions interrupted by the kill | %d |\n", interrupted)
	fmt.Fprintf(&b, "| Re-run and completed by another worker | %d |\n", recovered)
	fmt.Fprintf(&b, "| Lost | %d |\n", lost)
	if len(latencies) > 0 {
		lo, hi := minMax(latencies)
		fmt.Fprintf(&b, "| Kill to reclaim, per job | median %.1f s (range %.1f–%.1f s) |\n", median(latencies), lo, hi)
	}
	fmt.Fprintf(&b, "| Dead jobs at the end | %d of %d |\n", final.Total.Dead, final.Total.Total)
	fmt.Fprintf(&b, "| Whole run, including recovery | %s |\n", wall.Round(time.Second))
	fmt.Fprintf(&b, "| Killed container restarted | %s |\n\n", restarted)

	if len(problems) == 0 {
		fmt.Fprintf(&b, "**Verified.** Every photo passed through every stage, there are no dead jobs, and duplicate and near-duplicate groups match the corpus's ground truth exactly — the crash changed nothing about the result.\n\n")
	} else {
		fmt.Fprintf(&b, "**FAILED VERIFICATION:** %s\n\n", strings.Join(problems, "; "))
	}

	fmt.Fprintf(&b, "## Why the reclaim takes as long as it does\n\n")
	fmt.Fprintf(&b, "A SIGKILLed worker releases nothing, so its work waits on four things in sequence:\n\n")
	fmt.Fprintf(&b, "| Step | Adds |\n|---|---|\n")
	fmt.Fprintf(&b, "| Lease expires: 60 s after its last renewal, which was 0–5 s before the kill | 55–60 s |\n")
	fmt.Fprintf(&b, "| Next reaper pass reclaims it (every surviving worker reaps every 15 s) | 0–15 s |\n")
	fmt.Fprintf(&b, "| First-retry backoff before the job is claimable (2 s ±25%%) | 1.5–2.5 s |\n")
	fmt.Fprintf(&b, "| A worker frees a slot and claims it | about 0–2 s here |\n\n")
	fmt.Fprintf(&b, "That is a window of roughly **57–80 s**. The delay is deliberate: a shorter lease recovers faster but risks reclaiming work from a worker that is merely slow, running it twice. A clean `docker compose stop` releases leases immediately instead of waiting.\n")

	report := b.String()
	fmt.Println(report)
	if err := writeReport(*out, report); err != nil {
		return err
	}
	if len(problems) > 0 {
		return errors.New("crash recovery failed verification")
	}
	return nil
}

// ---------------------------------------------------------------------------
// clean
// ---------------------------------------------------------------------------

func cmdClean(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("clean", flag.ExitOnError)
	e, _ := commonFlags(fs)
	_ = fs.Parse(args)
	if err := e.resetLibrary(ctx); err != nil {
		return err
	}
	if err := os.RemoveAll(e.hostPath()); err != nil {
		return err
	}
	logf("removed %s and its library", e.hostPath())
	return nil
}
