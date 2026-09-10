package hashing

import (
	"image"
	"image/color"
	"math/bits"
	"os"
	"path/filepath"
	"testing"

	"github.com/srikarguntaka/photo-organizer/internal/fixtures"
)

func hashFile(t *testing.T, path string) uint64 {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("opening %s: %v", path, err)
	}
	defer f.Close()

	h, err := DHashReader(f)
	if err != nil {
		t.Fatalf("hashing %s: %v", path, err)
	}
	return h
}

// THE Phase 6 assertion: the corpus's planted near-duplicate groups -- an
// original, a recompressed copy and a resized copy -- must hash close together,
// even though SHA-256 correctly sees them as three different files.
func TestNearDuplicatesAreWithinThreshold(t *testing.T) {
	root := t.TempDir()
	m, err := fixtures.Generate(fixtures.Options{Root: root, Seed: 1, Scenes: 24})
	if err != nil {
		t.Fatal(err)
	}

	groups := map[string][]string{}
	for _, f := range m.Files {
		if f.SimilarGroup != "" {
			groups[f.SimilarGroup] = append(groups[f.SimilarGroup], f.RelPath)
		}
	}
	if len(groups) == 0 {
		t.Fatal("corpus contains no near-duplicate groups")
	}

	worst := 0
	for name, paths := range groups {
		if len(paths) < 2 {
			t.Errorf("group %s has %d member(s)", name, len(paths))
			continue
		}

		base := hashFile(t, filepath.Join(root, filepath.FromSlash(paths[0])))
		for _, p := range paths[1:] {
			h := hashFile(t, filepath.Join(root, filepath.FromSlash(p)))
			d := HammingDistance(base, h)
			if d > worst {
				worst = d
			}
			if d > DefaultSimilarityThreshold {
				t.Errorf("%s vs %s: distance %d exceeds the default threshold %d -- "+
					"the manifest says these are the same image recompressed or resized",
					paths[0], p, d, DefaultSimilarityThreshold)
			}
			t.Logf("%-34s vs %-34s  distance %2d", filepath.Base(paths[0]), filepath.Base(p), d)
		}
	}
	t.Logf("worst near-duplicate distance across %d groups: %d (threshold %d)",
		len(groups), worst, DefaultSimilarityThreshold)
}

// The other half of the guarantee: unrelated photos must NOT be within the
// threshold, or every photo lands in one giant group.
func TestUnrelatedPhotosExceedThreshold(t *testing.T) {
	root := t.TempDir()
	m, err := fixtures.Generate(fixtures.Options{Root: root, Seed: 1, Scenes: 24})
	if err != nil {
		t.Fatal(err)
	}

	// One representative per distinct scene, skipping anything the manifest
	// marks as a duplicate or near-duplicate of something else.
	var reps []string
	seen := map[string]bool{}
	for _, f := range m.Files {
		if f.Kind != "jpeg" || f.CorruptStage != "" {
			continue
		}
		if f.DuplicateGroup != "" || f.SimilarGroup != "" {
			continue
		}
		base := filepath.Base(f.RelPath)
		if seen[base] {
			continue
		}
		seen[base] = true
		reps = append(reps, f.RelPath)
	}
	if len(reps) < 4 {
		t.Fatalf("only %d distinct unrelated photos in the corpus; not enough to test", len(reps))
	}

	hashes := make([]uint64, len(reps))
	for i, p := range reps {
		hashes[i] = hashFile(t, filepath.Join(root, filepath.FromSlash(p)))
	}

	collisions, pairs, sum := 0, 0, 0
	for i := range hashes {
		for j := i + 1; j < len(hashes); j++ {
			d := HammingDistance(hashes[i], hashes[j])
			pairs++
			sum += d
			if d <= DefaultSimilarityThreshold {
				collisions++
				t.Errorf("unrelated photos %s and %s are only %d apart (threshold %d); "+
					"they would be grouped as near-duplicates",
					filepath.Base(reps[i]), filepath.Base(reps[j]), d, DefaultSimilarityThreshold)
			}
		}
	}
	t.Logf("%d unrelated pairs, mean distance %.1f, %d false positives",
		pairs, float64(sum)/float64(pairs), collisions)
}

