// Command photo-organizer is the operator CLI.
//
// It is a thin HTTP client, not a second implementation. Everything it does
// goes through the same API the web UI uses, which means: no duplicated
// business logic, no second database connection to keep in sync, and it keeps
// working unchanged if the API ever moves off this machine.
package main

import (
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
  photo-organizer [flags] <command>

Commands:
  status     Show API and database health
  version    Print the client version

Flags:
  -api string    Base URL of the API (default $API_BASE_URL or http://localhost:8080)
  -timeout dur   Request timeout (default 10s)
`

// version is overridden at build time with -ldflags "-X main.version=..."
var version = "dev"

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("photo-organizer", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() { fmt.Fprint(os.Stderr, usage) }

	defaultAPI := os.Getenv("API_BASE_URL")
	if defaultAPI == "" {
		defaultAPI = "http://localhost:8080"
	}

	apiBase := fs.String("api", defaultAPI, "base URL of the API")
	timeout := fs.Duration("timeout", 10*time.Second, "request timeout")

	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() == 0 {
		fs.Usage()
		return errors.New("no command given")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	c := &client{base: strings.TrimRight(*apiBase, "/"), http: &http.Client{Timeout: *timeout}}

	switch cmd := fs.Arg(0); cmd {
	case "status":
		return cmdStatus(ctx, c, out)
	case "version":
		fmt.Fprintf(out, "photo-organizer %s\n", version)
		return nil
	default:
		fs.Usage()
		return fmt.Errorf("unknown command %q", cmd)
	}
}

type client struct {
	base string
	http *http.Client
}

// get performs a GET and decodes the JSON body into v. It returns the HTTP
// status alongside the error so callers can distinguish "the API said no" from
// "the API is unreachable" -- a distinction that matters a lot when the whole
// point of the command is diagnosing which one it is.
func (c *client) get(ctx context.Context, path string, v any) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return 0, fmt.Errorf("requesting %s: %w", c.base+path, err)
	}
	defer resp.Body.Close()

	// Cap the body: a misconfigured -api pointing at something that streams
	// forever should not exhaust memory.
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return resp.StatusCode, fmt.Errorf("reading %s: %w", path, err)
	}
	if v != nil && len(body) > 0 {
		if err := json.Unmarshal(body, v); err != nil {
			return resp.StatusCode, fmt.Errorf("decoding %s (status %d): %w", path, resp.StatusCode, err)
		}
	}
	return resp.StatusCode, nil
}

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
	if err != nil {
		fmt.Fprintf(out, "Database UNKNOWN\n")
		return err
	}
	if status != http.StatusOK {
		fmt.Fprintf(out, "Database DOWN (%s)\n", ready.Detail)
		return errors.New("API is not ready")
	}
	fmt.Fprintf(out, "Database UP (%dms)\n", ready.LatencyMS)
	return nil
}

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
