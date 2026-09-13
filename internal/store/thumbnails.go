package store

import (
	"context"
	"fmt"
	"time"
)

// PhotoNeedingThumbnail is the minimal row the thumbnail pass needs.
type PhotoNeedingThumbnail struct {
	ID           string
	RelativePath string
}

// ListPhotoIDsNeedingThumbnail returns ids for job fan-out, after a cursor.
func (s *Store) ListPhotoIDsNeedingThumbnail(ctx context.Context, libraryID, afterID string, limit int) ([]string, error) {
	return s.listPhotoIDs(ctx, `
		SELECT id FROM photos
		WHERE library_id = $1
		  AND thumbnailed_at IS NULL
		  AND state NOT IN ('missing', 'failed')
		  AND ($3 = '' OR id > $3::uuid)
		ORDER BY id LIMIT $2`, libraryID, afterID, limit)
}

// SetPhotoThumbnail records a generated thumbnail.
//
// Idempotent: a keyed overwrite.
//
// Deliberately does NOT clear last_error. That column is shared by every pass,
// and a thumbnail succeeding says nothing about whether hashing or quality
// analysis failed on the same file -- clearing it here would erase another
// pass's diagnosis.
func (s *Store) SetPhotoThumbnail(ctx context.Context, photoID string, width, height int, bytes int64) error {
	const q = `
		UPDATE photos SET
			thumbnailed_at   = now(),
			thumbnail_width  = $2,
			thumbnail_height = $3,
			thumbnail_bytes  = $4
		WHERE id = $1`

	if _, err := s.pool.Exec(ctx, q, photoID, width, height, bytes); err != nil {
		return fmt.Errorf("store: setting photo thumbnail: %w", err)
	}
	return nil
}

// MarkThumbnailFailed records that no thumbnail could be made, without
// inventing dimensions.
func (s *Store) MarkThumbnailFailed(ctx context.Context, photoID, reason string) error {
	const q = `
		UPDATE photos SET
			thumbnailed_at   = now(),
			thumbnail_width  = NULL,
			thumbnail_height = NULL,
			thumbnail_bytes  = NULL,
			last_error       = $2
		WHERE id = $1`

	if _, err := s.pool.Exec(ctx, q, photoID, reason); err != nil {
		return fmt.Errorf("store: marking thumbnail failed: %w", err)
	}
	return nil
}

// GetThumbnailGeneratedAt reports when a photo's thumbnail was written.
//
// ErrNotFound covers every "nothing to serve" case at once: no such photo, a
// photo whose thumbnail has not been generated, and one whose generation
// failed. The serving endpoint treats all three the same way, so there is no
// reason to make it distinguish them.
func (s *Store) GetThumbnailGeneratedAt(ctx context.Context, photoID string) (time.Time, error) {
	const q = `
		SELECT thumbnailed_at FROM photos
		WHERE id = $1
		  AND thumbnailed_at IS NOT NULL
		  AND thumbnail_width IS NOT NULL`

	var t time.Time
	if err := s.pool.QueryRow(ctx, q, photoID).Scan(&t); err != nil {
		return time.Time{}, normaliseErr(err)
	}
	return t, nil
}
