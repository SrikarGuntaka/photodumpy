package clustering

import (
	"fmt"
	"math/rand"
	"testing"
	"time"
)

// BenchmarkSegment measures the BUILD_CLUSTERS computation at library scale.
// It is O(n log n), dominated by the sort, so 10x the photos should cost a
// little over 10x the time.
func BenchmarkSegment(b *testing.B) {
	for _, n := range []int{1_000, 10_000, 100_000} {
		b.Run(fmt.Sprintf("n=%d", n), func(b *testing.B) {
			rng := rand.New(rand.NewSource(1))
			base := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
			pts := make([]Point, n)
			for i := range pts {
				// Bursts of shooting separated by occasional long gaps, and GPS
				// on roughly half the photos -- the realistic mix, not the
				// easiest case for either test.
				p := Point{
					PhotoID:    fmt.Sprintf("p%07d", i),
					CapturedAt: base.Add(time.Duration(rng.Int63n(int64(365 * 24 * time.Hour)))),
					DateSource: SourceEXIF,
				}
				if rng.Intn(2) == 0 {
					lat, lon := 30+rng.Float64(), -97+rng.Float64()
					p.Latitude, p.Longitude = &lat, &lon
				}
				pts[i] = p
			}
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				// Segment sorts a copy, so the input is not pre-sorted for the
				// next iteration.
				Segment(pts, DefaultOptions())
			}
		})
	}
}

func BenchmarkHaversine(b *testing.B) {
	var sink float64
	for i := 0; i < b.N; i++ {
		sink = Haversine(30.2672, -97.7431, 32.7767, -96.7970)
	}
	_ = sink
}
