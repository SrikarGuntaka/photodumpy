package thumbnail

import (
	"bytes"
	"image"
	"image/jpeg"
	"testing"

	"github.com/srikarguntaka/photo-organizer/internal/fixtures"
)

// BenchmarkFromReader12MP is one GENERATE_THUMBNAIL job's compute: read EXIF
// orientation, decode a 12MP JPEG, flatten, resample to 400px and orient.
// Encoding and the atomic write are excluded -- see BenchmarkEncodeThumbnail.
func BenchmarkFromReader12MP(b *testing.B) {
	jpg, err := fixtures.BenchmarkJPEG()
	if err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := FromReader(bytes.NewReader(jpg), MaxEdge); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkRender12MP isolates rendering from decoding, to show how the job's
// time divides between the two.
func BenchmarkRender12MP(b *testing.B) {
	jpg, err := fixtures.BenchmarkJPEG()
	if err != nil {
		b.Fatal(err)
	}
	img, err := jpeg.Decode(bytes.NewReader(jpg))
	if err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		Render(img, 1, MaxEdge)
	}
}

func BenchmarkEncodeThumbnail(b *testing.B) {
	img := Render(image.NewRGBA(image.Rect(0, 0, 4032, 3024)), 1, MaxEdge)
	var buf bytes.Buffer
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		buf.Reset()
		if err := Encode(&buf, img); err != nil {
			b.Fatal(err)
		}
	}
}
