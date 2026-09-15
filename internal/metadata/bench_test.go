package metadata

import (
	"bytes"
	"testing"
	"time"

	"github.com/srikarguntaka/photo-organizer/internal/fixtures"
)

// BenchmarkExtract12MP is one EXTRACT_METADATA job's compute on a 12MP JPEG.
//
// The number that matters is how small it is next to the decoding benchmarks:
// dimensions come from the image header and EXIF from its APP1 segment, so the
// pixel data is never decoded. Its cost should not grow with resolution.
func BenchmarkExtract12MP(b *testing.B) {
	jpg, err := fixtures.BenchmarkJPEG()
	if err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := Extract(bytes.NewReader(jpg), time.Time{}); err != nil {
			b.Fatal(err)
		}
	}
}
