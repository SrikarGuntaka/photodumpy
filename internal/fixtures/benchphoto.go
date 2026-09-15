package fixtures

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

var (
	benchOnce  sync.Once
	benchBytes []byte
	benchErr   error
)

// BenchmarkJPEG returns the bytes of one generated 4032x3024 (12MP) JPEG with
// EXIF and GPS -- the size a phone camera produces -- for micro-benchmarks.
//
// Generated once per process and cached. Benchmark functions are called several
// times with increasing b.N, so generating inside one would repeat several
// seconds of setup on every call.
//
// The image is synthetic. Its pixel count matches a real photo, which is what
// decode and resampling costs scale with; its file size (under 1 MB) is well
// below a real 12MP JPEG's 3-5 MB, because generated scenes compress far
// better than photographs. Anything that scales with BYTES rather than pixels
// -- SHA-256, disk reads -- is therefore flattered by this input, and should be
// benchmarked with a byte-sized input instead.
func BenchmarkJPEG() ([]byte, error) {
	benchOnce.Do(func() {
		dir, err := os.MkdirTemp("", "photo-organizer-bench-")
		if err != nil {
			benchErr = err
			return
		}
		defer os.RemoveAll(dir)

		if _, err := Generate(Options{Root: dir, Seed: 1, Scenes: 1, Width: 4032, Height: 3024}); err != nil {
			benchErr = err
			return
		}
		// Scene 0 is always the plain photo with full EXIF and GPS.
		benchBytes, benchErr = os.ReadFile(filepath.Join(dir, "scene00.jpg"))
		if benchErr == nil && len(benchBytes) == 0 {
			benchErr = fmt.Errorf("fixtures: generated benchmark JPEG is empty")
		}
	})
	return benchBytes, benchErr
}
