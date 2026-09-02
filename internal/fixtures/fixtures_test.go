package fixtures

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"image/jpeg"
	"image/png"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rwcarlsen/goexif/exif"
)

// generate builds a corpus in a temp dir once per test.
func generate(t *testing.T, scenes int) (string, *Manifest) {
	t.Helper()
	root := t.TempDir()
	m, err := Generate(Options{Root: root, Seed: 1, Scenes: scenes})
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}
	return root, m
}

// The whole point of the corpus is that its EXIF is real. This decodes it with
// a third-party parser rather than our own code -- a bug shared between the
// generator and the extractor would otherwise cancel out and the test would
// pass while the data was wrong.
func TestGeneratedEXIFIsParseableByThirdPartyLibrary(t *testing.T) {
	root, m := generate(t, 24)

	checkedTime, checkedGPS := 0, 0

	for _, f := range m.Files {
		if f.CapturedAt == nil && f.Latitude == nil {
			continue
		}

		full := filepath.Join(root, filepath.FromSlash(f.RelPath))
		fh, err := os.Open(full)
		if err != nil {
			t.Fatalf("opening %s: %v", f.RelPath, err)
		}
		x, err := exif.Decode(fh)
		fh.Close()
		if err != nil {
			t.Errorf("%s: EXIF did not parse: %v", f.RelPath, err)
			continue
		}

		if f.CapturedAt != nil {
			got, err := x.DateTime()
			if err != nil {
				t.Errorf("%s: DateTime() error = %v", f.RelPath, err)
			} else {
				want, _ := time.Parse(time.RFC3339, *f.CapturedAt)

				// Compare WALL-CLOCK FIELDS, not instants.
				//
				// EXIF DateTimeOriginal is a naive local timestamp with no
				// timezone. goexif therefore attaches time.Local, so on a
				// machine in US/Central the same bytes decode to 09:47 -0500
				// while the manifest records 09:47 UTC. Those are different
				// instants but identical EXIF data, and time.Equal would fail
				// on a developer machine while passing in a UTC CI container.
				//
				// Phase 3 consequence: the metadata extractor must NOT store
				// whatever zone the parser guessed, or the same photo scanned
				// on two machines gets two different captured_at values and
				// clustering silently disagrees with itself. Interpret EXIF
				// wall-clock as UTC consistently and record that choice in
				// captured_at_source.
				gy, gm, gd := got.Date()
				wy, wm, wd := want.Date()
				sameDate := gy == wy && gm == wm && gd == wd
				sameTime := got.Hour() == want.Hour() &&
					got.Minute() == want.Minute() &&
					got.Second() == want.Second()

				if !sameDate || !sameTime {
					t.Errorf("%s: DateTime wall clock = %s, manifest says %s",
						f.RelPath,
						got.Format("2006-01-02 15:04:05"),
						want.Format("2006-01-02 15:04:05"))
				}
				checkedTime++
			}
		}

		if f.Latitude != nil {
			lat, lon, err := x.LatLong()
			if err != nil {
				t.Errorf("%s: LatLong() error = %v", f.RelPath, err)
				continue
			}
			// DMS encoding is lossy at the 4th decimal of arcseconds.
			if math.Abs(lat-*f.Latitude) > 0.0001 {
				t.Errorf("%s: lat = %f, manifest says %f", f.RelPath, lat, *f.Latitude)
			}
			if math.Abs(lon-*f.Longitude) > 0.0001 {
				t.Errorf("%s: lon = %f, manifest says %f", f.RelPath, lon, *f.Longitude)
			}
			checkedGPS++
		}
	}

	if checkedTime == 0 {
		t.Error("no files with EXIF timestamps were checked; the corpus is not exercising that path")
	}
	if checkedGPS == 0 {
		t.Error("no files with GPS were checked; the corpus is not exercising that path")
	}
	t.Logf("verified %d EXIF timestamps and %d GPS pairs against goexif", checkedTime, checkedGPS)
}

// Files the manifest says have no EXIF must genuinely have none -- otherwise
// the "missing metadata" code paths are never exercised.
func TestFilesWithoutEXIFReallyHaveNone(t *testing.T) {
	root, m := generate(t, 24)

	checked := 0
	for _, f := range m.Files {
		if f.Kind != "jpeg" || f.CapturedAt != nil {
			continue
		}
		full := filepath.Join(root, filepath.FromSlash(f.RelPath))
		fh, err := os.Open(full)
		if err != nil {
			t.Fatal(err)
		}
		_, err = exif.Decode(fh)
		fh.Close()

		if err == nil {
			t.Errorf("%s: manifest says no EXIF timestamp, but EXIF decoded successfully", f.RelPath)
		}
		checked++
	}
	if checked == 0 {
		t.Error("corpus contains no EXIF-less JPEGs; that failure path is untested")
	}
}

