// Package fixtures generates a synthetic photo library with known-by-
// construction properties.
//
// Why this exists: correctness tests need ground truth. Pointed at a real photo
// dump, "duplicate detection found 12 groups" is unfalsifiable -- nobody knows
// how many duplicate groups that folder actually contains. Here, the generator
// decides how many duplicates exist and writes that fact to a manifest, so a
// test can assert an exact number and fail loudly when the algorithm drifts.
//
// Real photo libraries are still the better *validation* input, and the
// benchmarks in Phase 11 use one. They just cannot verify.
package fixtures

import (
	"bytes"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// Manifest is the ground truth for a generated corpus. Tests read this instead
// of hardcoding expectations, so regenerating with a different seed or size
// does not require editing assertions.
type Manifest struct {
	Seed        int64    `json:"seed"`
	GeneratedAt string   `json:"generated_at"`
	Root        string   `json:"root"`
	Files       []File   `json:"files"`
	Summary     Summary  `json:"summary"`
	Notes       []string `json:"notes"`
}

// Summary is the set of counts a test most often wants to assert against.
type Summary struct {
	TotalFiles int `json:"total_files"`
	// Supported images the scanner is expected to ingest.
	SupportedImages int `json:"supported_images"`
	// Files the scanner must skip without erroring.
	UnsupportedFiles int `json:"unsupported_files"`
	// Number of *groups* of byte-identical files (each group has >= 2 members).
	ExactDuplicateGroups int `json:"exact_duplicate_groups"`
	// Total files that are a byte-identical copy of some other file.
	ExactDuplicateFiles int `json:"exact_duplicate_files"`
	// Number of groups of visually-similar-but-not-identical files.
	NearDuplicateGroups int `json:"near_duplicate_groups"`
	WithGPS             int `json:"with_gps"`
	WithoutGPS          int `json:"without_gps"`
	WithoutEXIF         int `json:"without_exif"`
	CorruptFiles        int `json:"corrupt_files"`
	IntentionallyBlurry int `json:"intentionally_blurry"`
	Underexposed        int `json:"underexposed"`
	Overexposed         int `json:"overexposed"`
}

// File is one generated file and everything true about it.
type File struct {
	// Path relative to the corpus root, forward slashes.
	RelPath string `json:"rel_path"`
	Kind    string `json:"kind"` // jpeg | png | unsupported | corrupt
	Bytes   int    `json:"bytes"`

	// Ground truth for duplicate detection. Files sharing a DuplicateGroup are
	// byte-identical. Files sharing a SimilarGroup are visually similar but
	// have different bytes.
	DuplicateGroup string `json:"duplicate_group,omitempty"`
	SimilarGroup   string `json:"similar_group,omitempty"`

	// CorruptStage records WHERE a damaged file fails, because "corrupt" is
	// stage-dependent and the pipeline treats the two cases differently:
	//
	//   "header" -> image.DecodeConfig fails; no metadata is recoverable
	//   "pixels" -> header and EXIF are intact and metadata extracts fine,
	//               but a full pixel decode fails (a half-written file)
	//
	// Phase 3 reads headers only, so a "pixels" file succeeds there and fails
	// later in the phases that touch actual pixels.
	CorruptStage string `json:"corrupt_stage,omitempty"`

	// Ground truth for quality analysis.
	Blurry       bool `json:"blurry,omitempty"`
	Underexposed bool `json:"underexposed,omitempty"`
	Overexposed  bool `json:"overexposed,omitempty"`

	// Ground truth for metadata extraction. Nil means the file genuinely has
	// no such metadata -- which the pipeline must handle, not treat as an error.
	CapturedAt *string  `json:"captured_at,omitempty"`
	Latitude   *float64 `json:"latitude,omitempty"`
	Longitude  *float64 `json:"longitude,omitempty"`

	Width  int `json:"width,omitempty"`
	Height int `json:"height,omitempty"`
}

// Options controls corpus size and shape.
type Options struct {
	// Root directory to write into. Created if absent.
	Root string
	// Seed makes generation deterministic. The same seed produces byte-identical
	// output, which is what lets a test compare hashes across runs.
	Seed int64
	// Scenes is the number of distinct base images. Total file count is larger
	// because of duplicates and variants.
	Scenes int
}

// Generate writes the corpus and returns its manifest. Any existing content at
// Root is left alone; callers wanting a clean corpus should remove it first.
func Generate(opts Options) (*Manifest, error) {
	if opts.Scenes <= 0 {
		opts.Scenes = 24
	}
	if opts.Root == "" {
		return nil, fmt.Errorf("fixtures: Root is required")
	}

	m := &Manifest{
		Seed:        opts.Seed,
		GeneratedAt: time.Now().UTC().Format(time.RFC3339),
		Root:        opts.Root,
		Notes: []string{
			"Generated corpus. Every property here is true by construction, not by inspection.",
			"EXIF is written by hand as an APP1 segment; only DateTimeOriginal and GPS are populated.",
			"Sub-directories exist to exercise recursive scanning, including one that is empty.",
		},
	}

	// A spread of capture times across several days, deliberately including
	// tight bursts (seconds apart) and large gaps (days), so clustering has
	// something real to cut on.
	base := time.Date(2026, 3, 14, 9, 0, 0, 0, time.UTC)

	// Austin and Dallas, far enough apart that Haversine must separate them.
	austin := [2]float64{30.2672, -97.7431}
	dallas := [2]float64{32.7767, -96.7970}

	dirs := []string{"", "trip", "trip/day1", "trip/day2", "screenshots", "misc/nested/deep"}
	for _, d := range dirs {
		if err := os.MkdirAll(filepath.Join(opts.Root, filepath.FromSlash(d)), 0o755); err != nil {
			return nil, fmt.Errorf("fixtures: creating %s: %w", d, err)
		}
	}
	// An empty directory must not break the walk.
	if err := os.MkdirAll(filepath.Join(opts.Root, "empty-dir"), 0o755); err != nil {
		return nil, err
	}

	write := func(relPath string, data []byte, f File) error {
		full := filepath.Join(opts.Root, filepath.FromSlash(relPath))
		if err := os.WriteFile(full, data, 0o644); err != nil {
			return fmt.Errorf("fixtures: writing %s: %w", relPath, err)
		}
		f.RelPath = relPath
		f.Bytes = len(data)
		m.Files = append(m.Files, f)
		return nil
	}

	for i := 0; i < opts.Scenes; i++ {
		dir := dirs[i%len(dirs)]
		sceneID := fmt.Sprintf("scene%02d", i)

		// Bursts: every fourth scene sits seconds after the previous one, the
		// rest are hours or days apart.
		var shotAt time.Time
		switch {
		case i%4 == 0 && i > 0:
			shotAt = base.Add(time.Duration(i) * 3 * time.Second)
		case i%7 == 0:
			shotAt = base.Add(time.Duration(i) * 30 * time.Hour)
		default:
			shotAt = base.Add(time.Duration(i) * 47 * time.Minute)
		}

		// Later scenes move to Dallas so clustering has a geographic boundary.
		loc := austin
		if i > opts.Scenes*2/3 {
			loc = dallas
		}

		img := scene(opts.Seed, sceneID, 640, 480)

		// --- variant selection -------------------------------------------
		// Each scene gets a role, cycling deterministically so the corpus has a
		// predictable mix rather than a random one that might omit a case.
		switch i % 8 {

		case 0: // plain photo, full EXIF with GPS
			b, err := encodeJPEGWithEXIF(img, 88, &shotAt, &loc)
			if err != nil {
				return nil, err
			}
			t := shotAt.Format(time.RFC3339)
			if err := write(join(dir, sceneID+".jpg"), b, File{
				Kind: "jpeg", CapturedAt: &t,
				Latitude: fptr(loc[0]), Longitude: fptr(loc[1]),
				Width: 640, Height: 480,
			}); err != nil {
				return nil, err
			}

		case 1: // exact duplicate pair -- byte-identical copies
			b, err := encodeJPEGWithEXIF(img, 88, &shotAt, &loc)
			if err != nil {
				return nil, err
			}
			t := shotAt.Format(time.RFC3339)
			group := "dup-" + sceneID
			for n, name := range []string{sceneID + ".jpg", sceneID + "-copy.jpg", sceneID + " (1).jpg"} {
				// Third copy lands in a different directory: duplicate
				// detection must be path-independent.
				d := dir
				if n == 2 {
					d = "misc/nested/deep"
				}
				if err := write(join(d, name), b, File{
					Kind: "jpeg", DuplicateGroup: group, CapturedAt: &t,
					Latitude: fptr(loc[0]), Longitude: fptr(loc[1]),
					Width: 640, Height: 480,
				}); err != nil {
					return nil, err
				}
			}

		case 2: // near-duplicates: same scene, re-encoded and resized
			group := "sim-" + sceneID
			t := shotAt.Format(time.RFC3339)

			orig, err := encodeJPEGWithEXIF(img, 92, &shotAt, &loc)
			if err != nil {
				return nil, err
			}
			if err := write(join(dir, sceneID+".jpg"), orig, File{
				Kind: "jpeg", SimilarGroup: group, CapturedAt: &t,
				Latitude: fptr(loc[0]), Longitude: fptr(loc[1]),
				Width: 640, Height: 480,
			}); err != nil {
				return nil, err
			}

			// Recompressed at low quality -- different bytes, near-identical
			// appearance. This is the case SHA-256 must miss and dHash catch.
			recomp, err := encodeJPEGWithEXIF(img, 40, &shotAt, &loc)
			if err != nil {
				return nil, err
			}
			if err := write(join(dir, sceneID+"-recompressed.jpg"), recomp, File{
				Kind: "jpeg", SimilarGroup: group, CapturedAt: &t,
				Latitude: fptr(loc[0]), Longitude: fptr(loc[1]),
				Width: 640, Height: 480,
			}); err != nil {
				return nil, err
			}

			// Resized to half dimensions.
			small := resizeNearest(img, 320, 240)
			sb, err := encodeJPEGWithEXIF(small, 85, &shotAt, &loc)
			if err != nil {
				return nil, err
			}
			if err := write(join(dir, sceneID+"-small.jpg"), sb, File{
				Kind: "jpeg", SimilarGroup: group, CapturedAt: &t,
				Latitude: fptr(loc[0]), Longitude: fptr(loc[1]),
				Width: 320, Height: 240,
			}); err != nil {
				return nil, err
			}

		case 3: // blurry
			blurred := boxBlur(img, 6)
			b, err := encodeJPEGWithEXIF(blurred, 88, &shotAt, &loc)
			if err != nil {
				return nil, err
			}
			t := shotAt.Format(time.RFC3339)
			if err := write(join(dir, sceneID+"-blurry.jpg"), b, File{
				Kind: "jpeg", Blurry: true, CapturedAt: &t,
				Latitude: fptr(loc[0]), Longitude: fptr(loc[1]),
				Width: 640, Height: 480,
			}); err != nil {
				return nil, err
			}

		case 4: // exposure extremes
			t := shotAt.Format(time.RFC3339)

			// These two are the SAME scene at different exposures, so they are
			// genuinely near-duplicates and are labelled as such.
			//
			// The label was missing at first, and a perceptual-hash test duly
			// reported them as false positives at distance 0-3. They were not:
			// dHash compares neighbouring pixels, so a uniform brightness shift
			// leaves every comparison unchanged. The ground truth was wrong,
			// not the algorithm.
			//
			// This pair is also where two features meet. Near-duplicate
			// detection says "these are the same photo"; quality analysis
			// (Phase 7) says "this one is correctly exposed and that one is
			// not". Together that is the suggest-the-best-copy feature, and
			// neither half can do it alone.
			exposureGroup := "sim-exposure-" + sceneID

			dark := adjustBrightness(img, -95)
			db, err := encodeJPEGWithEXIF(dark, 88, &shotAt, nil)
			if err != nil {
				return nil, err
			}
			if err := write(join(dir, sceneID+"-dark.jpg"), db, File{
				Kind: "jpeg", Underexposed: true, SimilarGroup: exposureGroup, CapturedAt: &t,
				Width: 640, Height: 480,
			}); err != nil {
				return nil, err
			}

			bright := adjustBrightness(img, 95)
			bb, err := encodeJPEGWithEXIF(bright, 88, &shotAt, nil)
			if err != nil {
				return nil, err
			}
			if err := write(join(dir, sceneID+"-bright.jpg"), bb, File{
				Kind: "jpeg", Overexposed: true, SimilarGroup: exposureGroup, CapturedAt: &t,
				Width: 640, Height: 480,
			}); err != nil {
				return nil, err
			}

		case 5: // no EXIF at all -- must fall back to filesystem mtime
			var buf bytes.Buffer
			if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 85}); err != nil {
				return nil, err
			}
			if err := write(join(dir, sceneID+"-noexif.jpg"), buf.Bytes(), File{
				Kind: "jpeg", Width: 640, Height: 480,
			}); err != nil {
				return nil, err
			}

		case 6: // PNG, no EXIF by nature
			var buf bytes.Buffer
			if err := png.Encode(&buf, img); err != nil {
				return nil, err
			}
			if err := write(join(dir, sceneID+".png"), buf.Bytes(), File{
				Kind: "png", Width: 640, Height: 480,
			}); err != nil {
				return nil, err
			}

		case 7: // EXIF timestamp but no GPS -- the common real-world case
			b, err := encodeJPEGWithEXIF(img, 88, &shotAt, nil)
			if err != nil {
				return nil, err
			}
			t := shotAt.Format(time.RFC3339)
			if err := write(join(dir, sceneID+"-nogps.jpg"), b, File{
				Kind: "jpeg", CapturedAt: &t, Width: 640, Height: 480,
			}); err != nil {
				return nil, err
			}
		}
	}

	// --- files that must be skipped or must fail gracefully ---------------

	// Unsupported extensions: ignored silently, never an error.
	for _, f := range []struct {
		path string
		data string
	}{
		{"notes.txt", "not an image"},
		{"misc/archive.zip", "PK\x03\x04not really a zip"},
		{"trip/clip.mp4", "video is out of scope for v1"},
		{"screenshots/.DS_Store", "macOS metadata"},
	} {
		if err := write(f.path, []byte(f.data), File{Kind: "unsupported"}); err != nil {
			return nil, err
		}
	}

	// A file with a .jpg extension that is not a JPEG. Extension-based format
	// detection will accept it; decoding must fail cleanly and mark the photo
	// failed rather than crashing a worker.
	if err := write("misc/truncated.jpg", []byte("\xff\xd8\xff\xe0 this is not a real jpeg"), File{
		Kind: "corrupt", CorruptStage: "header",
	}); err != nil {
		return nil, err
	}

	// A real JPEG truncated mid-stream -- decodes partially then errors, which
	// is a different failure path from "not a JPEG at all".
	{
		full, err := encodeJPEGWithEXIF(scene(opts.Seed, "truncated", 640, 480), 90, nil, nil)
		if err != nil {
			return nil, err
		}
		// Header and EXIF survive; the entropy-coded pixel data does not. This
		// is the realistic "interrupted copy" case, and the one that proves
		// metadata extraction does not need intact pixels.
		if err := write("misc/half-written.jpg", full[:len(full)/2], File{
			Kind: "corrupt", CorruptStage: "pixels",
		}); err != nil {
			return nil, err
		}
	}

	// Zero-byte file with an image extension.
	if err := write("misc/empty.jpg", nil, File{Kind: "corrupt", CorruptStage: "header"}); err != nil {
		return nil, err
	}

	sort.Slice(m.Files, func(i, j int) bool { return m.Files[i].RelPath < m.Files[j].RelPath })
	m.Summary = summarise(m.Files)

	// Write the manifest outside the corpus root would be cleaner, but keeping
	// it inside makes the corpus self-describing. It has an unsupported
	// extension, so the scanner ignores it.
	mb, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(opts.Root, "MANIFEST.json"), mb, 0o644); err != nil {
		return nil, fmt.Errorf("fixtures: writing manifest: %w", err)
	}

	return m, nil
}

