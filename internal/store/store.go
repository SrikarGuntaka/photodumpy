// Package store holds every SQL statement in the project.
//
// Nothing outside this package imports pgx. That boundary is what lets the
// algorithm packages (hashing, quality, clustering, duplicates) be tested with
// plain values and no database, and it means a query can be found by grepping
// one directory rather than the whole tree.
package store

import (
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrNotFound is returned when a lookup matches no row. Callers map it to a
// 404; anything else from this package is a genuine server fault.
var ErrNotFound = errors.New("store: not found")

// ErrConflict is returned when a write violates a uniqueness constraint that
// the caller is expected to handle (for example, creating a library for a path
// that already has one).
var ErrConflict = errors.New("store: conflicting row already exists")

// Store is the data access layer.
type Store struct {
	pool *pgxpool.Pool
}

func New(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// Pool exposes the underlying pool for the few callers that legitimately need
// it (health checks, integration test helpers). Regular code should use the
// methods on Store.
func (s *Store) Pool() *pgxpool.Pool { return s.pool }

// isUniqueViolation reports whether err is a Postgres unique-constraint error.
// Checking the SQLSTATE rather than matching on the message keeps this working
// across Postgres versions and locales.
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == "23505"
	}
	return false
}

// normaliseErr maps pgx sentinel errors onto this package's own, so callers
// never need to import pgx to interpret a result.
func normaliseErr(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, pgx.ErrNoRows):
		return ErrNotFound
	case isUniqueViolation(err):
		return ErrConflict
	default:
		return err
	}
}
