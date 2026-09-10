// Command worker consumes jobs from the PostgreSQL-backed queue.
//
// Scale it with:
//
//	docker compose up -d --scale worker=4
//
// Killing one mid-flight (`docker compose kill worker`) is the crash path: its
// leases expire and another worker picks the work up. Stopping one
// (`docker compose stop worker`) is the clean path: it releases its leases
// immediately and the work is reassigned in milliseconds.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"runtime"
	"syscall"

	"github.com/srikarguntaka/photo-organizer/internal/config"
	"github.com/srikarguntaka/photo-organizer/internal/database"
	"github.com/srikarguntaka/photo-organizer/internal/ingest"
	"github.com/srikarguntaka/photo-organizer/internal/store"
	"github.com/srikarguntaka/photo-organizer/internal/worker"
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

	concurrency := cfg.ProcessConcurrency
	if concurrency <= 0 {
		concurrency = runtime.NumCPU()
	}

	// The pool is sized to concurrency plus headroom. The headroom is not
	// arbitrary: heartbeat, lease renewal and the reaper each need a connection
	// on a schedule, and if job execution could consume every connection those
	// loops would stall -- a worker that cannot renew its leases loses its
	// work while still holding it.
	pool, err := database.Connect(ctx, cfg.DatabaseURL, int32(concurrency+4), cfg.DBConnectTimeout, log)
	if err != nil {
		return err
	}
	defer pool.Close()

	// Workers never run migrations. The API owns the schema, so scaling to 16
	// workers cannot cause a migration stampede.

	st := store.New(pool)
	processor := ingest.NewProcessor(st, log, ingest.ProcessorOptions{Concurrency: concurrency})

	w := worker.New(st, log, worker.Options{
		Concurrency:   concurrency,
		ShutdownGrace: cfg.ShutdownGrace,
	})
	worker.NewHandlers(st, processor, log, cfg.SimilarityThreshold).RegisterAll(w)

	return w.Run(ctx)
}
