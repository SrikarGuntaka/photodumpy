// Package database owns the Postgres connection pool and the migration runner.
// It is the only package that knows how to reach the database; everything above
// it receives a *pgxpool.Pool.
package database

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Connect opens a connection pool and blocks until Postgres actually answers a
// ping, or until the timeout elapses.
//
// The retry loop is not defensive padding: in Compose the API starts as soon as
// the Postgres *container* is healthy, which is a few hundred milliseconds
// before Postgres has finished recovery and is accepting connections. The same
// loop is what lets the API survive a `docker compose restart postgres` without
// needing to be restarted itself.
func Connect(ctx context.Context, dsn string, maxConns int32, timeout time.Duration, log *slog.Logger) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("database: parsing DATABASE_URL: %w", err)
	}
	cfg.MaxConns = maxConns
	// A connection that has been idle for a long time is more likely to have
	// been closed by the server or an intermediary; recycling proactively turns
	// a request-time error into a background one.
	cfg.MaxConnIdleTime = 5 * time.Minute
	cfg.MaxConnLifetime = time.Hour
	cfg.HealthCheckPeriod = 30 * time.Second

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("database: creating pool: %w", err)
	}

	deadline := time.Now().Add(timeout)
	backoff := 250 * time.Millisecond
	attempt := 0
	for {
		attempt++
		pingCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		err = pool.Ping(pingCtx)
		cancel()
		if err == nil {
			log.Info("database connected", "attempts", attempt, "max_conns", maxConns)
			return pool, nil
		}

		if time.Now().After(deadline) {
			pool.Close()
			return nil, fmt.Errorf("database: unreachable after %s (%d attempts): %w", timeout, attempt, err)
		}

		log.Warn("database not ready, retrying", "attempt", attempt, "backoff", backoff, "error", err)

		select {
		case <-ctx.Done():
			pool.Close()
			return nil, ctx.Err()
		case <-time.After(backoff):
		}

		// Exponential backoff, capped. Capped because we want the retry cadence
		// to stay responsive once Postgres does come back.
		if backoff < 4*time.Second {
			backoff *= 2
		}
	}
}
