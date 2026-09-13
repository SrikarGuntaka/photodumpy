package store

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
)

// PhotoView is one photo as the read API presents it: everything the pipeline
// has learned about a file, assembled in one row.
//
// Deliberately separate from photos.Photo. The domain type is what the
// ingestion passes read and write, and it stays narrow on purpose; this is a
// projection for display, joining in quality scores and group memberships that
// no pass needs. Widening the domain type instead would mean every handler
// carrying fields it has no business touching.
//
// Almost every field is a pointer, and that is the honest shape: a photo that
// has not been analysed has no sharpness, and a camera that recorded no
// location produced no latitude. Zero would be a measurement.
type PhotoView struct {
	ID               string `json:"id"`
	LibraryID        string `json:"library_id"`
	RelativePath     string `json:"relative_path"`
	OriginalFilename string `json:"original_filename"`
	FileSizeBytes    int64  `json:"file_size_bytes"`
	DetectedFormat   string `json:"detected_format"`
	State            string `json:"state"`

	Width       *int    `json:"width,omitempty"`
	Height      *int    `json:"height,omitempty"`
	Orientation *int    `json:"orientation,omitempty"`
	CameraMake  *string `json:"camera_make,omitempty"`
	CameraModel *string `json:"camera_model,omitempty"`

	CapturedAt       *string  `json:"captured_at,omitempty"`
	CapturedAtSource *string  `json:"captured_at_source,omitempty"`
	Latitude         *float64 `json:"latitude,omitempty"`
	Longitude        *float64 `json:"longitude,omitempty"`

	Sharpness    *float64 `json:"sharpness,omitempty"`
	Exposure     *float64 `json:"exposure,omitempty"`
	Contrast     *float64 `json:"contrast,omitempty"`
	Resolution   *float64 `json:"resolution,omitempty"`
	QualityScore *float64 `json:"quality_score,omitempty"`
	QualityFlags []string `json:"quality_flags"`

	// SHA256 and PHash are rendered as hex rather than as the bytea and bigint
	// they are stored as. The perceptual hash in particular is held as a signed
	// int64 (Postgres has no unsigned type), so the raw column reads as a
	// meaningless negative number.
	SHA256 *string `json:"sha256,omitempty"`
	PHash  *string `json:"phash,omitempty"`

	// Group memberships, as ids. The detail endpoint expands these; the list
	// endpoint returns them so a grid can badge a photo as "duplicate" without
	// a second request per tile.
	DuplicateGroupID *string `json:"duplicate_group_id,omitempty"`
	SimilarGroupID   *string `json:"similar_group_id,omitempty"`
	ClusterID        *string `json:"cluster_id,omitempty"`

	// Thumbnail dimensions, oriented upright. Present only when a thumbnail
	// exists, so a UI can reserve each grid tile's space before the image
	// loads -- and render "no preview" rather than a broken image when absent.
	ThumbnailWidth  *int `json:"thumbnail_width,omitempty"`
	ThumbnailHeight *int `json:"thumbnail_height,omitempty"`

	LastError *string `json:"last_error,omitempty"`
}

// PhotoFilter is every way the read API can narrow a photo listing.
//
// A struct rather than a long parameter list because the same filter has to
// drive both the page query and the count query, and the two silently
// disagreeing is exactly the bug that makes a paginator show "page 7 of 5".
type PhotoFilter struct {
	State string

	// Flag matches photos carrying a given quality flag, e.g. possibly_blurry.
	Flag string

	// HasGPS, when set, selects photos that do or do not carry coordinates.
	// A tri-state pointer because "no filter" and "filter to no GPS" are
	// different requests and a bool cannot express both.
	HasGPS *bool

	// HasDuplicates, HasSimilar select photos that belong to a group.
	HasDuplicates *bool
	HasSimilar    *bool

	CapturedFrom *time.Time
	CapturedTo   *time.Time

	// PathContains is a case-insensitive substring match on the relative path.
	PathContains string
}

// PhotoSort names an ordering. The API accepts these strings and nothing else.
type PhotoSort string

const (
	SortPath     PhotoSort = "path"
	SortCaptured PhotoSort = "captured_at"
	SortSize     PhotoSort = "file_size"
	SortQuality  PhotoSort = "quality"
	SortCreated  PhotoSort = "created_at"
)

// sortColumns maps the API's sort names to SQL expressions.
//
// THIS MAP IS THE SECURITY BOUNDARY for ordering. A sort key is the one place
// a read API is tempted to interpolate a user-supplied string into SQL,
// because ORDER BY cannot take a bind parameter. The defence is a closed
// allow-list: an unrecognised key falls back to the default, and the
// user's string never reaches the query text.
//
// NULLS LAST on every nullable column. Postgres sorts NULLs first in DESC by
// default, so "worst quality first" would otherwise open with a page of
// unanalysed photos -- presenting "not measured" as "measured badly", which is
// the same mistake MarkQualityFailed exists to avoid.
var sortColumns = map[PhotoSort]string{
	SortPath:     "relative_path",
	SortCaptured: "captured_at NULLS LAST",
	SortSize:     "file_size_bytes",
	SortQuality:  "quality_score NULLS LAST",
	SortCreated:  "created_at",
}

