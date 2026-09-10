package store

import (
	"context"
	"fmt"

	"github.com/srikarguntaka/photo-organizer/internal/duplicates"
)

// PhotoNeedingPHash is the minimal row the perceptual hashing pass needs.
type PhotoNeedingPHash struct {
	ID           string
	RelativePath string
}

// ListPhotosNeedingPHash returns photos that have not been perceptually hashed.
func (s *Store) ListPhotosNeedingPHash(ctx context.Context, libraryID string, limit int) ([]PhotoNeedingPHash, error) {
	if limit <= 0 || limit > 5000 {
		limit = 500
	}

	const q = `
		SELECT id, relative_path
		FROM photos
		WHERE library_id = $1
		  AND phashed_at IS NULL
		  -- Files already known to be gone or undecodable will not decode
		  -- either, and perceptual hashing needs pixels.
		  AND state NOT IN ('missing', 'failed')
		ORDER BY id
		LIMIT $2`

	rows, err := s.pool.Query(ctx, q, libraryID, limit)
	if err != nil {
		return nil, fmt.Errorf("store: listing photos needing phash: %w", err)
	}
	defer rows.Close()

	out := []PhotoNeedingPHash{}
	for rows.Next() {
		var p PhotoNeedingPHash
		if err := rows.Scan(&p.ID, &p.RelativePath); err != nil {
			return nil, fmt.Errorf("store: scanning photo: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ListPhotoIDsNeedingPHash returns ids for job fan-out, after a cursor.
func (s *Store) ListPhotoIDsNeedingPHash(ctx context.Context, libraryID, afterID string, limit int) ([]string, error) {
	return s.listPhotoIDs(ctx, `
		SELECT id FROM photos
		WHERE library_id = $1
		  AND phashed_at IS NULL
		  AND state NOT IN ('missing', 'failed')
		  AND ($3 = '' OR id > $3::uuid)
		ORDER BY id LIMIT $2`, libraryID, afterID, limit)
}

// SetPhotoPHash records a perceptual hash.
//
// The uint64 is stored as its two's-complement int64 because Postgres has no
// unsigned integer type. The bit pattern round-trips exactly, which is all
// Hamming distance needs.
func (s *Store) SetPhotoPHash(ctx context.Context, photoID string, hash uint64, failure *string) error {
	const q = `
		UPDATE photos
		SET phash      = $2,
		    phashed_at = now(),
		    last_error = COALESCE($3, last_error)
		WHERE id = $1`

	if _, err := s.pool.Exec(ctx, q, photoID, int64(hash), failure); err != nil {
		return fmt.Errorf("store: setting perceptual hash: %w", err)
	}
	return nil
}

// MarkPHashFailed records that perceptual hashing could not run, without
// storing a hash. A zero hash would make every failed photo a "near-duplicate"
// of every other failed photo.
func (s *Store) MarkPHashFailed(ctx context.Context, photoID string, reason string) error {
	const q = `
		UPDATE photos
		SET phash      = NULL,
		    phashed_at = now(),
		    last_error = $2
		WHERE id = $1`

	if _, err := s.pool.Exec(ctx, q, photoID, reason); err != nil {
		return fmt.Errorf("store: marking phash failed: %w", err)
	}
	return nil
}

// CountPhotosNeedingPHash reports outstanding perceptual hashing work.
func (s *Store) CountPhotosNeedingPHash(ctx context.Context, libraryID string) (int, error) {
	const q = `
		SELECT count(*) FROM photos
		WHERE library_id = $1 AND phashed_at IS NULL AND state NOT IN ('missing', 'failed')`

	var n int
	if err := s.pool.QueryRow(ctx, q, libraryID).Scan(&n); err != nil {
		return 0, fmt.Errorf("store: counting photos needing phash: %w", err)
	}
	return n, nil
}

// LoadSimilarityCandidates fetches every hashed photo in a library.
//
// The whole library at once, deliberately: grouping is a global operation over
// the similarity graph, and a paged version would find only the groups that
// happen to fall inside a page. At 100,000 photos this is roughly 6MB of
// candidate structs, which is a reasonable price for correctness.
func (s *Store) LoadSimilarityCandidates(ctx context.Context, libraryID string) ([]duplicates.Candidate, error) {
	const q = `
		SELECT id, phash, COALESCE(width, 0), COALESCE(height, 0), file_size_bytes,
		       relative_path, quality_score
		FROM photos
		WHERE library_id = $1
		  AND phash IS NOT NULL
		  AND state <> 'missing'
		ORDER BY id`

	rows, err := s.pool.Query(ctx, q, libraryID)
	if err != nil {
		return nil, fmt.Errorf("store: loading similarity candidates: %w", err)
	}
	defer rows.Close()

	out := []duplicates.Candidate{}
	for rows.Next() {
		var c duplicates.Candidate
		var signed int64
		if err := rows.Scan(&c.PhotoID, &signed, &c.Width, &c.Height, &c.FileSizeBytes,
			&c.RelativePath, &c.QualityScore); err != nil {
			return nil, fmt.Errorf("store: scanning candidate: %w", err)
		}
		c.Hash = uint64(signed)
		out = append(out, c)
	}
	return out, rows.Err()
}

// RebuildSimilarGroups recomputes near-duplicate groups for a library.
//
// An AGGREGATE stage, like duplicate grouping: it needs a global view, runs in
// one transaction, and is a full rebuild rather than an incremental patch so
// that re-running converges. That is what makes it safe under at-least-once
// job delivery.
//
// The grouping itself happens in Go rather than SQL. Hamming distance over a
// similarity graph with connected components is not something SQL expresses
// well, and internal/duplicates is a pure package that can be tested
// exhaustively without a database.
func (s *Store) RebuildSimilarGroups(ctx context.Context, libraryID string, threshold int) (groups int, reclaimable int64, err error) {
	candidates, err := s.LoadSimilarityCandidates(ctx, libraryID)
	if err != nil {
		return 0, 0, err
	}

	sizeByID := make(map[string]int64, len(candidates))
	for _, c := range candidates {
		sizeByID[c.PhotoID] = c.FileSizeBytes
	}

	found := duplicates.GroupSimilar(candidates, threshold)

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, 0, fmt.Errorf("store: begin similar rebuild: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	if _, err := tx.Exec(ctx, `
		DELETE FROM similar_group_members
		WHERE group_id IN (SELECT id FROM similar_groups WHERE library_id = $1)`,
		libraryID); err != nil {
		return 0, 0, fmt.Errorf("store: clearing similar members: %w", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM similar_groups WHERE library_id = $1`, libraryID); err != nil {
		return 0, 0, fmt.Errorf("store: clearing similar groups: %w", err)
	}

	for _, g := range found {
		// Reclaimable is everything except the copy being kept.
		var total int64
		for _, id := range g.PhotoIDs {
			total += sizeByID[id]
		}
		reclaim := total - sizeByID[g.SuggestedKeepID]
		if reclaim < 0 {
			reclaim = 0
		}

		var groupID string
		if err := tx.QueryRow(ctx, `
			INSERT INTO similar_groups (
				library_id, photo_count, threshold, max_distance,
				reclaimable_bytes, suggested_keep_photo_id
			) VALUES ($1, $2, $3, $4, $5, $6)
			RETURNING id`,
			libraryID, len(g.PhotoIDs), threshold, g.MaxDistance,
			reclaim, g.SuggestedKeepID,
		).Scan(&groupID); err != nil {
			return 0, 0, fmt.Errorf("store: inserting similar group: %w", err)
		}

		// PhotoIDs arrive already ranked best-first.
		for rank, photoID := range g.PhotoIDs {
			if _, err := tx.Exec(ctx, `
				INSERT INTO similar_group_members (group_id, photo_id, distance, rank)
				VALUES ($1, $2, $3, $4)`,
				groupID, photoID, g.Distances[photoID], rank+1,
			); err != nil {
				return 0, 0, fmt.Errorf("store: inserting similar member: %w", err)
			}
		}

		groups++
		reclaimable += reclaim
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, 0, fmt.Errorf("store: commit similar rebuild: %w", err)
	}
	return groups, reclaimable, nil
}

// SimilarGroup is a set of visually similar photos.
type SimilarGroup struct {
	ID               string  `json:"id"`
	PhotoCount       int     `json:"photo_count"`
	Threshold        int     `json:"threshold"`
	MaxDistance      int     `json:"max_distance"`
	ReclaimableBytes int64   `json:"reclaimable_bytes"`
	SuggestedKeepID  *string `json:"suggested_keep_photo_id,omitempty"`

	// Chained is true when the group is wider than the threshold it was built
	// with, meaning members were connected through intermediates rather than
	// all being mutually similar. Surfaced so the UI can mark such groups as
	// worth a closer look rather than presenting them with equal confidence.
	Chained bool `json:"chained"`

	Photos []SimilarMember `json:"photos"`
}

// SimilarMember is one photo in a similar group.
type SimilarMember struct {
	PhotoID       string `json:"photo_id"`
	RelativePath  string `json:"relative_path"`
	FileSizeBytes int64  `json:"file_size_bytes"`
	Width         *int   `json:"width,omitempty"`
	Height        *int   `json:"height,omitempty"`
	// Distance from the suggested-keep photo, in bits out of 64.
	Distance      int  `json:"distance"`
	Rank          int  `json:"rank"`
	SuggestedKeep bool `json:"suggested_keep"`
}

// ListSimilarGroups returns near-duplicate groups, largest first, with members.
func (s *Store) ListSimilarGroups(ctx context.Context, libraryID string, limit, offset int) ([]SimilarGroup, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}

	const groupQ = `
		SELECT id, photo_count, threshold, max_distance,
		       reclaimable_bytes, suggested_keep_photo_id
		FROM similar_groups
		WHERE library_id = $1
		ORDER BY photo_count DESC, reclaimable_bytes DESC, id
		LIMIT $2 OFFSET $3`

	rows, err := s.pool.Query(ctx, groupQ, libraryID, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("store: listing similar groups: %w", err)
	}

	out := []SimilarGroup{}
	index := map[string]int{}
	ids := []string{}

	for rows.Next() {
		var g SimilarGroup
		if err := rows.Scan(&g.ID, &g.PhotoCount, &g.Threshold, &g.MaxDistance,
			&g.ReclaimableBytes, &g.SuggestedKeepID); err != nil {
			rows.Close()
			return nil, fmt.Errorf("store: scanning similar group: %w", err)
		}
		g.Chained = g.MaxDistance > g.Threshold
		g.Photos = []SimilarMember{}
		index[g.ID] = len(out)
		ids = append(ids, g.ID)
		out = append(out, g)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: listing similar groups: %w", err)
	}
	if len(out) == 0 {
		return out, nil
	}

	const memberQ = `
		SELECT m.group_id, p.id, p.relative_path, p.file_size_bytes,
		       p.width, p.height, m.distance, m.rank
		FROM similar_group_members m
		JOIN photos p ON p.id = m.photo_id
		WHERE m.group_id = ANY($1)
		ORDER BY m.rank`

	mrows, err := s.pool.Query(ctx, memberQ, ids)
	if err != nil {
		return nil, fmt.Errorf("store: listing similar members: %w", err)
	}
	defer mrows.Close()

	for mrows.Next() {
		var groupID string
		var m SimilarMember
		if err := mrows.Scan(&groupID, &m.PhotoID, &m.RelativePath, &m.FileSizeBytes,
			&m.Width, &m.Height, &m.Distance, &m.Rank); err != nil {
			return nil, fmt.Errorf("store: scanning similar member: %w", err)
		}
		i, ok := index[groupID]
		if !ok {
			continue
		}
		if out[i].SuggestedKeepID != nil && *out[i].SuggestedKeepID == m.PhotoID {
			m.SuggestedKeep = true
		}
		out[i].Photos = append(out[i].Photos, m)
	}
	return out, mrows.Err()
}

// SimilarSummary is the headline for near-duplicates in a library.
type SimilarSummary struct {
	Groups           int   `json:"groups"`
	SimilarPhotos    int   `json:"similar_photos"`
	ReclaimableBytes int64 `json:"reclaimable_bytes"`
	ChainedGroups    int   `json:"chained_groups"`
	PHashed          int   `json:"phashed"`
	PendingPHash     int   `json:"pending_phash"`
}

// SummariseSimilar computes the near-duplicate summary in one pass.
func (s *Store) SummariseSimilar(ctx context.Context, libraryID string) (*SimilarSummary, error) {
	const q = `
		SELECT
			(SELECT count(*)                            FROM similar_groups WHERE library_id = $1),
			(SELECT COALESCE(sum(photo_count), 0)       FROM similar_groups WHERE library_id = $1),
			(SELECT COALESCE(sum(reclaimable_bytes), 0) FROM similar_groups WHERE library_id = $1),
			(SELECT count(*) FROM similar_groups WHERE library_id = $1 AND max_distance > threshold),
			(SELECT count(*) FROM photos WHERE library_id = $1 AND phash IS NOT NULL),
			(SELECT count(*) FROM photos WHERE library_id = $1 AND phashed_at IS NULL
			                               AND state NOT IN ('missing', 'failed'))`

	var out SimilarSummary
	if err := s.pool.QueryRow(ctx, q, libraryID).Scan(
		&out.Groups, &out.SimilarPhotos, &out.ReclaimableBytes,
		&out.ChainedGroups, &out.PHashed, &out.PendingPHash,
	); err != nil {
		return nil, fmt.Errorf("store: summarising similar groups: %w", err)
	}
	return &out, nil
}
