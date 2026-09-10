package store

import (
	"context"
	"fmt"

	"github.com/srikarguntaka/photo-organizer/internal/quality"
)

// PhotoNeedingQuality is the minimal row the quality pass needs.
type PhotoNeedingQuality struct {
	ID           string
	RelativePath string
}

// ListPhotosNeedingQuality returns photos that have not been analysed.
func (s *Store) ListPhotosNeedingQuality(ctx context.Context, libraryID string, limit int) ([]PhotoNeedingQuality, error) {
	if limit <= 0 || limit > 5000 {
		limit = 500
	}

	const q = `
		SELECT id, relative_path
		FROM photos
		WHERE library_id = $1
		  AND quality_analyzed_at IS NULL
		  -- Quality analysis needs pixels, so files already known to be gone or
		  -- undecodable will not yield anything.
		  AND state NOT IN ('missing', 'failed')
		ORDER BY id
		LIMIT $2`

	rows, err := s.pool.Query(ctx, q, libraryID, limit)
	if err != nil {
		return nil, fmt.Errorf("store: listing photos needing quality: %w", err)
	}
	defer rows.Close()

	out := []PhotoNeedingQuality{}
	for rows.Next() {
		var p PhotoNeedingQuality
		if err := rows.Scan(&p.ID, &p.RelativePath); err != nil {
			return nil, fmt.Errorf("store: scanning photo: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ListPhotoIDsNeedingQuality returns ids for job fan-out, after a cursor.
func (s *Store) ListPhotoIDsNeedingQuality(ctx context.Context, libraryID, afterID string, limit int) ([]string, error) {
	return s.listPhotoIDs(ctx, `
		SELECT id FROM photos
		WHERE library_id = $1
		  AND quality_analyzed_at IS NULL
		  AND state NOT IN ('missing', 'failed')
		  AND ($3 = '' OR id > $3::uuid)
		ORDER BY id LIMIT $2`, libraryID, afterID, limit)
}

// SetPhotoQuality records analysis results.
//
// Idempotent: a deterministic function of the decoded pixels, written as a
// keyed overwrite.
func (s *Store) SetPhotoQuality(ctx context.Context, photoID string, m quality.Metrics) error {
	const q = `
		UPDATE photos SET
			sharpness_score        = $2,
			exposure_score         = $3,
			contrast_score         = $4,
			resolution_score       = $5,
			quality_score          = $6,
			raw_laplacian_variance = $7,
			mean_luminance         = $8,
			shadow_clipping        = $9,
			highlight_clipping     = $10,
			quality_flags          = $11,
			quality_analyzed_at    = now()
		WHERE id = $1`

	_, err := s.pool.Exec(ctx, q, photoID,
		m.Sharpness, m.Exposure, m.Contrast, m.Resolution, m.Overall,
		m.RawLaplacianVariance, m.MeanLuminance, m.ShadowClipping, m.HighlightClipping,
		m.Flags,
	)
	if err != nil {
		return fmt.Errorf("store: setting photo quality: %w", err)
	}
	return nil
}

// MarkQualityFailed records that analysis could not run, without inventing
// scores. A photo with a fabricated 0.0 would look like the worst photo in the
// library rather than an unmeasured one.
func (s *Store) MarkQualityFailed(ctx context.Context, photoID, reason string) error {
	const q = `
		UPDATE photos SET
			quality_analyzed_at = now(),
			last_error          = $2
		WHERE id = $1`

	if _, err := s.pool.Exec(ctx, q, photoID, reason); err != nil {
		return fmt.Errorf("store: marking quality failed: %w", err)
	}
	return nil
}

// CountPhotosNeedingQuality reports outstanding analysis work.
func (s *Store) CountPhotosNeedingQuality(ctx context.Context, libraryID string) (int, error) {
	const q = `
		SELECT count(*) FROM photos
		WHERE library_id = $1 AND quality_analyzed_at IS NULL AND state NOT IN ('missing', 'failed')`

	var n int
	if err := s.pool.QueryRow(ctx, q, libraryID).Scan(&n); err != nil {
		return 0, fmt.Errorf("store: counting photos needing quality: %w", err)
	}
	return n, nil
}

// QualityCandidate is a photo with quality scores, for review listings.
type QualityCandidate struct {
	PhotoID       string   `json:"photo_id"`
	RelativePath  string   `json:"relative_path"`
	FileSizeBytes int64    `json:"file_size_bytes"`
	Width         *int     `json:"width,omitempty"`
	Height        *int     `json:"height,omitempty"`
	Sharpness     *float64 `json:"sharpness,omitempty"`
	Exposure      *float64 `json:"exposure,omitempty"`
	Contrast      *float64 `json:"contrast,omitempty"`
	Quality       *float64 `json:"quality_score,omitempty"`
	MeanLuminance *float64 `json:"mean_luminance,omitempty"`
	Flags         []string `json:"flags"`
}

// ListFlaggedPhotos returns photos carrying at least one quality flag, worst
// first.
//
// Ordered by quality_score ascending so the most questionable photos surface
// at the top of a review screen. Optionally filtered to a single flag.
func (s *Store) ListFlaggedPhotos(ctx context.Context, libraryID, flag string, limit, offset int) ([]QualityCandidate, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	if offset < 0 {
		offset = 0
	}

	const q = `
		SELECT id, relative_path, file_size_bytes, width, height,
		       sharpness_score, exposure_score, contrast_score, quality_score,
		       mean_luminance, quality_flags
		FROM photos
		WHERE library_id = $1
		  AND quality_flags <> '{}'
		  AND ($4 = '' OR quality_flags @> ARRAY[$4])
		ORDER BY quality_score NULLS LAST, relative_path
		LIMIT $2 OFFSET $3`

	rows, err := s.pool.Query(ctx, q, libraryID, limit, offset, flag)
	if err != nil {
		return nil, fmt.Errorf("store: listing flagged photos: %w", err)
	}
	defer rows.Close()

	out := []QualityCandidate{}
	for rows.Next() {
		var c QualityCandidate
		if err := rows.Scan(&c.PhotoID, &c.RelativePath, &c.FileSizeBytes,
			&c.Width, &c.Height, &c.Sharpness, &c.Exposure, &c.Contrast,
			&c.Quality, &c.MeanLuminance, &c.Flags); err != nil {
			return nil, fmt.Errorf("store: scanning flagged photo: %w", err)
		}
		if c.Flags == nil {
			c.Flags = []string{}
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// QualitySummary is the headline for a library.
type QualitySummary struct {
	Analyzed    int            `json:"analyzed"`
	Pending     int            `json:"pending"`
	Flagged     int            `json:"flagged"`
	ByFlag      map[string]int `json:"by_flag"`
	MeanQuality *float64       `json:"mean_quality_score,omitempty"`
}

// SummariseQuality computes the quality summary.
func (s *Store) SummariseQuality(ctx context.Context, libraryID string) (*QualitySummary, error) {
	const headline = `
		SELECT
			count(*) FILTER (WHERE quality_analyzed_at IS NOT NULL),
			count(*) FILTER (WHERE quality_analyzed_at IS NULL
			                   AND state NOT IN ('missing', 'failed')),
			count(*) FILTER (WHERE quality_flags <> '{}'),
			avg(quality_score) FILTER (WHERE quality_score IS NOT NULL)
		FROM photos WHERE library_id = $1`

	out := &QualitySummary{ByFlag: map[string]int{}}
	if err := s.pool.QueryRow(ctx, headline, libraryID).Scan(
		&out.Analyzed, &out.Pending, &out.Flagged, &out.MeanQuality,
	); err != nil {
		return nil, fmt.Errorf("store: summarising quality: %w", err)
	}

	// Per-flag counts. unnest turns the array column into rows so each flag can
	// be counted independently -- a photo with three flags contributes to all
	// three, which is what a review screen needs.
	const byFlag = `
		SELECT flag, count(*)
		FROM photos, unnest(quality_flags) AS flag
		WHERE library_id = $1
		GROUP BY flag
		ORDER BY 2 DESC`

	rows, err := s.pool.Query(ctx, byFlag, libraryID)
	if err != nil {
		return nil, fmt.Errorf("store: counting quality flags: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var flag string
		var n int
		if err := rows.Scan(&flag, &n); err != nil {
			return nil, fmt.Errorf("store: scanning flag count: %w", err)
		}
		out.ByFlag[flag] = n
	}
	return out, rows.Err()
}