// ValidSort reports whether a sort key is one this store understands.
//
// Exported so the HTTP layer can reject a typo with a 400 rather than let it
// fall through to the default ordering, where a caller would see a plausible
// page and no indication their sort was ignored.
func ValidSort(s PhotoSort) bool { _, ok := sortColumns[s]; return ok }

// ValidSorts lists the accepted sort keys, for error messages.
func ValidSorts() []string {
	out := make([]string, 0, len(sortColumns))
	for k := range sortColumns {
		out = append(out, string(k))
	}
	sort.Strings(out) // deterministic: map iteration order is not
	return out
}

// ListPhotosQuery is a filtered, sorted, paginated request.
type ListPhotosQuery struct {
	Filter     PhotoFilter
	Sort       PhotoSort
	Descending bool
	Limit      int
	Offset     int
}

// predicate builds the shared WHERE clause and its arguments.
//
// One builder feeds both the page query and the count query, so the two cannot
// drift apart. Every user value is a bind parameter; only the fixed operator
// text is assembled here.
func (f PhotoFilter) predicate(libraryID string) (string, []any) {
	args := []any{libraryID}
	clauses := []string{"library_id = $1"}

	add := func(sqlFmt string, v any) {
		args = append(args, v)
		clauses = append(clauses, fmt.Sprintf(sqlFmt, len(args)))
	}

	if f.State != "" {
		add("state = $%d::photo_state", f.State)
	}
	if f.Flag != "" {
		add("quality_flags @> ARRAY[$%d]", f.Flag)
	}
	if f.CapturedFrom != nil {
		add("captured_at >= $%d", *f.CapturedFrom)
	}
	if f.CapturedTo != nil {
		add("captured_at <= $%d", *f.CapturedTo)
	}
	if f.PathContains != "" {
		// ILIKE with the pattern built in SQL from a bound parameter, so the
		// user's text is never concatenated into the query. escapeLike
		// neutralises % and _ so a path containing them is searched for
		// literally instead of silently becoming a wildcard.
		add(`relative_path ILIKE '%%' || $%d || '%%' ESCAPE '\'`, escapeLike(f.PathContains))
	}

	// Presence filters. Written as IS NOT NULL / IS NULL rather than a bound
	// boolean because the negative case has to be a different operator, not a
	// different value.
	if f.HasGPS != nil {
		if *f.HasGPS {
			clauses = append(clauses, "latitude IS NOT NULL AND longitude IS NOT NULL")
		} else {
			clauses = append(clauses, "(latitude IS NULL OR longitude IS NULL)")
		}
	}
	if f.HasDuplicates != nil {
		clauses = append(clauses, membership("duplicate_group_members", *f.HasDuplicates))
	}
	if f.HasSimilar != nil {
		clauses = append(clauses, membership("similar_group_members", *f.HasSimilar))
	}

	return strings.Join(clauses, "\n  AND "), args
}

// membership renders an EXISTS test against a join table.
//
// The table name comes from this package's own call sites, never from a
// request -- it is a constant in the two places it is used.
func membership(table string, want bool) string {
	op := "EXISTS"
	if !want {
		op = "NOT EXISTS"
	}
	return fmt.Sprintf("%s (SELECT 1 FROM %s m WHERE m.photo_id = photos.id)", op, table)
}

// escapeLike neutralises LIKE metacharacters in a user-supplied substring.
//
// Without this, searching for "photo_1" matches "photo91", and a query of "%"
// matches everything -- not a security hole, since the value is still bound,
// but a search box that silently lies about what it matched. The backslash
// itself is escaped first, otherwise it would escape the escapes added after.
func escapeLike(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, "%", `\%`)
	s = strings.ReplaceAll(s, "_", `\_`)
	return s
}