func summarise(files []File) Summary {
	s := Summary{}
	dupGroups := map[string]int{}
	simGroups := map[string]int{}

	for _, f := range files {
		s.TotalFiles++
		switch f.Kind {
		case "jpeg", "png":
			s.SupportedImages++
		case "unsupported":
			s.UnsupportedFiles++
		case "corrupt":
			// Corrupt files still have image extensions, so the scanner
			// discovers them; they fail later, during decode.
			s.SupportedImages++
			s.CorruptFiles++
		}
		if f.DuplicateGroup != "" {
			dupGroups[f.DuplicateGroup]++
		}
		if f.SimilarGroup != "" {
			simGroups[f.SimilarGroup]++
		}
		if f.Latitude != nil {
			s.WithGPS++
		} else if f.Kind == "jpeg" || f.Kind == "png" {
			s.WithoutGPS++
		}
		if f.CapturedAt == nil && (f.Kind == "jpeg" || f.Kind == "png") {
			s.WithoutEXIF++
		}
		if f.Blurry {
			s.IntentionallyBlurry++
		}
		if f.Underexposed {
			s.Underexposed++
		}
		if f.Overexposed {
			s.Overexposed++
		}
	}

	for _, n := range dupGroups {
		if n >= 2 {
			s.ExactDuplicateGroups++
			s.ExactDuplicateFiles += n
		}
	}
	for _, n := range simGroups {
		if n >= 2 {
			s.NearDuplicateGroups++
		}
	}
	return s
}

