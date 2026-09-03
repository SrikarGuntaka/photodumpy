package store

import (
	"context"
	"fmt"
	"time"

	"github.com/srikarguntaka/photo-organizer/internal/photos"
)

// photoColumns and scanPhoto must stay in lockstep -- the column list and the
// Scan destinations are positional, so adding a column to one without the
// other is a runtime error rather than a compile error. Keeping them adjacent
// is the cheapest guard available short of a row-mapping library.
const photoColumns = `
	id, library_id, relative_path, original_filename,
	file_size_bytes, file_modified_at, detected_format,
	width, height, orientation,
	captured_at, captured_at_source,
	latitude, longitude,
	camera_make, camera_model,
	metadata_extracted_at,
	state, last_error, created_at, updated_at`

// scanPhoto reads one row selected with photoColumns.
func scanPhoto(row interface{ Scan(...any) error }) (photos.Photo, error) {
	var p photos.Photo
	err := row.Scan(
		&p.ID, &p.LibraryID, &p.RelativePath, &p.OriginalFilename,
		&p.FileSizeBytes, &p.FileModifiedAt, &p.DetectedFormat,
		&p.Width, &p.Height, &p.Orientation,
		&p.CapturedAt, &p.CapturedAtSource,
		&p.Latitude, &p.Longitude,
		&p.CameraMake, &p.CameraModel,
		&p.MetadataExtractedAt,
		&p.State, &p.LastError, &p.CreatedAt, &p.UpdatedAt,
	)
	return p, err
}

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
		p, err := scanPhoto(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scanning photo: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// GetPhoto looks up one photo by id.
func (s *Store) GetPhoto(ctx context.Context, id string) (*photos.Photo, error) {
	const q = `SELECT ` + photoColumns + ` FROM photos WHERE id = $1`

	p, err := scanPhoto(s.pool.QueryRow(ctx, q, id))
	if err != nil {
		return nil, normaliseErr(err)
	}
	return &p, nil
}

// PhotoNeedingMetadata is the minimal row the metadata processor needs: enough
// to open the file and fall back to its mtime, nothing more. Selecting only
// these columns keeps the working set small when a library has 100,000 photos.
type PhotoNeedingMetadata struct {
	ID             string
	RelativePath   string
	FileModifiedAt *time.Time
}

// ListPhotosNeedingMetadata returns photos that have not been through metadata
// extraction yet, oldest first.
//
// Backed by the partial index on (library_id, id) WHERE metadata_extracted_at
// IS NULL, so the query cost is proportional to the remaining work rather than
// to the size of the library -- it gets faster as processing progresses.
func (s *Store) ListPhotosNeedingMetadata(ctx context.Context, libraryID string, limit int) ([]PhotoNeedingMetadata, error) {
	if limit <= 0 || limit > 5000 {
		limit = 500
	}

	const q = `
		SELECT id, relative_path, file_modified_at
		FROM photos
		WHERE library_id = $1
		  AND metadata_extracted_at IS NULL
		ORDER BY id
		LIMIT $2`

	rows, err := s.pool.Query(ctx, q, libraryID, limit)
	if err != nil {
		return nil, fmt.Errorf("store: listing photos needing metadata: %w", err)
	}
	defer rows.Close()

	out := []PhotoNeedingMetadata{}
	for rows.Next() {
		var p PhotoNeedingMetadata
		if err := rows.Scan(&p.ID, &p.RelativePath, &p.FileModifiedAt); err != nil {
			return nil, fmt.Errorf("store: scanning photo: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// PhotoMetadata is the result of extraction, ready to persist.
type PhotoMetadata struct {
	Width            *int
	Height           *int
	Orientation      *int
	DetectedFormat   *string
	CapturedAt       *time.Time
	CapturedAtSource *string
	Latitude         *float64
	Longitude        *float64
	CameraMake       *string
	CameraModel      *string
	State            photos.State
	LastError        *string
}

// UpdatePhotoMetadata writes extraction results for one photo.
//
// Idempotent by construction: it is a full-row overwrite of the derived
// columns keyed by id, so running it twice writes identical values. That is
// what makes the at-least-once delivery of Phase 5 safe -- a job re-run after
// a lease expiry simply recomputes the same answer and overwrites it.
//
// metadata_extracted_at is set unconditionally, including on failure. A photo
// whose file is corrupt has still been through extraction; leaving the marker
// NULL would make the processor retry it forever.
func (s *Store) UpdatePhotoMetadata(ctx context.Context, photoID string, m PhotoMetadata) error {
	const q = `
		UPDATE photos SET
			width                 = $2,
			height                = $3,
			orientation           = $4,
			detected_format       = COALESCE($5, detected_format),
			captured_at           = $6,
			captured_at_source    = $7,
			latitude              = $8,
			longitude             = $9,
			camera_make           = $10,
			camera_model          = $11,
			state                 = $12,
			last_error            = $13,
			metadata_extracted_at = now()
		WHERE id = $1`

	_, err := s.pool.Exec(ctx, q, photoID,
		m.Width, m.Height, m.Orientation, m.DetectedFormat,
		m.CapturedAt, m.CapturedAtSource,
		m.Latitude, m.Longitude,
		m.CameraMake, m.CameraModel,
		string(m.State), m.LastError,
	)
	if err != nil {
		return fmt.Errorf("store: updating photo metadata: %w", err)
	}
	return nil
}

// CountPhotosNeedingMetadata reports outstanding extraction work, for progress
// reporting.
func (s *Store) CountPhotosNeedingMetadata(ctx context.Context, libraryID string) (int, error) {
	const q = `SELECT count(*) FROM photos WHERE library_id = $1 AND metadata_extracted_at IS NULL`

	var n int
	if err := s.pool.QueryRow(ctx, q, libraryID).Scan(&n); err != nil {
		return 0, fmt.Errorf("store: counting photos needing metadata: %w", err)
	}
	return n, nil
}

// MetadataSummary describes what extraction found across a library, for the
// progress endpoint and the CLI.
type MetadataSummary struct {
	Total        int `json:"total"`
	Extracted    int `json:"extracted"`
	Pending      int `json:"pending"`
	WithEXIFDate int `json:"with_exif_date"`
	WithFileDate int `json:"with_file_date"`
	WithNoDate   int `json:"with_no_date"`
	WithGPS      int `json:"with_gps"`
	Failed       int `json:"failed"`
}

// SummariseMetadata computes the whole summary in one query rather than six.
// FILTER is the readable way to write conditional aggregates in Postgres, and
// keeps this to a single sequential pass.
func (s *Store) SummariseMetadata(ctx context.Context, libraryID string) (*MetadataSummary, error) {
	const q = `
		SELECT
			count(*)                                                          AS total,
			count(*) FILTER (WHERE metadata_extracted_at IS NOT NULL)         AS extracted,
			count(*) FILTER (WHERE metadata_extracted_at IS NULL)             AS pending,
			count(*) FILTER (WHERE captured_at_source = 'exif')               AS with_exif_date,
			count(*) FILTER (WHERE captured_at_source = 'filesystem')         AS with_file_date,
			count(*) FILTER (WHERE metadata_extracted_at IS NOT NULL
			                   AND captured_at IS NULL)                       AS with_no_date,
			count(*) FILTER (WHERE latitude IS NOT NULL)                      AS with_gps,
			count(*) FILTER (WHERE state = 'failed')                          AS failed
		FROM photos
		WHERE library_id = $1`

	var m MetadataSummary
	err := s.pool.QueryRow(ctx, q, libraryID).Scan(
		&m.Total, &m.Extracted, &m.Pending,
		&m.WithEXIFDate, &m.WithFileDate, &m.WithNoDate,
		&m.WithGPS, &m.Failed,
	)
	if err != nil {
		return nil, fmt.Errorf("store: summarising metadata: %w", err)
	}
	return &m, nil
}
