// Package clustering groups photos into events: runs of pictures taken around
// the same time and, where the camera recorded it, in the same place.
//
// This is the step that turns a photo dump into something a person recognises.
// "Three hundred files from March" is not a memory; "the Saturday at the coast"
// is.
//
// Pure: it takes points and returns clusters. No database, no filesystem, no
// clock. Every threshold is a parameter, so the behaviour that matters --
// where a run of photos is cut in two -- is testable with exact assertions
// rather than by staring at a real library and deciding the output looks
// plausible.
//
// WHAT THIS PACKAGE ASSUMES ABOUT ITS INPUT, which is very little:
//
//   - Not every photo has GPS. Most do not, in practice. A photo without a
//     location must never break a cluster, and must never be excluded from
//     one.
//   - Not every photo has a trustworthy timestamp. Some have none at all and
//     cannot be clustered; others have only a filesystem mtime, which is a
//     fact about the file rather than about the photograph.
//   - Timestamps carry no zone. EXIF DateTimeOriginal is local wall-clock with
//     no offset recorded, so it is read as UTC upstream and compared here as
//     an instant. Gaps are therefore correct; absolute times may be shifted
//     for photos taken abroad, which is a property of the format, not a bug
//     here.
package clustering

import (
	"math"
	"sort"
	"time"
)

// DateSource records where a photo's timestamp came from. It changes how much
// the timestamp can be trusted, so it travels with it.
type DateSource string

const (
	// SourceEXIF is a camera-recorded capture time. Trustworthy.
	SourceEXIF DateSource = "exif"
	// SourceFilesystem is the file's modification time, used when EXIF has no
	// date. A fact about the FILE, not the photograph -- see Cluster.Confidence.
	SourceFilesystem DateSource = "filesystem"
)

// Point is one photo entering the clustering pass.
type Point struct {
	PhotoID string

	// CapturedAt is when the photo was taken. Points without one are not
	// clusterable and must be filtered out before Segment is called; Segment
	// reports them rather than guessing.
	CapturedAt time.Time
	DateSource DateSource

	// Latitude and Longitude are nil together when the photo has no GPS fix,
	// which is the common case. A point without coordinates still joins the
	// cluster its timestamp puts it in -- it simply does not participate in
	// the distance test.
	Latitude  *float64
	Longitude *float64
}

// HasLocation reports whether this point carries a usable GPS fix.
func (p Point) HasLocation() bool { return p.Latitude != nil && p.Longitude != nil }

// Options are the thresholds that decide where one event ends and the next
// begins. Both are judgement calls, which is why they are parameters.
type Options struct {
	// MaxGap is the silence between consecutive photos that ends an event.
	//
	// Four hours by default: long enough to survive lunch, a nap, or the drive
	// between two stops on the same day out; short enough that this morning
	// and tonight are separate events.
	MaxGap time.Duration

	// MaxRadiusMeters is how far a photo may sit from its cluster's anchor
	// before it starts a new cluster.
	//
	// 1500m by default -- roughly a fifteen-minute walk. Large enough that
	// wandering around one neighbourhood, museum or beach stays one event,
	// and that consumer GPS error (tens of metres, occasionally hundreds
	// indoors) never splits anything. Small enough to separate two stops on
	// the same trip.
	MaxRadiusMeters float64
}

// DefaultOptions are the calibrated defaults. See DESIGN_DECISIONS.md.
func DefaultOptions() Options {
	return Options{
		MaxGap:          4 * time.Hour,
		MaxRadiusMeters: 1500,
	}
}

// WithDefaults fills in any unset field with its calibrated default and
// returns the options that will ACTUALLY be used.
//
// Exported because callers that persist "the thresholds this result was built
// with" need the effective values, not the requested ones. Storing a
// caller-supplied zero would record that a cluster was built with a gap of
// zero seconds, which is both false and uninterpretable later -- the entire
// reason the thresholds are stored alongside the clusters.
func (o Options) WithDefaults() Options {
	d := DefaultOptions()
	if o.MaxGap <= 0 {
		o.MaxGap = d.MaxGap
	}
	if o.MaxRadiusMeters <= 0 {
		o.MaxRadiusMeters = d.MaxRadiusMeters
	}
	return o
}