// --- image construction ---------------------------------------------------

// scene builds a deterministic, photograph-like image.
//
// The structure here is not decorative -- it was chosen from measurement, and
// the first version of it broke perceptual hashing outright.
//
// That version drew hard-edged rectangles plus a 6px diagonal stripe every
// 64px. Measured against dHash:
//
//	hard edges + fine stripes:  near-dupe mean 18.0 max 30 | unrelated mean 20.6 min  8
//	smooth low-frequency:       near-dupe mean  1.3 max  4 | unrelated mean 32.2 min 13
//
// The distributions OVERLAPPED. No threshold could separate "the same photo
// recompressed" from "two unrelated photos", because a perceptual hash
// downsamples to 9x8 and a periodic 6px pattern aliases catastrophically at
// that scale -- a one-pixel shift from resizing moves stripes between cells and
// scrambles the comparisons. Real photographs have no such pattern.
//
// So scenes are now built in two layers, and both are load-bearing:
//
//  1. LOW FREQUENCY: a few superimposed sinusoids. This is the large smooth
//     luminance structure that survives downsampling, and it is what a
//     perceptual hash actually sees. Without it, dHash has no stable signal.
//
//  2. FINE TEXTURE: irregular, non-periodic, low-amplitude detail. This gives
//     sharpness metrics (Phase 7) something real to measure -- a purely smooth
//     image has near-zero Laplacian variance and would register as blurry no
//     matter what. It is deliberately non-periodic so it averages out under
//     downsampling instead of aliasing.
//
// Content is derived from baseSeed and the scene id together: the id keeps
// scenes distinct within one corpus, and baseSeed keeps two corpora generated
// with different seeds from sharing content.
func scene(baseSeed int64, id string, w, h int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))

	var idHash int64
	for _, c := range id {
		idHash = idHash*31 + int64(c)
	}
	// Multiply the seed by a large prime before mixing so adjacent seeds
	// produce unrelated corpora rather than near-identical ones.
	local := rand.New(rand.NewSource(baseSeed*1000003 + idHash))

	// --- layer 1: low-frequency luminance structure ------------------------
	type wave struct{ ax, ay, phase, amp float64 }
	waves := make([]wave, 4)
	for i := range waves {
		waves[i] = wave{
			// Low spatial frequencies only. Anything above ~3 cycles across
			// the frame starts to alias at the 9x8 hash resolution.
			ax:    (local.Float64()*2 - 1) * 2.5,
			ay:    (local.Float64()*2 - 1) * 2.5,
			phase: local.Float64() * 2 * math.Pi,
			amp:   0.3 + local.Float64()*0.7,
		}
	}

	baseR := local.Float64()*80 + 70
	baseG := local.Float64()*80 + 70
	baseB := local.Float64()*80 + 70

	// --- layer 2: fine texture --------------------------------------------
	// A handful of irregular high-frequency components with incommensurable
	// frequencies, so they never form a repeating pattern that could align
	// with the downsample grid.
	type detail struct{ ax, ay, phase, amp float64 }
	details := make([]detail, 6)
	for i := range details {
		details[i] = detail{
			ax:    18 + local.Float64()*37,
			ay:    18 + local.Float64()*37,
			phase: local.Float64() * 2 * math.Pi,
			amp:   6 + local.Float64()*10,
		}
	}

	clamp := func(f float64) uint8 {
		if f < 0 {
			return 0
		}
		if f > 255 {
			return 255
		}
		return uint8(f)
	}

	// Evaluate the sinusoids with the angle-addition identity rather than
	// calling sin() per pixel:
	//
	//	sin(A + B) = sin(A)cos(B) + cos(A)sin(B)
	//
	// where A depends only on x and B only on y. That turns 10 waves x 307,200
	// pixels = ~3M transcendental calls into 10 x (640 + 480) = ~11,000, and
	// the rest is multiply-add. The naive version made the fixture tests take
	// 50 seconds; this is a few hundred milliseconds for identical output.
	type axis struct{ sin, cos float64 }

	prep := func(n int, coeff, phase float64) []axis {
		out := make([]axis, n)
		for i := 0; i < n; i++ {
			a := coeff * (float64(i) / float64(n)) * 2 * math.Pi
			out[i] = axis{math.Sin(a + phase), math.Cos(a + phase)}
		}
		return out
	}

	// Phase is folded into the x term so the y term stays a pure sinusoid.
	lowX := make([][]axis, len(waves))
	lowY := make([][]axis, len(waves))
	for i, wv := range waves {
		lowX[i] = prep(w, wv.ax, wv.phase)
		lowY[i] = prep(h, wv.ay, 0)
	}

	fineX := make([][]axis, len(details))
	fineY := make([][]axis, len(details))
	for i, d := range details {
		fineX[i] = prep(w, d.ax, d.phase)
		fineY[i] = prep(h, d.ay, 0)
	}

	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			low := 0.0
			for i, wv := range waves {
				sx, cx := lowX[i][x].sin, lowX[i][x].cos
				sy, cy := lowY[i][y].sin, lowY[i][y].cos
				low += wv.amp * (sx*cy + cx*sy)
			}
			low = low / 4.0 * 90

			fine := 0.0
			for i, d := range details {
				sx, cx := fineX[i][x].sin, fineX[i][x].cos
				sy, cy := fineY[i][y].sin, fineY[i][y].cos
				fine += d.amp * (sx*cy + cx*sy)
			}
			fine /= 6.0

			img.SetRGBA(x, y, color.RGBA{
				clamp(baseR + low + fine),
				clamp(baseG + low*0.8 + fine),
				clamp(baseB + low*1.2 + fine),
				255,
			})
		}
	}

	return img
}

