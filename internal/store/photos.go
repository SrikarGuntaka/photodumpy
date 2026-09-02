package store

import (
	"context"
	"fmt"
	"time"

	"github.com/srikarguntaka/photo-organizer/internal/photos"
)

const photoColumns = `
	id, library_id, relative_path, original_filename,
	file_size_bytes, file_modified_at, detected_format,
	state, last_error, created_at, updated_at`

// InsertPhotoBatch inserts a batch of discovered photos, skipping any that are
// already known, and returns how many rows were actually new.
//
// Two decisions worth explaining.
//
// Batching: inserting one row per round trip makes a 10,000-photo scan 10,000
// round trips, which dominates the runtime even on localhost. Batching into
// arrays and unnesting them server-side makes it one round trip per batch. The
// arrays are passed as parameters rather than interpolated, so this is not
// string-built SQL.
//
// ON CONFLICT DO NOTHING: this is what makes re-scanning idempotent. The
// unique index on (library_id, relative_path) is the authority on "have we
// seen this file", so a second scan of an unchanged folder inserts nothing and
// the RowsAffected count proves it rather than us asserting it.
//
// Deliberately NOT updated on conflict: if a file's size or mtime changed, the
// row keeps its original values here. Detecting and handling changed files is
// a real concern, but it belongs with content hashing in Phase 4 where we can
// tell an edited photo from a touched timestamp. Silently overwriting now
// would throw away the ability to notice.
func (s *Store) InsertPhotoBatch(ctx context.Context, libraryID string, batch []photos.Discovered) (int, error) {
	if len(batch) == 0 {
		return 0, nil
	}

	relPaths := make([]string, len(batch))
	names := make([]string, len(batch))
	sizes := make([]int64, len(batch))
	modTimes := make([]*time.Time, len(batch))
	formats := make([]string, len(batch))

	for i, d := range batch {
		relPaths[i] = d.RelPath
		names[i] = d.Name
		sizes[i] = d.Size
		formats[i] = string(d.Format)
		if d.ModTime > 0 {
			t := time.Unix(d.ModTime, 0).UTC()
			modTimes[i] = &t
		}
	}

	const q = `
		INSERT INTO photos (
			library_id, relative_path, original_filename,
			file_size_bytes, file_modified_at, detected_format
		)
		SELECT $1, rel, name, size, modified, format
		FROM unnest($2::text[], $3::text[], $4::bigint[], $5::timestamptz[], $6::text[])
			AS t(rel, name, size, modified, format)
		ON CONFLICT (library_id, relative_path) DO NOTHING`

	tag, err := s.pool.Exec(ctx, q, libraryID, relPaths, names, sizes, modTimes, formats)
	if err != nil {
		return 0, fmt.Errorf("store: inserting photo batch: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// CountPhotos returns the number of photos in a library.
func (s *Store) CountPhotos(ctx context.Context, libraryID string) (int, error) {
	const q = `SELECT count(*) FROM photos WHERE library_id = $1`

	var n int
	if err := s.pool.QueryRow(ctx, q, libraryID).Scan(&n); err != nil {
		return 0, fmt.Errorf("store: counting photos: %w", err)
	}
	return n, nil
}

// CountPhotosByState returns photo counts grouped by state, for progress
// reporting. Backed by the (library_id, state) index.
func (s *Store) CountPhotosByState(ctx context.Context, libraryID string) (map[photos.State]int, error) {
	const q = `
		SELECT state, count(*)
		FROM photos
		WHERE library_id = $1
		GROUP BY state`

	rows, err := s.pool.Query(ctx, q, libraryID)
	if err != nil {
		return nil, fmt.Errorf("store: counting photos by state: %w", err)
	}
	defer rows.Close()

	out := map[photos.State]int{}
	for rows.Next() {
		var state photos.State
		var n int
		if err := rows.Scan(&state, &n); err != nil {
			return nil, fmt.Errorf("store: scanning state count: %w", err)
		}
		out[state] = n
	}
	return out, rows.Err()
}

// ListPhotosOptions controls pagination and filtering.
type ListPhotosOptions struct {
	Limit  int
	Offset int
	State  photos.State // empty means all states
}

// ListPhotos returns a page of photos ordered by relative path.
//
// Ordering by relative_path rather than created_at gives a stable, meaningful
// order that matches what the user sees in their file browser, and it is
// deterministic across rescans. Paginating on an unstable order silently skips
// and repeats rows between pages.
func (s *Store) ListPhotos(ctx context.Context, libraryID string, opts ListPhotosOptions) ([]photos.Photo, error) {
	if opts.Limit <= 0 || opts.Limit > 500 {
		opts.Limit = 100
	}
	if opts.Offset < 0 {
		opts.Offset = 0
	}

	// A single query with an optional filter, rather than two near-identical
	// query strings: $4 = '' disables the state predicate.
	const q = `
		SELECT ` + photoColumns + `
		FROM photos
		WHERE library_id = $1
		  AND ($4 = '' OR state = $4::photo_state)
		ORDER BY relative_path
		LIMIT $2 OFFSET $3`

	rows, err := s.pool.Query(ctx, q, libraryID, opts.Limit, opts.Offset, string(opts.State))
	if err != nil {
		return nil, fmt.Errorf("store: listing photos: %w", err)
	}
	defer rows.Close()

	out := []photos.Photo{}
	for rows.Next() {
		var p photos.Photo
		if err := rows.Scan(
			&p.ID, &p.LibraryID, &p.RelativePath, &p.OriginalFilename,
			&p.FileSizeBytes, &p.FileModifiedAt, &p.DetectedFormat,
			&p.State, &p.LastError, &p.CreatedAt, &p.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("store: scanning photo: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// GetPhoto looks up one photo by id.
func (s *Store) GetPhoto(ctx context.Context, id string) (*photos.Photo, error) {
	const q = `SELECT ` + photoColumns + ` FROM photos WHERE id = $1`

	var p photos.Photo
	err := s.pool.QueryRow(ctx, q, id).Scan(
		&p.ID, &p.LibraryID, &p.RelativePath, &p.OriginalFilename,
		&p.FileSizeBytes, &p.FileModifiedAt, &p.DetectedFormat,
		&p.State, &p.LastError, &p.CreatedAt, &p.UpdatedAt,
	)
	if err != nil {
		return nil, normaliseErr(err)
	}
	return &p, nil
}
