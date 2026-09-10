package clustering

import (
	"fmt"
	"math"
	"testing"
	"time"
)

func at(min int) time.Time {
	return time.Date(2026, 3, 14, 9, 0, 0, 0, time.UTC).Add(time.Duration(min) * time.Minute)
}

func ptr(f float64) *float64 { return &f }

// p builds a dated point with no location.
func p(id string, minutes int) Point {
	return Point{PhotoID: id, CapturedAt: at(minutes), DateSource: SourceEXIF}
}

// pl builds a dated point with a location.
func pl(id string, minutes int, lat, lon float64) Point {
	q := p(id, minutes)
	q.Latitude, q.Longitude = ptr(lat), ptr(lon)
	return q
}

func ids(c Cluster) []string { return c.PhotoIDs }

func sameIDs(got []string, want ...string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func TestSplitsOnTimeGap(t *testing.T) {
	res := Segment([]Point{
		p("a", 0),
		p("b", 30),
		p("c", 60),
		// 5 hours later: a new event.
		p("d", 60+5*60),
		p("e", 60+5*60+10),
	}, DefaultOptions())

	if len(res.Clusters) != 2 {
		t.Fatalf("got %d clusters, want 2", len(res.Clusters))
	}
	if !sameIDs(ids(res.Clusters[0]), "a", "b", "c") {
		t.Errorf("cluster 0 = %v", ids(res.Clusters[0]))
	}
	if !sameIDs(ids(res.Clusters[1]), "d", "e") {
		t.Errorf("cluster 1 = %v", ids(res.Clusters[1]))
	}
}

// A long shoot with no long pause is ONE event, even though it far exceeds the
// gap threshold end to end. Regression against measuring the gap from the
// cluster start rather than from the previous photo.
func TestSteadyShootingIsOneEvent(t *testing.T) {
	var pts []Point
	for i := 0; i < 60; i++ {
		pts = append(pts, p(fmt.Sprintf("p%02d", i), i*20)) // every 20 min for 20 hours
	}

	res := Segment(pts, DefaultOptions())
	if len(res.Clusters) != 1 {
		t.Fatalf("got %d clusters, want 1 -- a 20-hour wedding with no 4-hour pause is one event", len(res.Clusters))
	}
	if got := res.Clusters[0].EndedAt.Sub(res.Clusters[0].StartedAt); got != 1180*time.Minute {
		t.Errorf("span = %v, want 1180m", got)
	}
}

func TestGapExactlyAtThresholdDoesNotSplit(t *testing.T) {
	opts := DefaultOptions()
	res := Segment([]Point{p("a", 0), p("b", 240)}, opts) // exactly 4h

	if len(res.Clusters) != 1 {
		t.Fatalf("got %d clusters, want 1 -- the threshold is exclusive", len(res.Clusters))
	}

	res = Segment([]Point{p("a", 0), p("b", 241)}, opts)
	if len(res.Clusters) != 2 {
		t.Fatalf("got %d clusters, want 2 -- one minute past the threshold splits", len(res.Clusters))
	}
}

func TestSplitsOnDistance(t *testing.T) {
	// Austin and Dallas: same afternoon, 300km apart.
	res := Segment([]Point{
		pl("austin1", 0, 30.2672, -97.7431),
		pl("austin2", 10, 30.2680, -97.7440),
		pl("dallas", 60, 32.7767, -96.7970),
	}, DefaultOptions())

	if len(res.Clusters) != 2 {
		t.Fatalf("got %d clusters, want 2", len(res.Clusters))
	}
	if !sameIDs(ids(res.Clusters[0]), "austin1", "austin2") {
		t.Errorf("cluster 0 = %v", ids(res.Clusters[0]))
	}
}

// THE constraint: a photo without GPS must not break a cluster, and must not
// be excluded from one.
func TestPhotosWithoutGPSJoinOnTimeAlone(t *testing.T) {
	res := Segment([]Point{
		pl("gps1", 0, 30.2672, -97.7431),
		p("nogps", 10),
		pl("gps2", 20, 30.2680, -97.7440),
	}, DefaultOptions())

	if len(res.Clusters) != 1 {
		t.Fatalf("got %d clusters, want 1 -- a photo with no GPS must not split an event", len(res.Clusters))
	}
	if !sameIDs(ids(res.Clusters[0]), "gps1", "nogps", "gps2") {
		t.Fatalf("cluster = %v, want all three", ids(res.Clusters[0]))
	}
	if res.Clusters[0].LocatedCount != 2 {
		t.Errorf("LocatedCount = %d, want 2", res.Clusters[0].LocatedCount)
	}
}

// A library with no GPS at all -- the common case -- must still cluster.
func TestNoGPSAnywhereStillClusters(t *testing.T) {
	res := Segment([]Point{
		p("a", 0), p("b", 30),
		p("c", 30+300), p("d", 30+310),
	}, DefaultOptions())

	if len(res.Clusters) != 2 {
		t.Fatalf("got %d clusters, want 2", len(res.Clusters))
	}
	for i, c := range res.Clusters {
		if c.AnchorLat != nil || c.AnchorLon != nil {
			t.Errorf("cluster %d has an anchor from photos with no GPS", i)
		}
		if c.LocatedCount != 0 {
			t.Errorf("cluster %d LocatedCount = %d, want 0", i, c.LocatedCount)
		}
	}
}

// A cluster that begins with unlocated photos adopts the first fix it sees,
// and keeps the members that joined before it.
func TestAnchorAdoptedFromFirstFix(t *testing.T) {
	res := Segment([]Point{
		p("nogps1", 0),
		p("nogps2", 10),
		pl("gps", 20, 30.2672, -97.7431),
	}, DefaultOptions())

	if len(res.Clusters) != 1 {
		t.Fatalf("got %d clusters, want 1", len(res.Clusters))
	}
	c := res.Clusters[0]
	if !sameIDs(ids(c), "nogps1", "nogps2", "gps") {
		t.Fatalf("cluster = %v", ids(c))
	}
	if c.AnchorLat == nil || math.Abs(*c.AnchorLat-30.2672) > 1e-9 {
		t.Errorf("anchor not adopted from the first located member")
	}
}

// Anchoring bounds a cluster's radius. Without it, small steps chain
// indefinitely and a coast road becomes one "place".
func TestDriftDoesNotChain(t *testing.T) {
	// 30 photos, each ~300m further east, 5 minutes apart. No consecutive step
	// exceeds the 1500m radius, but the total span is ~9km.
	var pts []Point
	lon := -97.7431
	for i := 0; i < 30; i++ {
		pts = append(pts, pl(fmt.Sprintf("p%02d", i), i*5, 30.2672, lon))
		lon += 0.0031 // ~300m at this latitude
	}

	res := Segment(pts, DefaultOptions())
	if len(res.Clusters) < 2 {
		t.Fatalf("got %d cluster(s); anchoring should bound the radius rather than let 9km chain into one", len(res.Clusters))
	}
	for i, c := range res.Clusters {
		if c.MaxDistanceMeters > DefaultOptions().MaxRadiusMeters {
			t.Errorf("cluster %d spans %.0fm, beyond the %.0fm radius",
				i, c.MaxDistanceMeters, DefaultOptions().MaxRadiusMeters)
		}
	}
}

func TestUndatedPhotosAreReportedNotGuessed(t *testing.T) {
	res := Segment([]Point{
		p("dated", 0),
		{PhotoID: "undated1"},
		{PhotoID: "undated2"},
	}, DefaultOptions())

	if len(res.Clusters) != 1 || !sameIDs(ids(res.Clusters[0]), "dated") {
		t.Fatalf("clusters = %v", res.Clusters)
	}
	if len(res.UndatedPhotoIDs) != 2 {
		t.Fatalf("UndatedPhotoIDs = %v, want 2 entries", res.UndatedPhotoIDs)
	}
}

func TestEmptyInput(t *testing.T) {
	res := Segment(nil, DefaultOptions())
	if len(res.Clusters) != 0 || len(res.UndatedPhotoIDs) != 0 {
		t.Fatalf("got %+v, want empty", res)
	}
}

func TestOrderIndependence(t *testing.T) {
	forward := []Point{p("a", 0), p("b", 20), p("c", 400), p("d", 420)}
	shuffled := []Point{p("c", 400), p("a", 0), p("d", 420), p("b", 20)}

	a := Segment(forward, DefaultOptions())
	b := Segment(shuffled, DefaultOptions())

	if len(a.Clusters) != len(b.Clusters) {
		t.Fatalf("%d vs %d clusters", len(a.Clusters), len(b.Clusters))
	}
	for i := range a.Clusters {
		if !sameIDs(ids(a.Clusters[i]), ids(b.Clusters[i])...) {
			t.Errorf("cluster %d differs: %v vs %v", i, ids(a.Clusters[i]), ids(b.Clusters[i]))
		}
	}
}

func TestSameTimestampTieBrokenDeterministically(t *testing.T) {
	// A burst: three photos sharing a timestamp to the second.
	a := Segment([]Point{p("c", 0), p("a", 0), p("b", 0)}, DefaultOptions())
	if !sameIDs(ids(a.Clusters[0]), "a", "b", "c") {
		t.Fatalf("got %v, want id order", ids(a.Clusters[0]))
	}
}

func TestConfidenceReflectsDateSource(t *testing.T) {
	fs := func(id string, min int) Point {
		q := p(id, min)
		q.DateSource = SourceFilesystem
		return q
	}

	cases := []struct {
		name string
		pts  []Point
		want string
	}{
		{"all exif", []Point{p("a", 0), p("b", 10)}, "high"},
		{"all mtime", []Point{fs("a", 0), fs("b", 10)}, "low"},
		{"mixed", []Point{p("a", 0), fs("b", 10)}, "mixed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := Segment(tc.pts, DefaultOptions())
			if got := res.Clusters[0].Confidence(); got != tc.want {
				t.Errorf("Confidence() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestZeroOptionsUseDefaults(t *testing.T) {
	res := Segment([]Point{p("a", 0), p("b", 300)}, Options{})
	if len(res.Clusters) != 2 {
		t.Fatalf("got %d clusters; zero Options should fall back to the defaults", len(res.Clusters))
	}
}

// Regression: a caller that persists "the thresholds this was built with"
// needs the EFFECTIVE values. Storing an unset zero recorded that clusters
// were built with a four-hour gap of zero seconds -- caught in integration by
// a CHECK constraint, which is the right place for it to be caught but the
// wrong place to be relying on.
func TestWithDefaultsResolvesUnsetFields(t *testing.T) {
	got := Options{}.WithDefaults()
	want := DefaultOptions()
	if got != want {
		t.Fatalf("WithDefaults() = %+v, want %+v", got, want)
	}

	// Explicit values survive, including ones that differ from the defaults.
	custom := Options{MaxGap: 90 * time.Minute, MaxRadiusMeters: 250}
	if got := custom.WithDefaults(); got != custom {
		t.Errorf("WithDefaults() = %+v, want the caller's %+v unchanged", got, custom)
	}

	// One field set, the other not.
	half := Options{MaxGap: 90 * time.Minute}.WithDefaults()
	if half.MaxGap != 90*time.Minute {
		t.Errorf("MaxGap = %v, want the explicit 90m", half.MaxGap)
	}
	if half.MaxRadiusMeters != DefaultOptions().MaxRadiusMeters {
		t.Errorf("MaxRadiusMeters = %v, want the default", half.MaxRadiusMeters)
	}
}

// The distance map distinguishes "no fix" from "at the anchor". Conflating
// them would place every unlocated photo at the centre of the map.
func TestUnlocatedMembersHaveNoDistance(t *testing.T) {
	res := Segment([]Point{
		pl("anchor", 0, 30.2672, -97.7431),
		p("nogps", 10),
		pl("near", 20, 30.2680, -97.7440),
	}, DefaultOptions())

	c := res.Clusters[0]
	if d, ok := c.DistanceMeters["anchor"]; !ok || d != 0 {
		t.Errorf("anchor distance = %v (present %v), want exactly 0", d, ok)
	}
	if _, ok := c.DistanceMeters["nogps"]; ok {
		t.Errorf("a photo with no GPS must be ABSENT from the distance map, not present with 0")
	}
	d, ok := c.DistanceMeters["near"]
	if !ok || d <= 0 {
		t.Errorf("near distance = %v (present %v), want a positive distance", d, ok)
	}
	if d > c.MaxDistanceMeters {
		t.Errorf("member distance %v exceeds MaxDistanceMeters %v", d, c.MaxDistanceMeters)
	}
}

// ---------------------------------------------------------------------------
// Haversine
// ---------------------------------------------------------------------------

func TestHaversineKnownDistances(t *testing.T) {
	cases := []struct {
		name                   string
		lat1, lon1, lat2, lon2 float64
		wantMeters             float64
		tolerance              float64
	}{
		// Reference values from the spherical model at r = 6371008.8m.
		{"identical points", 30.2672, -97.7431, 30.2672, -97.7431, 0, 0},
		{"Austin to Dallas", 30.2672, -97.7431, 32.7767, -96.7970, 293_000, 3_000},
		{"London to Paris", 51.5074, -0.1278, 48.8566, 2.3522, 343_500, 3_000},
		{"one degree of latitude", 0, 0, 1, 0, 111_195, 100},
		// Longitude wraparound: these are 22km apart, not 40,000km.
		{"across the antimeridian", 0, 179.9, 0, -179.9, 22_240, 100},
		// Antipodal: the case that makes asin-based formulations return NaN.
		{"antipodal", 0, 0, 0, 180, 20_015_000, 5_000},
		{"poles", 90, 0, -90, 0, 20_015_000, 5_000},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Haversine(tc.lat1, tc.lon1, tc.lat2, tc.lon2)
			if math.IsNaN(got) {
				t.Fatalf("Haversine returned NaN")
			}
			if math.Abs(got-tc.wantMeters) > tc.tolerance {
				t.Errorf("got %.0fm, want %.0f +/- %.0f", got, tc.wantMeters, tc.tolerance)
			}
		})
	}
}

func TestHaversineIsSymmetric(t *testing.T) {
	a := Haversine(30.2672, -97.7431, 32.7767, -96.7970)
	b := Haversine(32.7767, -96.7970, 30.2672, -97.7431)
	if math.Abs(a-b) > 1e-6 {
		t.Errorf("asymmetric: %.6f vs %.6f", a, b)
	}
}

// Small separations are the case that actually matters, and the one the
// spherical law of cosines gets wrong.
func TestHaversinePrecisionAtShortRange(t *testing.T) {
	// One ten-thousandth of a degree of latitude is ~11.1m.
	got := Haversine(30.2672, -97.7431, 30.2673, -97.7431)
	if math.Abs(got-11.12) > 0.5 {
		t.Errorf("got %.2fm, want ~11.1m", got)
	}
	if got <= 0 {
		t.Errorf("got %v; a real separation must not collapse to zero", got)
	}
}

func TestHaversineHandlesEquatorAndPolesWithoutNaN(t *testing.T) {
	for _, lat := range []float64{-90, -89.999, 0, 89.999, 90} {
		for _, lon := range []float64{-180, -0.001, 0, 179.999, 180} {
			if d := Haversine(lat, lon, 0, 0); math.IsNaN(d) || math.IsInf(d, 0) {
				t.Errorf("Haversine(%v,%v,0,0) = %v", lat, lon, d)
			}
		}
	}
}