func resizeNearest(src *image.RGBA, w, h int) *image.RGBA {
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	sb := src.Bounds()
	for y := 0; y < h; y++ {
		sy := sb.Min.Y + y*sb.Dy()/h
		for x := 0; x < w; x++ {
			sx := sb.Min.X + x*sb.Dx()/w
			dst.SetRGBA(x, y, src.RGBAAt(sx, sy))
		}
	}
	return dst
}

// boxBlur applies a separable box blur. Crude, but it genuinely destroys
// high-frequency detail, which is what a sharpness metric must detect.
func boxBlur(src *image.RGBA, radius int) *image.RGBA {
	if radius < 1 {
		return src
	}
	b := src.Bounds()
	tmp := image.NewRGBA(b)
	dst := image.NewRGBA(b)

	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			var r, g, bl, n int
			for dx := -radius; dx <= radius; dx++ {
				sx := x + dx
				if sx < b.Min.X || sx >= b.Max.X {
					continue
				}
				c := src.RGBAAt(sx, y)
				r += int(c.R)
				g += int(c.G)
				bl += int(c.B)
				n++
			}
			tmp.SetRGBA(x, y, color.RGBA{uint8(r / n), uint8(g / n), uint8(bl / n), 255})
		}
	}
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			var r, g, bl, n int
			for dy := -radius; dy <= radius; dy++ {
				sy := y + dy
				if sy < b.Min.Y || sy >= b.Max.Y {
					continue
				}
				c := tmp.RGBAAt(x, sy)
				r += int(c.R)
				g += int(c.G)
				bl += int(c.B)
				n++
			}
			dst.SetRGBA(x, y, color.RGBA{uint8(r / n), uint8(g / n), uint8(bl / n), 255})
		}
	}
	return dst
}