// Photos with a timestamp but no GPS are the most common real-world case.
func TestTimestampWithoutGPSParsesAndHasNoLocation(t *testing.T) {
	root, m := generate(t, 24)

	checked := 0
	for _, f := range m.Files {
		if f.CapturedAt == nil || f.Latitude != nil {
			continue
		}
		full := filepath.Join(root, filepath.FromSlash(f.RelPath))
		fh, err := os.Open(full)
		if err != nil {
			t.Fatal(err)
		}
		x, err := exif.Decode(fh)
		fh.Close()
		if err != nil {
			t.Errorf("%s: EXIF did not parse: %v", f.RelPath, err)
			continue
		}
		if _, _, err := x.LatLong(); err == nil {
			t.Errorf("%s: manifest says no GPS, but LatLong() succeeded", f.RelPath)
		}
		checked++
	}
	if checked == 0 {
		t.Error("corpus has no timestamp-without-GPS files; that path is untested")
	}
}

// Files claimed to be byte-identical must actually hash identically, and files
// in different duplicate groups must not collide.
func TestExactDuplicatesAreByteIdentical(t *testing.T) {
	root, m := generate(t, 24)

	hashes := map[string][]string{} // group -> hashes
	for _, f := range m.Files {
		if f.DuplicateGroup == "" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(f.RelPath)))
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(data)
		hashes[f.DuplicateGroup] = append(hashes[f.DuplicateGroup], string(sum[:]))
	}

	if len(hashes) == 0 {
		t.Fatal("corpus contains no duplicate groups")
	}

	seen := map[string]string{}
	for group, hs := range hashes {
		if len(hs) < 2 {
			t.Errorf("duplicate group %s has only %d member(s)", group, len(hs))
			continue
		}
		for _, h := range hs[1:] {
			if h != hs[0] {
				t.Errorf("group %s: members are not byte-identical", group)
			}
		}
		if prev, dup := seen[hs[0]]; dup {
			t.Errorf("groups %s and %s share the same content; they should be distinct", prev, group)
		}
		seen[hs[0]] = group
	}
}

// Near-duplicates must NOT be byte-identical -- if they were, SHA-256 would
// catch them and the perceptual-hash path would never be exercised.
func TestNearDuplicatesAreNotByteIdentical(t *testing.T) {
	root, m := generate(t, 24)

	groups := map[string][][32]byte{}
	for _, f := range m.Files {
		if f.SimilarGroup == "" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(f.RelPath)))
		if err != nil {
			t.Fatal(err)
		}
		groups[f.SimilarGroup] = append(groups[f.SimilarGroup], sha256.Sum256(data))
	}

	if len(groups) == 0 {
		t.Fatal("corpus contains no near-duplicate groups")
	}
	for group, hs := range groups {
		if len(hs) < 2 {
			t.Errorf("similar group %s has only %d member(s)", group, len(hs))
		}
		for i := range hs {
			for j := i + 1; j < len(hs); j++ {
				if hs[i] == hs[j] {
					t.Errorf("group %s: members %d and %d are byte-identical; "+
						"they must differ or the perceptual-hash path is untested", group, i, j)
				}
			}
		}
	}
}

// Every file the manifest calls a decodable image must decode; every file it
// calls corrupt must fail. A "corrupt" file that happens to decode would make
// the error-handling tests vacuous.
func TestDecodabilityMatchesManifest(t *testing.T) {
	root, m := generate(t, 24)

	for _, f := range m.Files {
		full := filepath.Join(root, filepath.FromSlash(f.RelPath))
		data, err := os.ReadFile(full)
		if err != nil {
			t.Fatal(err)
		}

		switch f.Kind {
		case "jpeg":
			img, err := jpeg.Decode(bytes.NewReader(data))
			if err != nil {
				t.Errorf("%s: expected a decodable JPEG, got %v", f.RelPath, err)
				continue
			}
			if f.Width > 0 {
				if b := img.Bounds(); b.Dx() != f.Width || b.Dy() != f.Height {
					t.Errorf("%s: decoded %dx%d, manifest says %dx%d",
						f.RelPath, b.Dx(), b.Dy(), f.Width, f.Height)
				}
			}
		case "png":
			if _, err := png.Decode(bytes.NewReader(data)); err != nil {
				t.Errorf("%s: expected a decodable PNG, got %v", f.RelPath, err)
			}
		case "corrupt":
			if _, err := jpeg.Decode(bytes.NewReader(data)); err == nil {
				t.Errorf("%s: manifest says corrupt, but it decoded cleanly", f.RelPath)
			}
		}
	}
}

