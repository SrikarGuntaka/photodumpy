package database

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"log/slog"
	"path"
	"sort"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

// migrationLockID is an arbitrary but fixed key for pg_advisory_lock. Any
// process running migrations takes this lock first, so two API replicas (or an
// API and a `make migrate`) starting simultaneously cannot both try to create
// the same table. The loser blocks, then finds nothing left to apply.
const migrationLockID int64 = 0x504F524741 // "PORGA"

// Migration is one embedded .sql file.
type Migration struct {
	Version  int
	Name     string
	SQL      string
	Checksum string
}

// Migrate applies every pending migration from fsys, in version order, each in
// its own transaction.
//
// Two properties worth calling out:
//
//  1. Each migration runs inside a transaction, so a migration that fails
//     halfway leaves the schema exactly as it was. Postgres has transactional
//     DDL, which is precisely why this is worth doing by hand rather than
//     shelling out to a tool.
//  2. The checksum of every applied file is stored. If a migration that has
//     already run is edited afterwards, we refuse to start rather than silently
//     running a schema that does not match the files in the repo.
func Migrate(ctx context.Context, pool *pgxpool.Pool, fsys fs.FS, log *slog.Logger) error {
	migrations, err := load(fsys)
	if err != nil {
		return err
	}
	if len(migrations) == 0 {
		return fmt.Errorf("database: no migrations found -- the embed is empty")
	}

	conn, err := pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("database: acquiring connection for migration: %w", err)
	}
	defer conn.Release()

	// The advisory lock is held on this specific connection for the whole run
	// and released explicitly below (it would also be released when the
	// connection returns to the pool and is reset, but being explicit means the
	// lock is not held for the lifetime of a pooled connection).
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, migrationLockID); err != nil {
		return fmt.Errorf("database: acquiring migration advisory lock: %w", err)
	}
	defer func() {
		if _, err := conn.Exec(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock($1)`, migrationLockID); err != nil {
			log.Error("releasing migration advisory lock", "error", err)
		}
	}()

	if _, err := conn.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version     integer     PRIMARY KEY,
			name        text        NOT NULL,
			checksum    text        NOT NULL,
			applied_at  timestamptz NOT NULL DEFAULT now()
		)`); err != nil {
		return fmt.Errorf("database: creating schema_migrations: %w", err)
	}

	applied := map[int]string{}
	rows, err := conn.Query(ctx, `SELECT version, checksum FROM schema_migrations`)
	if err != nil {
		return fmt.Errorf("database: reading schema_migrations: %w", err)
	}
	for rows.Next() {
		var v int
		var sum string
		if err := rows.Scan(&v, &sum); err != nil {
			rows.Close()
			return fmt.Errorf("database: scanning schema_migrations: %w", err)
		}
		applied[v] = sum
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("database: reading schema_migrations: %w", err)
	}

	pending := 0
	for _, m := range migrations {
		if sum, ok := applied[m.Version]; ok {
			if sum != m.Checksum {
				return fmt.Errorf(
					"database: migration %04d_%s was already applied but its contents changed "+
						"(recorded %s, on disk %s). Applied migrations are immutable -- add a new one instead",
					m.Version, m.Name, sum[:12], m.Checksum[:12])
			}
			continue
		}

		start := log.With("version", m.Version, "name", m.Name)
		start.Info("applying migration")

		if err := applyOne(ctx, conn, m); err != nil {
			return err
		}
		pending++
	}

	log.Info("migrations up to date", "applied_now", pending, "total", len(migrations))
	return nil
}

func applyOne(ctx context.Context, conn *pgxpool.Conn, m Migration) error {
	tx, err := conn.Begin(ctx)
	if err != nil {
		return fmt.Errorf("database: begin for migration %04d: %w", m.Version, err)
	}
	// Rollback is a no-op after a successful Commit.
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	if _, err := tx.Exec(ctx, m.SQL); err != nil {
		return fmt.Errorf("database: migration %04d_%s failed: %w", m.Version, m.Name, err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO schema_migrations (version, name, checksum) VALUES ($1, $2, $3)`,
		m.Version, m.Name, m.Checksum,
	); err != nil {
		return fmt.Errorf("database: recording migration %04d: %w", m.Version, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("database: commit for migration %04d: %w", m.Version, err)
	}
	return nil
}

// load reads every *.sql file at the root of fsys. Filenames must look like
// "0001_description.sql"; anything else is an error rather than a silent skip,
// because a migration that does not run is the kind of bug that surfaces in
// production three weeks later.
func load(fsys fs.FS) ([]Migration, error) {
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return nil, fmt.Errorf("database: reading migrations dir: %w", err)
	}

	var out []Migration
	seen := map[int]string{}

	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if path.Ext(name) != ".sql" {
			continue
		}

		base := strings.TrimSuffix(name, ".sql")
		idx := strings.Index(base, "_")
		if idx <= 0 {
			return nil, fmt.Errorf("database: migration %q must be named <version>_<description>.sql", name)
		}
		version, err := strconv.Atoi(base[:idx])
		if err != nil {
			return nil, fmt.Errorf("database: migration %q has a non-numeric version prefix", name)
		}
		if prev, dup := seen[version]; dup {
			return nil, fmt.Errorf("database: migrations %q and %q share version %d", prev, name, version)
		}
		seen[version] = name

		body, err := fs.ReadFile(fsys, name)
		if err != nil {
			return nil, fmt.Errorf("database: reading migration %q: %w", name, err)
		}
		sum := sha256.Sum256(body)

		out = append(out, Migration{
			Version:  version,
			Name:     base[idx+1:],
			SQL:      string(body),
			Checksum: hex.EncodeToString(sum[:]),
		})
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })
	return out, nil
}