func adjustBrightness(src *image.RGBA, delta int) *image.RGBA {
	b := src.Bounds()
	dst := image.NewRGBA(b)
	clamp := func(v int) uint8 {
		if v < 0 {
			return 0
		}
		if v > 255 {
			return 255
		}
		return uint8(v)
	}
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			c := src.RGBAAt(x, y)
			dst.SetRGBA(x, y, color.RGBA{
				clamp(int(c.R) + delta),
				clamp(int(c.G) + delta),
				clamp(int(c.B) + delta),
				255,
			})
		}
	}
	return dst
}

func fptr(f float64) *float64 { return &f }

func join(dir, name string) string {
	if dir == "" {
		return name
	}
	return dir + "/" + name
}

// --- EXIF -----------------------------------------------------------------

// encodeJPEGWithEXIF encodes img and splices a hand-built EXIF APP1 segment in
// after SOI. Writing EXIF by hand rather than pulling in a library keeps the
// fixture generator dependency-free, and more importantly means the test data
// does not share a code path with the extractor under test -- a bug in a shared
// library would otherwise cancel itself out and the test would pass wrongly.
//
// Passing nil for shotAt or loc omits that data entirely, which is how the
// "no EXIF timestamp" and "no GPS" cases are produced.
func encodeJPEGWithEXIF(img image.Image, quality int, shotAt *time.Time, loc *[2]float64) ([]byte, error) {
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: quality}); err != nil {
		return nil, fmt.Errorf("fixtures: encoding jpeg: %w", err)
	}
	raw := buf.Bytes()

	if shotAt == nil && loc == nil {
		return raw, nil
	}

	app1 := buildEXIF(shotAt, loc)

	// SOI is the first two bytes; the APP1 segment goes immediately after.
	out := make([]byte, 0, len(raw)+len(app1))
	out = append(out, raw[:2]...)
	out = append(out, app1...)
	out = append(out, raw[2:]...)
	return out, nil
}