// Cluster is one event.
type Cluster struct {
	// PhotoIDs in capture order, oldest first.
	PhotoIDs []string

	StartedAt time.Time
	EndedAt   time.Time

	// AnchorLat and AnchorLon are the cluster's first GPS fix, and the point
	// every later distance is measured from. Nil when no member had a
	// location, which is the common case for scanned or stripped photos.
	AnchorLat *float64
	AnchorLon *float64

	// MaxDistanceMeters is the furthest any member sits from the anchor. Zero
	// when fewer than two members carry a location.
	//
	// Surfaced for the same reason similar_groups surfaces max_distance: it
	// lets a reviewer see how spread out an event actually is instead of
	// taking the grouping on trust.
	MaxDistanceMeters float64

	// LocatedCount is how many members carried a GPS fix. The rest joined on
	// time alone, which is normal and worth being able to see.
	LocatedCount int

	// DistanceMeters holds each located member's distance from the anchor, so
	// the UI can show why a photo is in an event rather than asserting it.
	//
	// A member is ABSENT from this map when it has no GPS fix -- which is not
	// the same as being present with a distance of 0. "No fix" and "at the
	// anchor" are different facts, and conflating them would put every
	// unlocated photo at the centre of the map.
	DistanceMeters map[string]float64

	// FilesystemDatedCount is how many members were placed using an mtime
	// rather than a camera timestamp. See Confidence.
	FilesystemDatedCount int
}

// Confidence reports how much the cluster's boundaries can be trusted.
//
// A cluster built entirely from filesystem mtimes is a statement about when
// files were WRITTEN, not when photographs were taken. Copying a folder gives
// every file the same mtime within a second or two, which would otherwise
// collapse an entire library into one enormous "event" and present that
// confidently. Naming the weakness is better than hiding it.
func (c Cluster) Confidence() string {
	switch {
	case c.FilesystemDatedCount == 0:
		return "high"
	case c.FilesystemDatedCount == len(c.PhotoIDs):
		return "low"
	default:
		return "mixed"
	}
}

// Result is everything Segment worked out, including what it could not place.
type Result struct {
	Clusters []Cluster

	// UndatedPhotoIDs are photos with no timestamp at all. They are NOT
	// clustered and NOT silently dropped: with no time and no place there is
	// nothing to group them by, and inventing a cluster for them would be
	// presenting a guess as a finding.
	UndatedPhotoIDs []string
}

// Segment groups points into events.
//
// The algorithm is a single ordered pass, cutting between consecutive photos
// when either test fails. That is deliberate rather than a simplification:
// events are intrinsically sequential in time, and a general-purpose
// clusterer (k-means, DBSCAN over a time/space metric) would need a fabricated
// exchange rate between "one hour" and "one kilometre" to compute a distance
// at all. There is no honest such number, so the two dimensions are kept
// separate and each gets its own threshold with its own units.
//
// Complexity is O(n log n), dominated by the sort.
func Segment(points []Point, opts Options) Result {
	opts = opts.WithDefaults()
	res := Result{Clusters: []Cluster{}, UndatedPhotoIDs: []string{}}

	dated := make([]Point, 0, len(points))
	for _, p := range points {
		if p.CapturedAt.IsZero() {
			res.UndatedPhotoIDs = append(res.UndatedPhotoIDs, p.PhotoID)
			continue
		}
		dated = append(dated, p)
	}
	if len(dated) == 0 {
		return res
	}

	// Ties broken by photo id so the output is deterministic. Two photos from
	// a burst can share a timestamp to the second, and without a tiebreak the
	// cluster contents would depend on the order rows came back from the
	// database.
	sort.Slice(dated, func(i, j int) bool {
		if dated[i].CapturedAt.Equal(dated[j].CapturedAt) {
			return dated[i].PhotoID < dated[j].PhotoID
		}
		return dated[i].CapturedAt.Before(dated[j].CapturedAt)
	})

	cur := newCluster(dated[0])
	for i := 1; i < len(dated); i++ {
		p := dated[i]
		if breaksCluster(cur, dated[i-1], p, opts) {
			res.Clusters = append(res.Clusters, cur)
			cur = newCluster(p)
			continue
		}
		addTo(&cur, p)
	}
	res.Clusters = append(res.Clusters, cur)
	return res
}

// breaksCluster decides whether p starts a new event.
func breaksCluster(cur Cluster, prev, p Point, opts Options) bool {
	// The time test is between CONSECUTIVE photos, not against the cluster's
	// start. A four-hour gap is what a break in shooting looks like; a wedding
	// photographed steadily for nine hours is still one event, and measuring
	// from the start would guillotine it at the four-hour mark.
	if p.CapturedAt.Sub(prev.CapturedAt) > opts.MaxGap {
		return true
	}

	// The distance test is against the cluster's ANCHOR, not the previous
	// photo, and that asymmetry is the point. Consecutive-pair comparison
	// permits unbounded drift: photograph every 200m along a coast road and
	// no single step exceeds the threshold, so fifty kilometres of coastline
	// becomes one "place". Anchoring bounds a cluster's radius by
	// construction.
	//
	// The cost is that membership depends on which photo came first, and a
	// genuine walking tour gets cut into segments. That is the better failure:
	// several clusters a person can merge by eye beats one that silently spans
	// a county.
	if !cur.hasAnchor() || !p.HasLocation() {
		// Either the cluster has never seen a location or this photo has none.
		// Nothing to compare, so time alone decides -- a photo with no GPS
		// never breaks a cluster and is never excluded from one.
		return false
	}
	return Haversine(*cur.AnchorLat, *cur.AnchorLon, *p.Latitude, *p.Longitude) > opts.MaxRadiusMeters
}

