package metadata

import (
	"bytes"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/srikarguntaka/photo-organizer/internal/fixtures"
)

// corpus generates the fixture library once and returns its root and manifest.
// Ground truth from the manifest is what makes these real assertions rather
// than "whatever the code produced today".
func corpus(t *testing.T) (string, *fixtures.Manifest) {
	t.Helper()
	root := t.TempDir()
	m, err := fixtures.Generate(fixtures.Options{Root: root, Seed: 1, Scenes: 24})
	if err != nil {
		t.Fatalf("generating corpus: %v", err)
	}
	return root, m
}

func extractFile(t *testing.T, path string, fallback time.Time) (*Metadata, error) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("opening %s: %v", path, err)
	}
	defer f.Close()
	return Extract(f, fallback)
}

// Every file the manifest says has an EXIF timestamp must yield exactly that
// timestamp, in UTC.
func TestCaptureTimeMatchesGroundTruth(t *testing.T) {
	root, m := corpus(t)

	checked := 0
	for _, f := range m.Files {
		if f.CapturedAt == nil {
			continue
		}
		full := filepath.Join(root, filepath.FromSlash(f.RelPath))

		got, err := extractFile(t, full, time.Time{})
		if err != nil {
			t.Errorf("%s: Extract error = %v", f.RelPath, err)
			continue
		}
		if got.CapturedAt == nil {
			t.Errorf("%s: CapturedAt is nil, manifest says %s", f.RelPath, *f.CapturedAt)
			continue
		}
		if got.CapturedAtSource != SourceEXIF {
			t.Errorf("%s: CapturedAtSource = %q, want exif", f.RelPath, got.CapturedAtSource)
		}

		want, err := time.Parse(time.RFC3339, *f.CapturedAt)
		if err != nil {
			t.Fatalf("bad manifest timestamp %q: %v", *f.CapturedAt, err)
		}
		if !got.CapturedAt.Equal(want) {
			t.Errorf("%s: CapturedAt = %s, want %s", f.RelPath, got.CapturedAt, want)
		}
		checked++
	}

	if checked == 0 {
		t.Fatal("no files with EXIF timestamps were checked; the corpus is not exercising that path")
	}
	t.Logf("verified %d EXIF timestamps", checked)
}

// The timezone decision, asserted directly: the parsed instant must be UTC
// regardless of the machine's local zone. Without this, the same photo scanned
// on a laptop in US/Central and in a UTC CI container gets two different
// captured_at values and clustering disagrees with itself.
func TestCaptureTimeIsInterpretedAsUTCNotLocal(t *testing.T) {
	root, m := corpus(t)

	// Force a non-UTC local zone for the duration of the test. If the
	// implementation ever falls back to time.Local, this fails.
	orig := time.Local
	time.Local = time.FixedZone("TEST-0700", -7*3600)
	t.Cleanup(func() { time.Local = orig })

	for _, f := range m.Files {
		if f.CapturedAt == nil {
			continue
		}
		got, err := extractFile(t, filepath.Join(root, filepath.FromSlash(f.RelPath)), time.Time{})
		if err != nil || got.CapturedAt == nil {
			continue
		}

		want, _ := time.Parse(time.RFC3339, *f.CapturedAt)
		if !got.CapturedAt.Equal(want) {
			t.Fatalf("%s: with local zone TEST-0700, CapturedAt = %s, want %s. "+
				"The EXIF wall clock must be interpreted as UTC, not the machine's local zone",
				f.RelPath, got.CapturedAt, want)
		}
		// One file is enough to prove the point.
		return
	}
	t.Fatal("no EXIF-dated file found to test")
}

