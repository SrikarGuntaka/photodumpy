package hashing

import (
	"bytes"
	"testing"

	"github.com/srikarguntaka/photo-organizer/internal/fixtures"
)

// BenchmarkDHashReader12MP is one COMPUTE_PERCEPTUAL_HASH job's compute: decode
// a 12MP JPEG and reduce it to 64 bits. Decode dominates; the hash itself is a
// 9x8 comparison.
func BenchmarkDHashReader12MP(b *testing.B) {
	jpg, err := fixtures.BenchmarkJPEG()
	if err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := DHashReader(bytes.NewReader(jpg)); err != nil {
			b.Fatal(err)
		}
	}
}
