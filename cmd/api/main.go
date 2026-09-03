// Command api serves the HTTP API and applies database migrations at startup.
package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/srikarguntaka/photo-organizer/internal/api"
	"github.com/srikarguntaka/photo-organizer/internal/config"
	"github.com/srikarguntaka/photo-organizer/internal/database"
	"github.com/srikarguntaka/photo-organizer/internal/ingest"
	"github.com/srikarguntaka/photo-organizer/internal/store"
	"github.com/srikarguntaka/photo-organizer/migrations"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "fatal: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	log := cfg.NewLogger("api")

	// NotifyContext cancels ctx on SIGINT/SIGTERM. Everything below takes ctx,
	// so a Ctrl-C during startup (e.g. while retrying a database connection)
	// exits promptly instead of hanging for the full timeout.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	log.Info("starting", "env", cfg.Env, "addr", cfg.HTTPAddr, "photo_root", cfg.PhotoRoot)

	pool, err := database.Connect(ctx, cfg.DatabaseURL, cfg.DBMaxConns, cfg.DBConnectTimeout, log)
	if err != nil {
		return err
	}
	defer pool.Close()

	// Migrations run in the API process, guarded by a Postgres advisory lock so
	// concurrent replicas cannot race. For a single-user local tool this beats
	// a separate migration container that has to be sequenced by hand.
	if err := database.Migrate(ctx, pool, migrations.FS(), log); err != nil {
		return err
	}

	st := store.New(pool)

	// A scan interrupted by a crash leaves its library marked "scanning"
	// forever, which would make MarkScanStarted refuse every future scan.
	// Reconciling at startup is the only place that can distinguish "the
	// process died" from "a scan is genuinely running", because a fresh
	// process by definition owns no scans.
	if n, err := st.ReconcileInterruptedScans(ctx); err != nil {
		return err
	} else if n > 0 {
		log.Warn("reconciled scans interrupted by a previous shutdown", "libraries", n)
	}

	scanner := ingest.NewScanner(st, log)
	processor := ingest.NewProcessor(st, log, ingest.ProcessorOptions{
		Concurrency: cfg.ProcessConcurrency,
	})

	srv := &http.Server{
		Addr:    cfg.HTTPAddr,
		Handler: api.NewServer(cfg, pool, st, scanner, processor, log).Handler(),
		// Without these, a slow or malicious client can hold a connection open
		// indefinitely. ReadHeaderTimeout in particular is the guard against
		// Slowloris.
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		log.Info("http listening", "addr", cfg.HTTPAddr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- fmt.Errorf("http server: %w", err)
			return
		}
		errCh <- nil
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		log.Info("shutdown signal received", "grace", cfg.ShutdownGrace)
	}

	// Graceful shutdown: stop accepting new connections, let in-flight requests
	// finish, then force-close whatever is left. Note the fresh context --
	// ctx is already cancelled at this point.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownGrace)
	defer cancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Error("graceful shutdown timed out, forcing close", "error", err)
		_ = srv.Close()
		return fmt.Errorf("shutdown: %w", err)
	}

	// Stop in-flight scans after the HTTP server has drained, so no new scan
	// can start while we are winding down. Cancelled scans lose no work: rows
	// already inserted stay, and the next scan resumes because inserts are
	// idempotent.
	if err := scanner.Shutdown(shutdownCtx); err != nil {
		log.Warn("scans did not stop cleanly", "error", err)
	}
	if err := processor.Shutdown(shutdownCtx); err != nil {
		log.Warn("metadata extraction did not stop cleanly", "error", err)
	}

	log.Info("stopped cleanly")
	return nil
}
