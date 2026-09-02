package store

import (
	"context"
	"fmt"

	"github.com/srikarguntaka/photo-organizer/internal/photos"
)

const libraryColumns = `
	id, name, source_kind, root_path,
	last_scan_started_at, last_scan_finished_at,
	created_at, updated_at`

// CreateLibrary inserts a library, or returns the existing one for the same
// (source_kind, root_path).
//
// "Scan this folder" run twice must not create two libraries, and making the
// caller do a read-then-write would be a race. ON CONFLICT DO UPDATE (rather
// than DO NOTHING) is used so the row is always RETURNED -- with DO NOTHING a
// conflicting insert returns zero rows and would need a second query.
func (s *Store) CreateLibrary(ctx context.Context, name, rootPath string, kind photos.SourceKind) (*photos.Library, bool, error) {
	const q = `
		INSERT INTO libraries (name, source_kind, root_path)
		VALUES ($1, $2, $3)
		ON CONFLICT (source_kind, root_path) DO UPDATE
			-- A no-op update that still returns the row. Setting name to its
			-- existing value keeps an existing library's name untouched when a
			-- rescan is requested with a different one.
			SET name = libraries.name
		RETURNING ` + libraryColumns + `, (xmax = 0) AS inserted`

	var l photos.Library
	var created bool
	err := s.pool.QueryRow(ctx, q, name, string(kind), rootPath).Scan(
		&l.ID, &l.Name, &l.SourceKind, &l.RootPath,
		&l.LastScanStartedAt, &l.LastScanFinishedAt,
		&l.CreatedAt, &l.UpdatedAt,
		&created,
	)
	if err != nil {
		return nil, false, fmt.Errorf("store: creating library: %w", normaliseErr(err))
	}
	return &l, created, nil
}

// GetLibrary looks up one library by id.
func (s *Store) GetLibrary(ctx context.Context, id string) (*photos.Library, error) {
	const q = `SELECT ` + libraryColumns + ` FROM libraries WHERE id = $1`

	var l photos.Library
	err := s.pool.QueryRow(ctx, q, id).Scan(
		&l.ID, &l.Name, &l.SourceKind, &l.RootPath,
		&l.LastScanStartedAt, &l.LastScanFinishedAt,
		&l.CreatedAt, &l.UpdatedAt,
	)
	if err != nil {
		return nil, normaliseErr(err)
	}
	return &l, nil
}

// ListLibraries returns every library, newest first.
func (s *Store) ListLibraries(ctx context.Context) ([]photos.Library, error) {
	const q = `SELECT ` + libraryColumns + ` FROM libraries ORDER BY created_at DESC`

	rows, err := s.pool.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("store: listing libraries: %w", err)
	}
	defer rows.Close()

	// Non-nil empty slice so the API renders [] rather than null.
	out := []photos.Library{}
	for rows.Next() {
		var l photos.Library
		if err := rows.Scan(
			&l.ID, &l.Name, &l.SourceKind, &l.RootPath,
			&l.LastScanStartedAt, &l.LastScanFinishedAt,
			&l.CreatedAt, &l.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("store: scanning library: %w", err)
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// MarkScanStarted records that a scan has begun, and reports whether it
// actually claimed the scan.
//
// The guard is in the WHERE clause, not in application code, so two concurrent
// requests cannot both believe they started the scan: exactly one UPDATE
// matches. This is the same claim-by-update pattern the job queue uses in
// Phase 5, on a smaller scale.
//
// force bypasses the guard, for recovering a library left marked "scanning" by
// a process that died.
func (s *Store) MarkScanStarted(ctx context.Context, id string, force bool) (bool, error) {
	const q = `
		UPDATE libraries
		SET last_scan_started_at  = now(),
		    last_scan_finished_at = NULL
		WHERE id = $1
		  AND ($2
		       OR last_scan_started_at IS NULL
		       OR last_scan_finished_at IS NOT NULL)`

	tag, err := s.pool.Exec(ctx, q, id, force)
	if err != nil {
		return false, fmt.Errorf("store: marking scan started: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// MarkScanFinished closes out a scan.
//
// Uses context.WithoutCancel at the call site rather than here: this must run
// even when the scan was cancelled, or the library stays marked "scanning"
// forever and the next scan is refused.
func (s *Store) MarkScanFinished(ctx context.Context, id string) error {
	const q = `UPDATE libraries SET last_scan_finished_at = now() WHERE id = $1`

	if _, err := s.pool.Exec(ctx, q, id); err != nil {
		return fmt.Errorf("store: marking scan finished: %w", err)
	}
	return nil
}

// ReconcileInterruptedScans closes out scans left open by a process that died.
//
// Called once at API startup. Without it, a library whose scan was interrupted
// by a crash stays marked "scanning" indefinitely and MarkScanStarted refuses
// every subsequent scan. Returns how many were reconciled so the caller can
// log it -- silently fixing this would hide a crash.
func (s *Store) ReconcileInterruptedScans(ctx context.Context) (int, error) {
	const q = `
		UPDATE libraries
		SET last_scan_finished_at = now()
		WHERE last_scan_started_at IS NOT NULL
		  AND last_scan_finished_at IS NULL`

	tag, err := s.pool.Exec(ctx, q)
	if err != nil {
		return 0, fmt.Errorf("store: reconciling interrupted scans: %w", err)
	}
	return int(tag.RowsAffected()), nil
}