// The property that makes dHash the right choice: a uniform brightness shift
// changes every pixel but no gradient, so the hash should barely move. aHash
// would flip many bits here.
func TestBrightnessShiftBarelyMovesTheHash(t *testing.T) {
	base := gradientImage(200, 150, 0)
	brighter := gradientImage(200, 150, 40)

	d := HammingDistance(DHash(base), DHash(brighter))
	if d > 2 {
		t.Errorf("a uniform +40 brightness shift moved the hash by %d bits; dHash compares "+
			"neighbouring pixels precisely so that absolute level does not matter", d)
	}
	t.Logf("uniform brightness shift moved the hash by %d bits", d)
}

// Resizing must preserve the hash, since "the same photo at a different size"
// is the primary case this feature exists to catch.
func TestResizePreservesTheHash(t *testing.T) {
	full := gradientImage(800, 600, 0)
	half := gradientImage(400, 300, 0)
	small := gradientImage(200, 150, 0)

	if d := HammingDistance(DHash(full), DHash(half)); d > 4 {
		t.Errorf("halving the dimensions moved the hash by %d bits, want <= 4", d)
	}
	if d := HammingDistance(DHash(full), DHash(small)); d > 6 {
		t.Errorf("quartering the dimensions moved the hash by %d bits, want <= 6", d)
	}
}

// Structurally different images must be far apart.
func TestDifferentImagesAreFarApart(t *testing.T) {
	a := gradientImage(200, 150, 0)
	b := checkerImage(200, 150)

	if d := HammingDistance(DHash(a), DHash(b)); d < 10 {
		t.Errorf("a gradient and a checkerboard are only %d bits apart; the hash is not "+
			"discriminating between structurally different images", d)
	}
}

func TestHashIsDeterministic(t *testing.T) {
	img := gradientImage(300, 200, 0)
	first := DHash(img)
	for i := 0; i < 10; i++ {
		if got := DHash(img); got != first {
			t.Fatalf("DHash is not deterministic: %016x then %016x", first, got)
		}
	}
}

func TestHammingDistance(t *testing.T) {
	cases := []struct {
		a, b uint64
		want int
	}{
		{0, 0, 0},
		{0, 1, 1},
		{0, 0b1011, 3},
		{^uint64(0), 0, 64},
		{0xFFFFFFFF00000000, 0x00000000FFFFFFFF, 64},
		{0xAAAAAAAAAAAAAAAA, 0x5555555555555555, 64},
	}
	for _, c := range cases {
		if got := HammingDistance(c.a, c.b); got != c.want {
			t.Errorf("HammingDistance(%016x, %016x) = %d, want %d", c.a, c.b, got, c.want)
		}
	}

	// Symmetry: distance is a metric, and the grouping code relies on it.
	for i := 0; i < 100; i++ {
		a, b := uint64(i*2654435761), uint64(i*40503)
		if HammingDistance(a, b) != HammingDistance(b, a) {
			t.Fatalf("HammingDistance is not symmetric for %016x, %016x", a, b)
		}
	}
}

// The hash must use its full width. A hash that only ever sets a few bit
// positions would collide constantly.
//
// Measured on PHOTO-LIKE images, not synthetic noise. The first version of this
// test used blocky noise and reported 56 of 64 positions -- and that figure was
// identical at 40, 100, 400 and 1000 samples, so it was a structural property
// of the noise generator rather than undersampling. Its 16px blocks alias
// against the 9x8 cell grid, the same class of artifact that made the original
// fixture images unhashable. Photo-like input reaches 64/64 at every sample
// count.
func TestHashUsesFullWidth(t *testing.T) {
	if DHashBits != 64 {
		t.Fatalf("DHashBits = %d, want 64 -- the schema stores this in a bigint", DHashBits)
	}

	var union uint64
	for i := 1; i <= 40; i++ {
		union |= DHash(calibPhotoStyle(int64(i), 640, 480))
	}
	if n := bits.OnesCount64(union); n != 64 {
		t.Errorf("across 40 photo-like images only %d of 64 bit positions were ever set; "+
			"the hash is not using its full width", n)
	}
}

