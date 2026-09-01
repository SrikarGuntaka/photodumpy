// Command worker is the job-processing binary.
//
// PHASE 1 STATUS: THIS IS A DELIBERATE STUB.
//
// The real worker -- registration, heartbeats, lease renewal, bounded-concurrency
// job execution and crash recovery -- arrives in Phase 5. What exists today is
// only the process shell: configuration, database connection, and graceful
// shutdown. It claims no jobs and processes nothing, and it says so in its logs.
//
// It exists now rather than in Phase 5 so that the Docker image, Compose wiring
// and shutdown behaviour are exercised from the start, and because the signal
// handling below is real code that Phase 5 builds on rather than replaces.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/srikarguntaka/photo-organizer/internal/config"
	"github.com/srikarguntaka/photo-organizer/internal/database"
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

	hostname, _ := os.Hostname()
	log := cfg.NewLogger("worker").With("hostname", hostname, "pid", os.Getpid())

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := database.Connect(ctx, cfg.DatabaseURL, cfg.DBMaxConns, cfg.DBConnectTimeout, log)
	if err != nil {
		return err
	}
	defer pool.Close()

	// Workers never run migrations. Only the API does, so schema changes have
	// exactly one owner and scaling workers cannot cause a migration stampede.
	log.Warn("worker is a Phase 1 stub: it is connected but claims no jobs (job queue lands in Phase 5)")

	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Info("shutdown signal received, stopping cleanly")
			return nil
		case <-ticker.C:
			// Proves the database connection survives idling and that a
			// Postgres restart is recovered from, which is worth verifying
			// before the real work depends on it.
			pingCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
			err := pool.Ping(pingCtx)
			cancel()
			if err != nil {
				log.Error("database ping failed", "error", err)
				continue
			}
			log.Debug("idle: database reachable")
		}
	}
}