func (c Cluster) hasAnchor() bool { return c.AnchorLat != nil && c.AnchorLon != nil }

func newCluster(p Point) Cluster {
	c := Cluster{
		PhotoIDs:       []string{p.PhotoID},
		StartedAt:      p.CapturedAt,
		EndedAt:        p.CapturedAt,
		DistanceMeters: map[string]float64{},
	}
	if p.HasLocation() {
		lat, lon := *p.Latitude, *p.Longitude
		c.AnchorLat, c.AnchorLon = &lat, &lon
		c.LocatedCount = 1
		// The anchor is its own reference point, so its distance is exactly 0.
		c.DistanceMeters[p.PhotoID] = 0
	}
	if p.DateSource == SourceFilesystem {
		c.FilesystemDatedCount = 1
	}
	return c
}

func addTo(c *Cluster, p Point) {
	c.PhotoIDs = append(c.PhotoIDs, p.PhotoID)
	if p.CapturedAt.After(c.EndedAt) {
		c.EndedAt = p.CapturedAt
	}
	if p.DateSource == SourceFilesystem {
		c.FilesystemDatedCount++
	}
	if !p.HasLocation() {
		return
	}

	c.LocatedCount++
	if !c.hasAnchor() {
		// First fix in a cluster that started without one. It becomes the
		// anchor, and every earlier member stays -- they joined on time, which
		// is still true. No earlier member can have a distance, because a
		// cluster without an anchor has seen no locations at all.
		lat, lon := *p.Latitude, *p.Longitude
		c.AnchorLat, c.AnchorLon = &lat, &lon
		c.DistanceMeters[p.PhotoID] = 0
		return
	}

	d := Haversine(*c.AnchorLat, *c.AnchorLon, *p.Latitude, *p.Longitude)
	c.DistanceMeters[p.PhotoID] = d
	if d > c.MaxDistanceMeters {
		c.MaxDistanceMeters = d
	}
}

// earthRadiusMeters is the IUGG mean radius. The Earth is an oblate spheroid,
// so any single radius is an approximation: a spherical model is off by up to
// about 0.5% against the WGS-84 ellipsoid. At the scale that matters here --
// deciding whether two photos are within a kilometre or two -- that is metres,
// far inside consumer GPS error, and it buys a formula with no iteration and
// no failure modes.
const earthRadiusMeters = 6371008.8

// Haversine returns the great-circle distance in metres between two points
// given in degrees.
//
// Chosen over the equirectangular approximation, which is faster but degrades
// badly near the poles, and over Vincenty, which is more accurate on the
// ellipsoid but iterative and famously fails to converge for near-antipodal
// points. Haversine is exact for the spherical model at every separation.
//
// It is also numerically stable for SMALL distances, which is the case that
// actually matters here. The spherical law of cosines is algebraically
// equivalent and loses precision catastrophically for nearby points, because
// it feeds a value very close to 1 into acos. Almost every comparison this
// package makes is between two photos a few hundred metres apart.
func Haversine(lat1, lon1, lat2, lon2 float64) float64 {
	const rad = math.Pi / 180

	p1 := lat1 * rad
	p2 := lat2 * rad
	dp := (lat2 - lat1) * rad
	dl := (lon2 - lon1) * rad

	sdp := math.Sin(dp / 2)
	sdl := math.Sin(dl / 2)

	a := sdp*sdp + math.Cos(p1)*math.Cos(p2)*sdl*sdl

	// atan2(sqrt(a), sqrt(1-a)) rather than asin(sqrt(a)): the two agree
	// exactly in exact arithmetic, but rounding can push a a hair above 1 for
	// antipodal points, and asin(>1) is NaN while atan2 stays well defined.
	//
	// Longitude wraparound needs no special handling: sin((l2-l1)/2) is
	// periodic, so a pair straddling the antimeridian -- +179.9 and -179.9 --
	// gives the correct 22km rather than the 40,000km a naive coordinate
	// difference would produce.
	return 2 * earthRadiusMeters * math.Atan2(math.Sqrt(a), math.Sqrt(1-a))
}