// Real corpus images must produce well-distributed hashes, not all-zero or
// all-one degenerate values.
func TestCorpusHashesAreNotDegenerate(t *testing.T) {
	root := t.TempDir()
	m, err := fixtures.Generate(fixtures.Options{Root: root, Seed: 1, Scenes: 24})
	if err != nil {
		t.Fatal(err)
	}

	checked := 0
	for _, f := range m.Files {
		if f.Kind != "jpeg" || f.CorruptStage != "" {
			continue
		}
		h := hashFile(t, filepath.Join(root, filepath.FromSlash(f.RelPath)))
		ones := bits.OnesCount64(h)
		if ones == 0 || ones == 64 {
			t.Errorf("%s hashed to a degenerate all-%d value", f.RelPath, ones/64)
		}
		checked++
	}
	if checked == 0 {
		t.Fatal("no images checked")
	}
	t.Logf("checked %d corpus images for degenerate hashes", checked)
}

// A corrupt file must fail rather than silently returning a zero hash, which
// would make every corrupt file a "near-duplicate" of every other.
func TestCorruptImageFailsRatherThanHashingToZero(t *testing.T) {
	root := t.TempDir()
	m, err := fixtures.Generate(fixtures.Options{Root: root, Seed: 1, Scenes: 24})
	if err != nil {
		t.Fatal(err)
	}

	checked := 0
	for _, f := range m.Files {
		if f.CorruptStage == "" {
			continue
		}
		fh, err := os.Open(filepath.Join(root, filepath.FromSlash(f.RelPath)))
		if err != nil {
			t.Fatal(err)
		}
		_, herr := DHashReader(fh)
		fh.Close()

		if herr == nil {
			t.Errorf("%s (corrupt at the %s stage) produced a hash instead of an error; "+
				"corrupt files hashing to a shared value would group them all together",
				f.RelPath, f.CorruptStage)
		}
		checked++
	}
	if checked == 0 {
		t.Fatal("corpus has no corrupt files")
	}
}

func TestSimilarHelper(t *testing.T) {
	a := uint64(0b1111)
	b := uint64(0b1010) // 2 bits differ

	if !Similar(a, b, 2) {
		t.Error("Similar should be inclusive at exactly the threshold")
	}
	if Similar(a, b, 1) {
		t.Error("Similar returned true beyond the threshold")
	}
}

func TestDHashString(t *testing.T) {
	if got := DHashString(0); got != "0000000000000000" {
		t.Errorf("DHashString(0) = %q, want 16 zeroes", got)
	}
	if got := DHashString(^uint64(0)); got != "ffffffffffffffff" {
		t.Errorf("DHashString(max) = %q", got)
	}
}

// --- test image builders --------------------------------------------------

// gradientImage builds a smooth diagonal gradient with an optional uniform
// brightness offset.
func gradientImage(w, h, offset int) image.Image {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	clamp := func(v int) uint8 {
		if v < 0 {
			return 0
		}
		if v > 255 {
			return 255
		}
		return uint8(v)
	}
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			v := (x*255/w + y*255/h) / 2
			img.SetRGBA(x, y, color.RGBA{clamp(v + offset), clamp(v + offset), clamp(v + offset), 255})
		}
	}
	return img
}

func checkerImage(w, h int) image.Image {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	cell := w / 8
	if cell < 1 {
		cell = 1
	}
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			if (x/cell+y/cell)%2 == 0 {
				img.SetRGBA(x, y, color.RGBA{255, 255, 255, 255})
			} else {
				img.SetRGBA(x, y, color.RGBA{0, 0, 0, 255})
			}
		}
	}
	return img
}
