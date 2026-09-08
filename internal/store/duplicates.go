package store

import (
	"context"
	"fmt"
	"time"
)

// PhotoNeedingHash is the minimal row the hashing pass needs.
type PhotoNeedingHash struct {
	ID            string
	RelativePath  string
	FileSizeBytes int64
}

// ListPhotosNeedingHash returns photos that have not been hashed yet.
//
// Backed by the partial index on (library_id, id) WHERE hashed_at IS NULL, so
// the query gets cheaper as the pass progresses.
func (s *Store) ListPhotosNeedingHash(ctx context.Context, libraryID string, limit int) ([]PhotoNeedingHash, error) {
	if limit <= 0 || limit > 5000 {
		limit = 500
	}

	const q = `
		SELECT id, relative_path, file_size_bytes
		FROM photos
		WHERE library_id = $1
		  AND hashed_at IS NULL
		  -- A file already known to be gone or undecodable will not hash
		  -- either. Skipping avoids re-reading files we know will fail.
		  AND state <> 'missing'
		ORDER BY id
		LIMIT $2`

	rows, err := s.pool.Query(ctx, q, libraryID, limit)
	if err != nil {
		return nil, fmt.Errorf("store: listing photos needing hash: %w", err)
	}
	defer rows.Close()

	out := []PhotoNeedingHash{}
	for rows.Next() {
		var p PhotoNeedingHash
		if err := rows.Scan(&p.ID, &p.RelativePath, &p.FileSizeBytes); err != nil {
			return nil, fmt.Errorf("store: scanning photo: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// SetPhotoHash records a computed digest.
//
// Idempotent: a keyed overwrite of a deterministic function of the file's
// bytes. hashed_at is set unconditionally so a failure is not retried forever.
func (s *Store) SetPhotoHash(ctx context.Context, photoID string, sum []byte, failure *string) error {
	const q = `
		UPDATE photos
		SET sha256     = $2,
		    hashed_at  = now(),
		    last_error = COALESCE($3, last_error)
		WHERE id = $1`

	if _, err := s.pool.Exec(ctx, q, photoID, sum, failure); err != nil {
		return fmt.Errorf("store: setting photo hash: %w", err)
	}
	return nil
}

// CountPhotosNeedingHash reports outstanding hashing work.
func (s *Store) CountPhotosNeedingHash(ctx context.Context, libraryID string) (int, error) {
	const q = `
		SELECT count(*) FROM photos
		WHERE library_id = $1 AND hashed_at IS NULL AND state <> 'missing'`

	var n int
	if err := s.pool.QueryRow(ctx, q, libraryID).Scan(&n); err != nil {
		return 0, fmt.Errorf("store: counting photos needing hash: %w", err)
	}
	return n, nil
}

// RebuildDuplicateGroups recomputes every exact-duplicate group for a library.
//
// This is the AGGREGATE stage described in ARCHITECTURE.md: it needs a global
// view of the library (a GROUP BY over every hash), so sharding it across
// workers would mean merging partial groups -- more coordination than the
// operation costs. In Phase 5 it becomes a single library-scoped job rather
// than N per-photo jobs.
//
// Idempotent by rebuild-in-place: the whole thing runs in one transaction that
// deletes the library's groups and re-derives them. Running it twice converges
// to the same state, which is what makes at-least-once job delivery safe. It is
// a full rebuild rather than an incremental patch because reconciling
// "this photo's hash changed, so remove it from group A, maybe delete group A
// if it now has one member, add it to group B..." is far more code and far more
// ways to be subtly wrong, for an operation that is a couple of indexed scans.
//
// Returns the number of groups and the total reclaimable bytes.
func (s *Store) RebuildDuplicateGroups(ctx context.Context, libraryID string) (groups int, reclaimable int64, err error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, 0, fmt.Errorf("store: begin rebuild: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	// Members go first: ON DELETE CASCADE would handle it, but being explicit
	// keeps the intent readable.
	if _, err := tx.Exec(ctx, `
		DELETE FROM duplicate_group_members
		WHERE group_id IN (SELECT id FROM duplicate_groups WHERE library_id = $1)`,
		libraryID); err != nil {
		return 0, 0, fmt.Errorf("store: clearing group members: %w", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM duplicate_groups WHERE library_id = $1`, libraryID); err != nil {
		return 0, 0, fmt.Errorf("store: clearing groups: %w", err)
	}

	// Build the groups.
	//
	// The suggested_keep heuristic deserves explaining. For EXACT duplicates
	// the files are byte-identical, so this is not a choice about image
	// quality -- every candidate is the same image. It is a choice about which
	// PATH to keep, and the goal is to pick the one that looks like the
	// original rather than a copy:
	//
	//   1. Fewest path separators  -- "photo.jpg" beats "backup/old/photo.jpg"
	//   2. Shortest filename       -- "photo.jpg" beats "photo (1).jpg" and
	//                                 "photo-copy.jpg"
	//   3. Alphabetical            -- a deterministic tiebreak, so repeated
	//                                 rebuilds produce identical suggestions
	//
	// Step 3 matters more than it looks: without a total ordering the
	// suggestion could flip between runs, and a user who saw "keep A" yesterday
	// would be told "keep B" today for unchanged data.
	const buildGroups = `
		WITH dupes AS (
			SELECT sha256,
			       count(*)          AS photo_count,
			       sum(file_size_bytes) AS total_bytes,
			       min(file_size_bytes) AS one_copy_bytes
			FROM photos
			WHERE library_id = $1
			  AND sha256 IS NOT NULL
			  AND state <> 'missing'
			GROUP BY sha256
			HAVING count(*) > 1
		),
		ranked AS (
			SELECT p.sha256,
			       p.id,
			       row_number() OVER (
			           PARTITION BY p.sha256
			           ORDER BY (length(p.relative_path)
			                     - length(replace(p.relative_path, '/', ''))) ASC,
			                    length(p.relative_path) ASC,
			                    p.relative_path ASC
			       ) AS rank
			FROM photos p
			JOIN dupes d ON d.sha256 = p.sha256
			WHERE p.library_id = $1 AND p.state <> 'missing'
		)
		INSERT INTO duplicate_groups (
			library_id, sha256, photo_count, total_bytes,
			reclaimable_bytes, suggested_keep_photo_id
		)
		SELECT $1,
		       d.sha256,
		       d.photo_count,
		       d.total_bytes,
		       d.total_bytes - d.one_copy_bytes,
		       r.id
		FROM dupes d
		JOIN ranked r ON r.sha256 = d.sha256 AND r.rank = 1
		RETURNING reclaimable_bytes`

	rows, err := tx.Query(ctx, buildGroups, libraryID)
	if err != nil {
		return 0, 0, fmt.Errorf("store: building duplicate groups: %w", err)
	}
	for rows.Next() {
		var r int64
		if err := rows.Scan(&r); err != nil {
			rows.Close()
			return 0, 0, fmt.Errorf("store: scanning group: %w", err)
		}
		groups++
		reclaimable += r
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, 0, fmt.Errorf("store: building duplicate groups: %w", err)
	}

	// Populate membership by joining photos back to the groups just created.
	if _, err := tx.Exec(ctx, `
		INSERT INTO duplicate_group_members (group_id, photo_id)
		SELECT g.id, p.id
		FROM duplicate_groups g
		JOIN photos p
		  ON p.library_id = g.library_id
		 AND p.sha256 = g.sha256
		 AND p.state <> 'missing'
		WHERE g.library_id = $1`, libraryID); err != nil {
		return 0, 0, fmt.Errorf("store: populating group members: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, 0, fmt.Errorf("store: commit rebuild: %w", err)
	}
	return groups, reclaimable, nil
}

// DuplicateGroup is one set of byte-identical photos.
type DuplicateGroup struct {
	ID               string    `json:"id"`
	SHA256           []byte    `json:"-"`
	SHA256Hex        string    `json:"sha256"`
	PhotoCount       int       `json:"photo_count"`
	TotalBytes       int64     `json:"total_bytes"`
	ReclaimableBytes int64     `json:"reclaimable_bytes"`
	SuggestedKeepID  *string   `json:"suggested_keep_photo_id,omitempty"`
	CreatedAt        time.Time `json:"created_at"`

	// Photos is populated by ListDuplicateGroups so the UI can render a group
	// without a follow-up request per group.
	Photos []DuplicateMember `json:"photos"`
}

// DuplicateMember is one photo within a duplicate group.
type DuplicateMember struct {
	PhotoID       string `json:"photo_id"`
	RelativePath  string `json:"relative_path"`
	FileSizeBytes int64  `json:"file_size_bytes"`
	// SuggestedKeep marks the one member this system recommends retaining.
	// Nothing is ever deleted -- this is a suggestion for the user to act on.
	SuggestedKeep bool `json:"suggested_keep"`
}

// ListDuplicateGroups returns groups worst-offender first, with members.
//
// Two queries rather than N+1: one for the groups on the page, one for all
// their members. Ordering by reclaimable_bytes DESC is backed by an index and
// puts the biggest wins at the top, which is what a user reviewing cleanup
// suggestions actually wants.
func (s *Store) ListDuplicateGroups(ctx context.Context, libraryID string, limit, offset int) ([]DuplicateGroup, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}

	const groupQ = `
		SELECT id, sha256, photo_count, total_bytes, reclaimable_bytes,
		       suggested_keep_photo_id, created_at
		FROM duplicate_groups
		WHERE library_id = $1
		ORDER BY reclaimable_bytes DESC, id
		LIMIT $2 OFFSET $3`

	rows, err := s.pool.Query(ctx, groupQ, libraryID, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("store: listing duplicate groups: %w", err)
	}

	out := []DuplicateGroup{}
	index := map[string]int{}
	ids := []string{}

	for rows.Next() {
		var g DuplicateGroup
		if err := rows.Scan(&g.ID, &g.SHA256, &g.PhotoCount, &g.TotalBytes,
			&g.ReclaimableBytes, &g.SuggestedKeepID, &g.CreatedAt); err != nil {
			rows.Close()
			return nil, fmt.Errorf("store: scanning duplicate group: %w", err)
		}
		g.SHA256Hex = fmt.Sprintf("%x", g.SHA256)
		g.Photos = []DuplicateMember{}
		index[g.ID] = len(out)
		ids = append(ids, g.ID)
		out = append(out, g)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: listing duplicate groups: %w", err)
	}
	if len(out) == 0 {
		return out, nil
	}

	const memberQ = `
		SELECT m.group_id, p.id, p.relative_path, p.file_size_bytes
		FROM duplicate_group_members m
		JOIN photos p ON p.id = m.photo_id
		WHERE m.group_id = ANY($1)
		ORDER BY p.relative_path`

	mrows, err := s.pool.Query(ctx, memberQ, ids)
	if err != nil {
		return nil, fmt.Errorf("store: listing group members: %w", err)
	}
	defer mrows.Close()

	for mrows.Next() {
		var groupID string
		var m DuplicateMember
		if err := mrows.Scan(&groupID, &m.PhotoID, &m.RelativePath, &m.FileSizeBytes); err != nil {
			return nil, fmt.Errorf("store: scanning group member: %w", err)
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

// DuplicateSummary is the headline for a library.
type DuplicateSummary struct {
	Groups           int   `json:"groups"`
	DuplicateFiles   int   `json:"duplicate_files"`
	ReclaimableBytes int64 `json:"reclaimable_bytes"`
	Hashed           int   `json:"hashed"`
	PendingHash      int   `json:"pending_hash"`
}

// SummariseDuplicates computes the whole summary in one pass.
func (s *Store) SummariseDuplicates(ctx context.Context, libraryID string) (*DuplicateSummary, error) {
	const q = `
		SELECT
			(SELECT count(*)                        FROM duplicate_groups WHERE library_id = $1),
			(SELECT COALESCE(sum(photo_count), 0)   FROM duplicate_groups WHERE library_id = $1),
			(SELECT COALESCE(sum(reclaimable_bytes), 0) FROM duplicate_groups WHERE library_id = $1),
			(SELECT count(*) FROM photos WHERE library_id = $1 AND sha256 IS NOT NULL),
			(SELECT count(*) FROM photos WHERE library_id = $1 AND hashed_at IS NULL AND state <> 'missing')`

	var d DuplicateSummary
	if err := s.pool.QueryRow(ctx, q, libraryID).Scan(
		&d.Groups, &d.DuplicateFiles, &d.ReclaimableBytes, &d.Hashed, &d.PendingHash,
	); err != nil {
		return nil, fmt.Errorf("store: summarising duplicates: %w", err)
	}
	return &d, nil
}