// photoViewColumns is the projection behind PhotoView.
//
// The group memberships are scalar subqueries rather than LEFT JOINs. A join
// against three membership tables multiplies rows -- a photo in a duplicate
// group AND a similar group AND a cluster would come back three times, and
// LIMIT would then paginate over the multiplied rows rather than over photos.
// Each subquery is a single indexed lookup on photo_id.
const photoViewColumns = `
	photos.id, photos.library_id, photos.relative_path, photos.original_filename,
	photos.file_size_bytes, photos.detected_format, photos.state,
	photos.width, photos.height, photos.orientation,
	photos.camera_make, photos.camera_model,
	photos.captured_at, photos.captured_at_source,
	photos.latitude, photos.longitude,
	photos.sharpness_score, photos.exposure_score, photos.contrast_score,
	photos.resolution_score, photos.quality_score, photos.quality_flags,
	encode(photos.sha256, 'hex'),
	CASE WHEN photos.phash IS NULL THEN NULL
	     ELSE to_hex(photos.phash) END,
	(SELECT group_id::text FROM duplicate_group_members m WHERE m.photo_id = photos.id LIMIT 1),
	(SELECT group_id::text FROM similar_group_members    m WHERE m.photo_id = photos.id LIMIT 1),
	(SELECT cluster_id::text FROM cluster_members        m WHERE m.photo_id = photos.id LIMIT 1),
	photos.thumbnail_width, photos.thumbnail_height,
	photos.last_error`

func scanPhotoView(row interface{ Scan(...any) error }) (PhotoView, error) {
	var (
		v           PhotoView
		captured    *time.Time
		flags       []string
		phashHex    *string
		sha256Hex   *string
		capturedSrc *string
	)
	err := row.Scan(
		&v.ID, &v.LibraryID, &v.RelativePath, &v.OriginalFilename,
		&v.FileSizeBytes, &v.DetectedFormat, &v.State,
		&v.Width, &v.Height, &v.Orientation,
		&v.CameraMake, &v.CameraModel,
		&captured, &capturedSrc,
		&v.Latitude, &v.Longitude,
		&v.Sharpness, &v.Exposure, &v.Contrast,
		&v.Resolution, &v.QualityScore, &flags,
		&sha256Hex, &phashHex,
		&v.DuplicateGroupID, &v.SimilarGroupID, &v.ClusterID,
		&v.ThumbnailWidth, &v.ThumbnailHeight,
		&v.LastError,
	)
	if err != nil {
		return v, err
	}
	if captured != nil {
		s := captured.UTC().Format(time.RFC3339)
		v.CapturedAt = &s
	}
	v.CapturedAtSource = capturedSrc
	v.SHA256 = sha256Hex
	v.PHash = phashHex
	v.QualityFlags = flags
	if v.QualityFlags == nil {
		v.QualityFlags = []string{}
	}
	return v, nil
}

// ListPhotoViews returns one page of photos, and the total matching the same
// filter.
//
// The total is computed with the SAME predicate as the page, from one builder.
// A count that quietly ignores a filter is how a UI ends up offering page 7 of
// a 5-page result.
func (s *Store) ListPhotoViews(ctx context.Context, libraryID string, q ListPhotosQuery) ([]PhotoView, int, error) {
	if q.Limit <= 0 || q.Limit > 500 {
		q.Limit = 100
	}
	if q.Offset < 0 {
		q.Offset = 0
	}

	order, ok := sortColumns[q.Sort]
	if !ok {
		order = sortColumns[SortPath]
	}
	if q.Descending {
		// Inserted before NULLS LAST, which must stay at the end of the term.
		if rest, found := strings.CutSuffix(order, " NULLS LAST"); found {
			order = rest + " DESC NULLS LAST"
		} else {
			order += " DESC"
		}
	}

	// EVERY ordering ends with id. Without a unique tiebreak, rows that compare
	// equal on the sort key -- three copies of the same file, all 49.9 KB --
	// have no defined order between them, and Postgres is free to return them
	// differently on each query. Paging through such a result silently repeats
	// some rows and skips others.
	order += ", photos.id"

	where, args := q.Filter.predicate(libraryID)

	countSQL := `SELECT count(*) FROM photos WHERE ` + where
	var total int
	if err := s.pool.QueryRow(ctx, countSQL, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("store: counting photo views: %w", err)
	}

	pageSQL := fmt.Sprintf(`
		SELECT %s
		FROM photos
		WHERE %s
		ORDER BY %s
		LIMIT $%d OFFSET $%d`,
		photoViewColumns, where, order, len(args)+1, len(args)+2)

	rows, err := s.pool.Query(ctx, pageSQL, append(args, q.Limit, q.Offset)...)
	if err != nil {
		return nil, 0, fmt.Errorf("store: listing photo views: %w", err)
	}
	defer rows.Close()

	out := []PhotoView{}
	for rows.Next() {
		v, err := scanPhotoView(rows)
		if err != nil {
			return nil, 0, fmt.Errorf("store: scanning photo view: %w", err)
		}
		out = append(out, v)
	}
	return out, total, rows.Err()
}

// GetPhotoView returns one photo's full read model.
func (s *Store) GetPhotoView(ctx context.Context, photoID string) (*PhotoView, error) {
	q := `SELECT ` + photoViewColumns + ` FROM photos WHERE photos.id = $1`

	v, err := scanPhotoView(s.pool.QueryRow(ctx, q, photoID))
	if err != nil {
		return nil, normaliseErr(err)
	}
	return &v, nil
}