// Determinism is what lets a test compare hashes across runs and what makes a
// benchmark reproducible.
func TestGenerationIsDeterministic(t *testing.T) {
	rootA, mA := generate(t, 12)
	rootB := t.TempDir()
	mB, err := Generate(Options{Root: rootB, Seed: 1, Scenes: 12})
	if err != nil {
		t.Fatal(err)
	}

	if len(mA.Files) != len(mB.Files) {
		t.Fatalf("file counts differ: %d vs %d", len(mA.Files), len(mB.Files))
	}
	for i := range mA.Files {
		a, b := mA.Files[i], mB.Files[i]
		if a.RelPath != b.RelPath {
			t.Fatalf("file %d: paths differ: %s vs %s", i, a.RelPath, b.RelPath)
		}
		da, err := os.ReadFile(filepath.Join(rootA, filepath.FromSlash(a.RelPath)))
		if err != nil {
			t.Fatal(err)
		}
		db, err := os.ReadFile(filepath.Join(rootB, filepath.FromSlash(b.RelPath)))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(da, db) {
			t.Errorf("%s: content differs between runs with the same seed", a.RelPath)
		}
	}
}

// A different seed must produce different image content, otherwise the seed is
// decorative.
func TestDifferentSeedProducesDifferentContent(t *testing.T) {
	rootA := t.TempDir()
	if _, err := Generate(Options{Root: rootA, Seed: 1, Scenes: 8}); err != nil {
		t.Fatal(err)
	}
	rootB := t.TempDir()
	if _, err := Generate(Options{Root: rootB, Seed: 99, Scenes: 8}); err != nil {
		t.Fatal(err)
	}
	// Hash the whole corpus rather than individual files: the requirement is
	// that two seeds produce materially different image content, not that
	// every single byte of every file differs.
	sumDir := func(root string) [32]byte {
		h := sha256.New()
		_ = filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() || strings.HasSuffix(p, "MANIFEST.json") {
				return nil
			}
			d, _ := os.ReadFile(p)
			h.Write(d)
			return nil
		})
		var out [32]byte
		copy(out[:], h.Sum(nil))
		return out
	}
	if sumDir(rootA) == sumDir(rootB) {
		t.Error("seeds 1 and 99 produced identical corpora; the seed has no effect")
	}
}

// The manifest must be valid JSON on disk and agree with what Generate
// returned -- tests read the file, not the return value.
func TestManifestOnDiskMatchesReturnValue(t *testing.T) {
	root, m := generate(t, 12)

	data, err := os.ReadFile(filepath.Join(root, "MANIFEST.json"))
	if err != nil {
		t.Fatalf("reading manifest: %v", err)
	}
	var onDisk Manifest
	if err := json.Unmarshal(data, &onDisk); err != nil {
		t.Fatalf("manifest is not valid JSON: %v", err)
	}
	if onDisk.Summary != m.Summary {
		t.Errorf("on-disk summary %+v differs from returned %+v", onDisk.Summary, m.Summary)
	}
	if len(onDisk.Files) != len(m.Files) {
		t.Errorf("on-disk has %d files, returned has %d", len(onDisk.Files), len(m.Files))
	}
}

// Summary counts must match the file list they claim to summarise; a test that
// asserts against a wrong count is worse than no test.
func TestSummaryAgreesWithFileList(t *testing.T) {
	_, m := generate(t, 24)

	var supported, unsupported, corrupt, withGPS, blurry int
	for _, f := range m.Files {
		switch f.Kind {
		case "jpeg", "png":
			supported++
		case "corrupt":
			supported++
			corrupt++
		case "unsupported":
			unsupported++
		}
		if f.Latitude != nil {
			withGPS++
		}
		if f.Blurry {
			blurry++
		}
	}

	if m.Summary.TotalFiles != len(m.Files) {
		t.Errorf("TotalFiles = %d, file list has %d", m.Summary.TotalFiles, len(m.Files))
	}
	if m.Summary.SupportedImages != supported {
		t.Errorf("SupportedImages = %d, counted %d", m.Summary.SupportedImages, supported)
	}
	if m.Summary.UnsupportedFiles != unsupported {
		t.Errorf("UnsupportedFiles = %d, counted %d", m.Summary.UnsupportedFiles, unsupported)
	}
	if m.Summary.CorruptFiles != corrupt {
		t.Errorf("CorruptFiles = %d, counted %d", m.Summary.CorruptFiles, corrupt)
	}
	if m.Summary.WithGPS != withGPS {
		t.Errorf("WithGPS = %d, counted %d", m.Summary.WithGPS, withGPS)
	}
	if m.Summary.IntentionallyBlurry != blurry {
		t.Errorf("IntentionallyBlurry = %d, counted %d", m.Summary.IntentionallyBlurry, blurry)
	}
}

// Recursive scanning needs nesting and an empty directory to walk over.
func TestCorpusHasNestedAndEmptyDirectories(t *testing.T) {
	root, m := generate(t, 24)

	if fi, err := os.Stat(filepath.Join(root, "empty-dir")); err != nil || !fi.IsDir() {
		t.Error("expected an empty-dir to exercise walking a directory with no files")
	}

	maxDepth := 0
	for _, f := range m.Files {
		if d := strings.Count(f.RelPath, "/"); d > maxDepth {
			maxDepth = d
		}
	}
	if maxDepth < 3 {
		t.Errorf("deepest file is %d levels down; want >= 3 to exercise recursion", maxDepth)
	}
}
