package store

import (
	"context"
	"fmt"
	"time"

	"github.com/srikarguntaka/photo-organizer/internal/clustering"
)

// LoadClusteringPoints reads every photo in a library that could belong to an
// event.
//
// Photos with no capture time are included deliberately, with a zero
// CapturedAt. Segment reports them as unclustered rather than dropping them,
// so the counts reconcile: clustered + undated + missing/failed = the library.
// Filtering them out here would make photos silently disappear from the
// totals, which is how a "the numbers nearly add up" bug starts.
func (s *Store) LoadClusteringPoints(ctx context.Context, libraryID string) ([]clustering.Point, error) {
	const q = `
		SELECT id, captured_at, captured_at_source, latitude, longitude
		FROM photos
		WHERE library_id = $1
		  AND state NOT IN ('missing', 'failed')
		ORDER BY id`

	rows, err := s.pool.Query(ctx, q, libraryID)
	if err != nil {
		return nil, fmt.Errorf("store: loading clustering points: %w", err)
	}
	defer rows.Close()

	out := []clustering.Point{}
	for rows.Next() {
		var (
			p        clustering.Point
			captured *time.Time
			source   *string
			lat, lon *float64
		)
		if err := rows.Scan(&p.PhotoID, &captured, &source, &lat, &lon); err != nil {
			return nil, fmt.Errorf("store: scanning clustering point: %w", err)
		}
		if captured != nil {
			p.CapturedAt = *captured
		}
		if source != nil {
			p.DateSource = clustering.DateSource(*source)
		}
		// Latitude and longitude are only a location together. A row with one
		// of the two is malformed, and treating it as a fix would place the
		// photo on the equator or the prime meridian.
		if lat != nil && lon != nil {
			p.Latitude, p.Longitude = lat, lon
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// RebuildClusters recomputes every event in a library from scratch.
//
// Idempotent by replacement, like the other aggregate stages: clustering is a
// deterministic function of the photos and the thresholds, so the honest way
// to make it safe to re-run is to delete and rewrite rather than to attempt an
// incremental merge. A run that adds no photos produces byte-identical rows.
//
// Whole-library rather than incremental because a single new photo can
// legitimately MERGE two existing events -- one taken in the gap between them
// joins both into one. An incremental path would need to handle merges,
// splits and re-anchoring, and at this scale the full rebuild is a few
// milliseconds.
func (s *Store) RebuildClusters(ctx context.Context, libraryID string, opts clustering.Options) (clusters int, undated int, err error) {
	points, err := s.LoadClusteringPoints(ctx, libraryID)
	if err != nil {
		return 0, 0, err
	}

	// Resolve the thresholds BEFORE clustering, so the values recorded on each
	// row are the ones actually applied. Segment would default them internally
	// anyway; taking the resolved copy here is what keeps the stored
	// max_gap_seconds from being a caller's unset zero.
	opts = opts.WithDefaults()
	res := clustering.Segment(points, opts)

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, 0, fmt.Errorf("store: begin cluster rebuild: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	// Members first: the foreign key points this way, and clearing them
	// separately keeps the cascade from doing work implicitly.
	if _, err := tx.Exec(ctx, `
		DELETE FROM cluster_members
		WHERE cluster_id IN (SELECT id FROM clusters WHERE library_id = $1)`,
		libraryID); err != nil {
		return 0, 0, fmt.Errorf("store: clearing cluster members: %w", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM clusters WHERE library_id = $1`, libraryID); err != nil {
		return 0, 0, fmt.Errorf("store: clearing clusters: %w", err)
	}

	gapSeconds := int(opts.MaxGap.Seconds())
	for _, c := range res.Clusters {
		var clusterID string
		if err := tx.QueryRow(ctx, `
			INSERT INTO clusters (
				library_id, photo_count, started_at, ended_at,
				max_gap_seconds, max_radius_meters,
				anchor_latitude, anchor_longitude, max_distance_meters,
				located_count, filesystem_dated_count
			) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
			RETURNING id`,
			libraryID, len(c.PhotoIDs), c.StartedAt, c.EndedAt,
			gapSeconds, opts.MaxRadiusMeters,
			c.AnchorLat, c.AnchorLon, c.MaxDistanceMeters,
			c.LocatedCount, c.FilesystemDatedCount,
		).Scan(&clusterID); err != nil {
			return 0, 0, fmt.Errorf("store: inserting cluster: %w", err)
		}

		for i, photoID := range c.PhotoIDs {
			// Distance comes from the clustering pass, which is the only
			// implementation of Haversine in the codebase. Recomputing it in
			// SQL would be a second implementation free to disagree with the
			// first -- and it would have to be the asin form, which loses
			// precision at exactly the short ranges this feature works at.
			//
			// Absent from the map means "no GPS fix", which is stored as NULL
			// rather than as 0.
			var distance *float64
			if d, ok := c.DistanceMeters[photoID]; ok {
				distance = &d
			}

			if _, err := tx.Exec(ctx, `
				INSERT INTO cluster_members (cluster_id, photo_id, sequence, distance_meters)
				VALUES ($1, $2, $3, $4)`,
				clusterID, photoID, i+1, distance,
			); err != nil {
				return 0, 0, fmt.Errorf("store: inserting cluster member: %w", err)
			}
		}
		clusters++
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, 0, fmt.Errorf("store: commit cluster rebuild: %w", err)
	}
	return clusters, len(res.UndatedPhotoIDs), nil
}

// ClusterMember is one photo on an event's timeline.
type ClusterMember struct {
	PhotoID        string   `json:"photo_id"`
	RelativePath   string   `json:"relative_path"`
	Sequence       int      `json:"sequence"`
	CapturedAt     *string  `json:"captured_at,omitempty"`
	DistanceMeters *float64 `json:"distance_meters,omitempty"`
	Latitude       *float64 `json:"latitude,omitempty"`
	Longitude      *float64 `json:"longitude,omitempty"`
}

// Cluster is one event as the API reports it.
type Cluster struct {
	ID         string `json:"id"`
	PhotoCount int    `json:"photo_count"`
	StartedAt  string `json:"started_at"`
	EndedAt    string `json:"ended_at"`

	AnchorLatitude    *float64 `json:"anchor_latitude,omitempty"`
	AnchorLongitude   *float64 `json:"anchor_longitude,omitempty"`
	MaxDistanceMeters float64  `json:"max_distance_meters"`

	LocatedCount         int `json:"located_count"`
	FilesystemDatedCount int `json:"filesystem_dated_count"`

	// Confidence is "high", "mixed" or "low", derived from how many members
	// were dated by a camera rather than by a filesystem mtime.
	Confidence string `json:"confidence"`

	Photos []ClusterMember `json:"photos"`
}

// ListClusters returns a library's events, newest first.
func (s *Store) ListClusters(ctx context.Context, libraryID string, limit, offset int) ([]Cluster, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	if offset < 0 {
		offset = 0
	}

	const q = `
		SELECT id, photo_count, started_at, ended_at,
		       anchor_latitude, anchor_longitude, max_distance_meters,
		       located_count, filesystem_dated_count
		FROM clusters
		WHERE library_id = $1
		ORDER BY started_at DESC
		LIMIT $2 OFFSET $3`

	rows, err := s.pool.Query(ctx, q, libraryID, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("store: listing clusters: %w", err)
	}
	defer rows.Close()

	out := []Cluster{}
	ids := []string{}
	for rows.Next() {
		var (
			c                 Cluster
			started, ended    time.Time
			fsDated, photoCnt int
		)
		if err := rows.Scan(&c.ID, &photoCnt, &started, &ended,
			&c.AnchorLatitude, &c.AnchorLongitude, &c.MaxDistanceMeters,
			&c.LocatedCount, &fsDated); err != nil {
			return nil, fmt.Errorf("store: scanning cluster: %w", err)
		}
		c.PhotoCount = photoCnt
		c.FilesystemDatedCount = fsDated
		c.StartedAt = started.UTC().Format(time.RFC3339)
		c.EndedAt = ended.UTC().Format(time.RFC3339)
		c.Confidence = clustering.Cluster{
			PhotoIDs:             make([]string, photoCnt),
			FilesystemDatedCount: fsDated,
		}.Confidence()
		c.Photos = []ClusterMember{}
		out = append(out, c)
		ids = append(ids, c.ID)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return out, nil
	}

	// Members for every listed cluster in one query rather than one per
	// cluster. A hundred events would otherwise be a hundred round trips.
	const memberQ = `
		SELECT m.cluster_id, m.photo_id, p.relative_path, m.sequence,
		       p.captured_at, m.distance_meters, p.latitude, p.longitude
		FROM cluster_members m
		JOIN photos p ON p.id = m.photo_id
		WHERE m.cluster_id = ANY($1)
		ORDER BY m.cluster_id, m.sequence`

	mrows, err := s.pool.Query(ctx, memberQ, ids)
	if err != nil {
		return nil, fmt.Errorf("store: listing cluster members: %w", err)
	}
	defer mrows.Close()

	byID := make(map[string]*Cluster, len(out))
	for i := range out {
		byID[out[i].ID] = &out[i]
	}
	for mrows.Next() {
		var (
			clusterID string
			m         ClusterMember
			captured  *time.Time
		)
		if err := mrows.Scan(&clusterID, &m.PhotoID, &m.RelativePath, &m.Sequence,
			&captured, &m.DistanceMeters, &m.Latitude, &m.Longitude); err != nil {
			return nil, fmt.Errorf("store: scanning cluster member: %w", err)
		}
		if captured != nil {
			s := captured.UTC().Format(time.RFC3339)
			m.CapturedAt = &s
		}
		if c, ok := byID[clusterID]; ok {
			c.Photos = append(c.Photos, m)
		}
	}
	return out, mrows.Err()
}

// ClusterSummary is the headline for a library's timeline.
type ClusterSummary struct {
	Clusters int `json:"clusters"`
	// Clustered is how many photos landed in an event.
	Clustered int `json:"clustered_photos"`
	// Undated is how many could not be placed because they have no timestamp.
	// Reported rather than hidden: these are not a failure, they are a fact
	// about the files.
	Undated int `json:"undated_photos"`
	// Located is how many clustered photos carried a GPS fix.
	Located int `json:"located_photos"`
	// LowConfidence counts events built entirely from filesystem mtimes.
	LowConfidence int `json:"low_confidence_clusters"`
	// LargestPhotoCount is the biggest event's size.
	LargestPhotoCount int `json:"largest_cluster_photos"`
}

// SummariseClusters computes the timeline headline.
func (s *Store) SummariseClusters(ctx context.Context, libraryID string) (*ClusterSummary, error) {
	const q = `
		SELECT
			count(*),
			coalesce(sum(photo_count), 0),
			coalesce(sum(located_count), 0),
			count(*) FILTER (WHERE filesystem_dated_count = photo_count),
			coalesce(max(photo_count), 0)
		FROM clusters WHERE library_id = $1`

	out := &ClusterSummary{}
	if err := s.pool.QueryRow(ctx, q, libraryID).Scan(
		&out.Clusters, &out.Clustered, &out.Located,
		&out.LowConfidence, &out.LargestPhotoCount,
	); err != nil {
		return nil, fmt.Errorf("store: summarising clusters: %w", err)
	}

	// Undated is counted from photos rather than inferred by subtraction, so
	// the two numbers are independent measurements and a mismatch between them
	// is visible instead of arithmetically impossible.
	const undatedQ = `
		SELECT count(*) FROM photos
		WHERE library_id = $1 AND captured_at IS NULL
		  AND state NOT IN ('missing', 'failed')`
	if err := s.pool.QueryRow(ctx, undatedQ, libraryID).Scan(&out.Undated); err != nil {
		return nil, fmt.Errorf("store: counting undated photos: %w", err)
	}
	return out, nil
}