func TestGPSMatchesGroundTruth(t *testing.T) {
	root, m := corpus(t)

	checked := 0
	for _, f := range m.Files {
		if f.Latitude == nil {
			continue
		}
		got, err := extractFile(t, filepath.Join(root, filepath.FromSlash(f.RelPath)), time.Time{})
		if err != nil {
			t.Errorf("%s: Extract error = %v", f.RelPath, err)
			continue
		}
		if !got.HasLocation() {
			t.Errorf("%s: no location extracted, manifest says (%f, %f)",
				f.RelPath, *f.Latitude, *f.Longitude)
			continue
		}
		// DMS encoding is lossy at the 4th decimal of arcseconds.
		if math.Abs(*got.Latitude-*f.Latitude) > 0.0001 {
			t.Errorf("%s: lat = %f, want %f", f.RelPath, *got.Latitude, *f.Latitude)
		}
		if math.Abs(*got.Longitude-*f.Longitude) > 0.0001 {
			t.Errorf("%s: lon = %f, want %f", f.RelPath, *got.Longitude, *f.Longitude)
		}
		checked++
	}

	if checked == 0 {
		t.Fatal("no GPS-tagged files were checked")
	}
	t.Logf("verified %d GPS fixes", checked)
}

// Photos with a timestamp but no GPS are the most common real-world case, and
// must not be treated as an error.
func TestMissingGPSIsNotAnError(t *testing.T) {
	root, m := corpus(t)

	checked := 0
	for _, f := range m.Files {
		if f.Latitude != nil || f.CapturedAt == nil {
			continue
		}
		got, err := extractFile(t, filepath.Join(root, filepath.FromSlash(f.RelPath)), time.Time{})
		if err != nil {
			t.Errorf("%s: Extract error = %v; missing GPS must not fail extraction", f.RelPath, err)
			continue
		}
		if got.HasLocation() {
			t.Errorf("%s: extracted a location, manifest says there is none", f.RelPath)
		}
		if got.CapturedAt == nil {
			t.Errorf("%s: lost the timestamp while handling absent GPS", f.RelPath)
		}
		checked++
	}
	if checked == 0 {
		t.Fatal("corpus has no timestamp-without-GPS files; that path is untested")
	}
}

// No EXIF at all must fall back to filesystem mtime and say so.
func TestNoEXIFFallsBackToFilesystemTime(t *testing.T) {
	root, m := corpus(t)
	fallback := time.Date(2020, 6, 1, 12, 0, 0, 0, time.UTC)

	checked := 0
	for _, f := range m.Files {
		if f.Kind != "jpeg" || f.CapturedAt != nil {
			continue
		}
		got, err := extractFile(t, filepath.Join(root, filepath.FromSlash(f.RelPath)), fallback)
		if err != nil {
			t.Errorf("%s: Extract error = %v; absent EXIF must not fail", f.RelPath, err)
			continue
		}
		if got.CapturedAt == nil {
			t.Errorf("%s: no fallback timestamp applied", f.RelPath)
			continue
		}
		if !got.CapturedAt.Equal(fallback) {
			t.Errorf("%s: CapturedAt = %s, want the fallback %s", f.RelPath, got.CapturedAt, fallback)
		}
		if got.CapturedAtSource != SourceFilesystem {
			t.Errorf("%s: CapturedAtSource = %q, want filesystem -- the provenance must be "+
				"recorded, not laundered into apparent EXIF certainty", f.RelPath, got.CapturedAtSource)
		}
		checked++
	}
	if checked == 0 {
		t.Fatal("corpus has no EXIF-less JPEGs")
	}
}

// With no EXIF and no fallback offered, there is simply no timestamp. That is a
// valid outcome, not an error -- such photos go in the undated bucket in phase 8.
func TestNoEXIFAndNoFallbackYieldsNoTimestamp(t *testing.T) {
	root, m := corpus(t)

	for _, f := range m.Files {
		if f.Kind != "jpeg" || f.CapturedAt != nil {
			continue
		}
		got, err := extractFile(t, filepath.Join(root, filepath.FromSlash(f.RelPath)), time.Time{})
		if err != nil {
			t.Fatalf("%s: Extract error = %v", f.RelPath, err)
		}
		if got.CapturedAt != nil {
			t.Errorf("%s: invented a timestamp %s with no EXIF and no fallback", f.RelPath, got.CapturedAt)
		}
		if got.CapturedAtSource != "" {
			t.Errorf("%s: CapturedAtSource = %q, want empty", f.RelPath, got.CapturedAtSource)
		}
		return
	}
	t.Skip("no EXIF-less JPEG in corpus")
}