// buildEXIF constructs a minimal little-endian TIFF structure inside an APP1
// segment: IFD0 with an ExifIFD pointer and optionally a GPS IFD pointer.
func buildEXIF(shotAt *time.Time, loc *[2]float64) []byte {
	var tiff bytes.Buffer

	// TIFF header: little-endian, magic 42, IFD0 at offset 8.
	tiff.Write([]byte{'I', 'I', 42, 0})
	tiff.Write(u32(8))

	// Values longer than 4 bytes live in a pool after the IFDs. Offsets are
	// relative to the start of the TIFF header, so the pool base must account
	// for every IFD written before it.
	type entry struct {
		tag, typ uint16
		count    uint32
		value    []byte // inline (<=4 bytes) or nil when using an offset
		pool     []byte // data to place in the pool
	}

	var ifd0 []entry
	var exifIFD []entry
	var gpsIFD []entry

	if shotAt != nil {
		// EXIF datetime format is "YYYY:MM:DD HH:MM:SS\0" -- 20 bytes.
		ts := shotAt.Format("2006:01:02 15:04:05") + "\x00"
		exifIFD = append(exifIFD,
			entry{tag: 0x9003, typ: 2, count: uint32(len(ts)), pool: []byte(ts)}, // DateTimeOriginal
			entry{tag: 0x9004, typ: 2, count: uint32(len(ts)), pool: []byte(ts)}, // DateTimeDigitized
		)
	}

	if loc != nil {
		latRef, lonRef := "N\x00", "E\x00"
		lat, lon := loc[0], loc[1]
		if lat < 0 {
			latRef, lat = "S\x00", -lat
		}
		if lon < 0 {
			lonRef, lon = "W\x00", -lon
		}
		gpsIFD = append(gpsIFD,
			entry{tag: 0x0001, typ: 2, count: 2, value: pad4([]byte(latRef))},
			entry{tag: 0x0002, typ: 5, count: 3, pool: dms(lat)},
			entry{tag: 0x0003, typ: 2, count: 2, value: pad4([]byte(lonRef))},
			entry{tag: 0x0004, typ: 5, count: 3, pool: dms(lon)},
		)
	}

	// IFD0 carries a Make/Model so the file looks like a real camera capture,
	// plus pointers to the sub-IFDs.
	make0 := "photo-organizer\x00"
	model0 := "fixture-generator\x00"
	ifd0 = append(ifd0,
		entry{tag: 0x010F, typ: 2, count: uint32(len(make0)), pool: []byte(make0)},
		entry{tag: 0x0110, typ: 2, count: uint32(len(model0)), pool: []byte(model0)},
	)

	// Layout: IFD0, then ExifIFD, then GPS IFD, then the value pool.
	sizeOf := func(n int) int { return 2 + n*12 + 4 }

	ifd0Count := len(ifd0)
	if len(exifIFD) > 0 {
		ifd0Count++ // ExifIFDPointer
	}
	if len(gpsIFD) > 0 {
		ifd0Count++ // GPSInfoIFDPointer
	}

	ifd0Off := 8
	exifOff := ifd0Off + sizeOf(ifd0Count)
	gpsOff := exifOff
	if len(exifIFD) > 0 {
		gpsOff = exifOff + sizeOf(len(exifIFD))
	}
	poolOff := gpsOff
	if len(gpsIFD) > 0 {
		poolOff = gpsOff + sizeOf(len(gpsIFD))
	}

	pool := &bytes.Buffer{}
	addPool := func(b []byte) uint32 {
		off := uint32(poolOff + pool.Len())
		pool.Write(b)
		// TIFF offsets should be word-aligned.
		if pool.Len()%2 == 1 {
			pool.WriteByte(0)
		}
		return off
	}

	writeIFD := func(w *bytes.Buffer, entries []entry, extra map[uint16]uint32) {
		total := len(entries) + len(extra)
		w.Write(u16(uint16(total)))

		type pair struct {
			tag uint16
			e   entry
		}
		all := make([]pair, 0, total)
		for _, e := range entries {
			all = append(all, pair{e.tag, e})
		}
		for tag, off := range extra {
			all = append(all, pair{tag, entry{tag: tag, typ: 4, count: 1, value: u32(off)}})
		}
		// TIFF requires entries sorted by tag.
		sort.Slice(all, func(i, j int) bool { return all[i].tag < all[j].tag })

		for _, p := range all {
			e := p.e
			w.Write(u16(e.tag))
			w.Write(u16(e.typ))
			w.Write(u32(e.count))
			if e.pool != nil {
				w.Write(u32(addPool(e.pool)))
			} else {
				w.Write(e.value)
			}
		}
		w.Write(u32(0)) // no next IFD
	}

	var ifds bytes.Buffer
	extra := map[uint16]uint32{}
	if len(exifIFD) > 0 {
		extra[0x8769] = uint32(exifOff)
	}
	if len(gpsIFD) > 0 {
		extra[0x8825] = uint32(gpsOff)
	}
	writeIFD(&ifds, ifd0, extra)
	if len(exifIFD) > 0 {
		writeIFD(&ifds, exifIFD, nil)
	}
	if len(gpsIFD) > 0 {
		writeIFD(&ifds, gpsIFD, nil)
	}

	tiff.Write(ifds.Bytes())
	tiff.Write(pool.Bytes())

	// APP1: marker, length (includes the length field itself), "Exif\0\0", TIFF.
	payload := append([]byte("Exif\x00\x00"), tiff.Bytes()...)
	seg := []byte{0xFF, 0xE1}
	seg = append(seg, u16be(uint16(len(payload)+2))...)
	seg = append(seg, payload...)
	return seg
}

// dms encodes a decimal degree as three EXIF RATIONALs (deg, min, sec).
func dms(deg float64) []byte {
	d := math.Floor(deg)
	mf := (deg - d) * 60
	m := math.Floor(mf)
	s := (mf - m) * 60

	var b bytes.Buffer
	b.Write(u32(uint32(d)))
	b.Write(u32(1))
	b.Write(u32(uint32(m)))
	b.Write(u32(1))
	// Seconds kept to 4 decimal places via a denominator of 10000.
	b.Write(u32(uint32(math.Round(s * 10000))))
	b.Write(u32(10000))
	return b.Bytes()
}

func u16(v uint16) []byte   { return []byte{byte(v), byte(v >> 8)} }
func u32(v uint32) []byte   { return []byte{byte(v), byte(v >> 8), byte(v >> 16), byte(v >> 24)} }
func u16be(v uint16) []byte { return []byte{byte(v >> 8), byte(v)} }

func pad4(b []byte) []byte {
	out := make([]byte, 4)
	copy(out, b)
	return out
}