// RelatedPhoto is a sibling in one of a photo's groups.
type RelatedPhoto struct {
	PhotoID       string   `json:"photo_id"`
	RelativePath  string   `json:"relative_path"`
	FileSizeBytes int64    `json:"file_size_bytes"`
	Width         *int     `json:"width,omitempty"`
	Height        *int     `json:"height,omitempty"`
	QualityScore  *float64 `json:"quality_score,omitempty"`

	// Distance is the Hamming distance for a similar-group sibling, and metres
	// for a cluster sibling. Nil where neither applies -- exact duplicates are
	// identical, so there is no distance to report.
	Distance *float64 `json:"distance,omitempty"`

	// SuggestedKeep marks the copy this system recommends retaining. Always a
	// suggestion; nothing is ever deleted.
	SuggestedKeep bool `json:"suggested_keep"`

	// IsSelf marks the photo the detail request was about, so a UI can show it
	// in place among its siblings rather than filtering it out and losing the
	// ordering.
	IsSelf bool `json:"is_self"`
}

// PhotoRelations is everything a photo is grouped with.
type PhotoRelations struct {
	Duplicates []RelatedPhoto `json:"duplicates"`
	Similar    []RelatedPhoto `json:"similar"`
	Cluster    []RelatedPhoto `json:"cluster"`
}

// GetPhotoRelations loads the members of every group a photo belongs to.
//
// Each of the three queries self-joins through the membership table rather
// than taking a group id from the caller, so there is no window in which the
// caller's id is stale.
func (s *Store) GetPhotoRelations(ctx context.Context, photoID string) (*PhotoRelations, error) {
	out := &PhotoRelations{
		Duplicates: []RelatedPhoto{},
		Similar:    []RelatedPhoto{},
		Cluster:    []RelatedPhoto{},
	}

	// No distance and no rank: every member is byte-identical, so there is
	// nothing to measure between them and duplicate_group_members stores no
	// ordering. Keeper first, then path -- deterministic, and the same order
	// the duplicates listing uses.
	const duplicatesQ = `
		SELECT p.id, p.relative_path, p.file_size_bytes, p.width, p.height,
		       p.quality_score, NULL::double precision,
		       (g.suggested_keep_photo_id = p.id), (p.id = $1)
		FROM duplicate_group_members me
		JOIN duplicate_groups g        ON g.id = me.group_id
		JOIN duplicate_group_members m ON m.group_id = me.group_id
		JOIN photos p                  ON p.id = m.photo_id
		WHERE me.photo_id = $1
		ORDER BY (g.suggested_keep_photo_id = p.id) DESC, p.relative_path`

	const similarQ = `
		SELECT p.id, p.relative_path, p.file_size_bytes, p.width, p.height,
		       p.quality_score, m.distance::double precision,
		       (g.suggested_keep_photo_id = p.id), (p.id = $1)
		FROM similar_group_members me
		JOIN similar_groups g        ON g.id = me.group_id
		JOIN similar_group_members m ON m.group_id = me.group_id
		JOIN photos p                ON p.id = m.photo_id
		WHERE me.photo_id = $1
		ORDER BY m.rank`

	// Cluster siblings have no suggested keeper: an event is not a set of
	// alternatives, so there is nothing to choose between. The column is a
	// literal false rather than a join.
	const clusterQ = `
		SELECT p.id, p.relative_path, p.file_size_bytes, p.width, p.height,
		       p.quality_score, m.distance_meters,
		       false, (p.id = $1)
		FROM cluster_members me
		JOIN cluster_members m ON m.cluster_id = me.cluster_id
		JOIN photos p          ON p.id = m.photo_id
		WHERE me.photo_id = $1
		ORDER BY m.sequence`

	for _, spec := range []struct {
		sql  string
		dest *[]RelatedPhoto
	}{
		{duplicatesQ, &out.Duplicates},
		{similarQ, &out.Similar},
		{clusterQ, &out.Cluster},
	} {
		rows, err := s.pool.Query(ctx, spec.sql, photoID)
		if err != nil {
			return nil, fmt.Errorf("store: loading photo relations: %w", err)
		}
		for rows.Next() {
			var r RelatedPhoto
			if err := rows.Scan(&r.PhotoID, &r.RelativePath, &r.FileSizeBytes,
				&r.Width, &r.Height, &r.QualityScore, &r.Distance,
				&r.SuggestedKeep, &r.IsSelf); err != nil {
				rows.Close()
				return nil, fmt.Errorf("store: scanning related photo: %w", err)
			}
			*spec.dest = append(*spec.dest, r)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, fmt.Errorf("store: reading photo relations: %w", err)
		}
	}
	return out, nil
}