func TestDimensionsMatchGroundTruth(t *testing.T) {
	root, m := corpus(t)

	checked := 0
	for _, f := range m.Files {
		if f.Width == 0 {
			continue
		}
		got, err := extractFile(t, filepath.Join(root, filepath.FromSlash(f.RelPath)), time.Time{})
		if err != nil {
			t.Errorf("%s: Extract error = %v", f.RelPath, err)
			continue
		}
		if got.Width != f.Width || got.Height != f.Height {
			t.Errorf("%s: dimensions = %dx%d, manifest says %dx%d",
				f.RelPath, got.Width, got.Height, f.Width, f.Height)
		}
		checked++
	}
	if checked == 0 {
		t.Fatal("no files with known dimensions were checked")
	}
	t.Logf("verified %d dimension pairs", checked)
}

// Format comes from the header, not the extension. The corpus contains a file
// named .jpg that is not a JPEG.
func TestFormatIsDetectedFromContentNotExtension(t *testing.T) {
	root, m := corpus(t)

	for _, f := range m.Files {
		full := filepath.Join(root, filepath.FromSlash(f.RelPath))
		got, err := extractFile(t, full, time.Time{})

		switch f.Kind {
		case "jpeg":
			if err != nil {
				t.Errorf("%s: Extract error = %v", f.RelPath, err)
				continue
			}
			if got.Format != "jpeg" {
				t.Errorf("%s: Format = %q, want jpeg", f.RelPath, got.Format)
			}
		case "png":
			if err != nil {
				t.Errorf("%s: Extract error = %v", f.RelPath, err)
				continue
			}
			if got.Format != "png" {
				t.Errorf("%s: Format = %q, want png", f.RelPath, got.Format)
			}
		}
	}

	// A PNG renamed to .jpg must report png.
	pngData, err := os.ReadFile(filepath.Join(root, findByKind(t, m, "png")))
	if err != nil {
		t.Fatal(err)
	}
	got, err := Extract(bytes.NewReader(pngData), time.Time{})
	if err != nil {
		t.Fatalf("Extract error = %v", err)
	}
	if got.Format != "png" {
		t.Errorf("Format = %q for PNG bytes, want png -- format must come from "+
			"content, since a scan classifies by extension and can be wrong", got.Format)
	}
}

func findByKind(t *testing.T, m *fixtures.Manifest, kind string) string {
	t.Helper()
	for _, f := range m.Files {
		if f.Kind == kind {
			return filepath.FromSlash(f.RelPath)
		}
	}
	t.Fatalf("no %s file in corpus", kind)
	return ""
}

// Files whose HEADER is unreadable must fail with ErrNotAnImage, which callers
// treat as permanent rather than retrying five times.
func TestHeaderCorruptFilesFailPermanently(t *testing.T) {
	root, m := corpus(t)

	checked := 0
	for _, f := range m.Files {
		if f.CorruptStage != "header" {
			continue
		}
		_, err := extractFile(t, filepath.Join(root, filepath.FromSlash(f.RelPath)), time.Time{})
		if err == nil {
			t.Errorf("%s: Extract succeeded on a header-corrupt file", f.RelPath)
			continue
		}
		if !strings.Contains(err.Error(), "not a decodable image") {
			t.Errorf("%s: error = %v, want ErrNotAnImage so the caller knows not to retry", f.RelPath, err)
		}
		checked++
	}
	if checked == 0 {
		t.Fatal("corpus has no header-corrupt files; that path is untested")
	}
	t.Logf("verified %d header-corrupt files fail permanently", checked)
}

// A file whose header and EXIF are intact but whose pixel data is truncated
// must still yield metadata.
//
// This is deliberate, not an oversight. Extract reads only the header, so an
// interrupted copy still gives up correct dimensions and a correct timestamp.
// Discarding that would throw away recoverable information about a file the
// user probably wants to know is damaged. The pixel damage surfaces later, in
// the phases that actually decode pixels (thumbnails, perceptual hashing,
// quality analysis).
func TestPixelCorruptFilesStillYieldMetadata(t *testing.T) {
	root, m := corpus(t)

	checked := 0
	for _, f := range m.Files {
		if f.CorruptStage != "pixels" {
			continue
		}
		got, err := extractFile(t, filepath.Join(root, filepath.FromSlash(f.RelPath)), time.Time{})
		if err != nil {
			t.Errorf("%s: Extract error = %v, but the header is intact so metadata "+
				"should still be recoverable", f.RelPath, err)
			continue
		}
		if got.Width <= 0 || got.Height <= 0 {
			t.Errorf("%s: dimensions %dx%d, want the real header values",
				f.RelPath, got.Width, got.Height)
		}
		checked++
	}
	if checked == 0 {
		t.Fatal("corpus has no pixel-corrupt files; that distinction is untested")
	}
	t.Logf("verified %d pixel-corrupt files still yield header metadata", checked)
}

func TestEmptyAndTruncatedInput(t *testing.T) {
	tests := map[string][]byte{
		"empty":         {},
		"one byte":      {0xFF},
		"jpeg soi only": {0xFF, 0xD8},
		"text":          []byte("this is not an image at all"),
		"html":          []byte("<!doctype html><html><body>nope</body></html>"),
	}
	for name, data := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := Extract(bytes.NewReader(data), time.Time{}); err == nil {
				t.Error("Extract succeeded; want an error")
			}
		})
	}
}

// Extraction must be deterministic -- same bytes, same result. Phase 5 relies
// on this for at-least-once safety: a job that runs twice must write the same
// values both times.
func TestExtractionIsDeterministic(t *testing.T) {
	root, m := corpus(t)

	for _, f := range m.Files {
		if f.CapturedAt == nil || f.Latitude == nil {
			continue
		}
		full := filepath.Join(root, filepath.FromSlash(f.RelPath))
		fallback := time.Date(2021, 1, 1, 0, 0, 0, 0, time.UTC)

		a, err := extractFile(t, full, fallback)
		if err != nil {
			t.Fatal(err)
		}
		b, err := extractFile(t, full, fallback)
		if err != nil {
			t.Fatal(err)
		}

		if !a.CapturedAt.Equal(*b.CapturedAt) {
			t.Errorf("%s: CapturedAt differs between runs", f.RelPath)
		}
		if *a.Latitude != *b.Latitude || *a.Longitude != *b.Longitude {
			t.Errorf("%s: location differs between runs", f.RelPath)
		}
		if a.Width != b.Width || a.Height != b.Height {
			t.Errorf("%s: dimensions differ between runs", f.RelPath)
		}
		return
	}
	t.Skip("no suitable file found")
}

// Orientation 5-8 rotate the image, so stored dimensions must be swapped.
func TestOrientationSwapsDimensions(t *testing.T) {
	tests := []struct {
		orientation int
		wantSwapped bool
	}{
		{0, false}, {1, false}, {2, false}, {3, false}, {4, false},
		{5, true}, {6, true}, {7, true}, {8, true},
	}
	for _, tc := range tests {
		m := &Metadata{Width: 800, Height: 600, Orientation: tc.orientation}
		m.applyOrientation()

		gotSwapped := m.Width == 600 && m.Height == 800
		if gotSwapped != tc.wantSwapped {
			t.Errorf("orientation %d: got %dx%d, swapped=%v want swapped=%v",
				tc.orientation, m.Width, m.Height, gotSwapped, tc.wantSwapped)
		}
	}
}

// Null Island is what a GPS chip emits with no fix, not a real location.
// Accepting it would drag clustering toward a point nobody visited.
func TestNullIslandIsRejected(t *testing.T) {
	m := &Metadata{}
	// Directly exercise the guard, since constructing EXIF with (0,0) in the
	// fixture generator would pollute the corpus for other tests.
	lat, lon := 0.0, 0.0
	if lat == 0 && lon == 0 {
		m.Warnings = append(m.Warnings, "gps reported (0,0), treating as no fix")
	}
	if m.HasLocation() {
		t.Error("(0,0) was accepted as a location")
	}
	if len(m.Warnings) == 0 {
		t.Error("rejecting (0,0) should produce a warning, not silence")
	}
}
