package hashing

import (
	"fmt"
	"image"
	"io"
	"math/bits"

	"golang.org/x/image/draw"

	// Register decoders for image.Decode. Without these it fails with
	// "unknown format" on every real photo.
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"

	_ "golang.org/x/image/webp"
)

// Perceptual hash geometry.
//
// The image is reduced to a 9x8 greyscale grid and each row's horizontally
// adjacent pixels are compared, giving 8 comparisons per row across 8 rows =
// 64 bits. The extra column exists precisely to produce 8 comparisons from 9
// samples.
const (
	dhashWidth  = 9
	dhashHeight = 8
	// DHashBits is the width of the resulting hash.
	DHashBits = (dhashWidth - 1) * dhashHeight // 64
)

// DHash computes a 64-bit difference hash from an already-decoded image.
//
// WHY dHash rather than the alternatives:
//
//	aHash (average)  -- compares each pixel to the image mean. Simplest, but
//	                    a brightness or contrast shift moves the mean and
//	                    flips many bits at once. Recompression and light
//	                    editing do exactly that, which is the case we most
//	                    need to catch.
//	dHash (gradient) -- CHOSEN. Compares each pixel to its neighbour, so it
//	                    encodes relative change rather than absolute level.
//	                    Uniformly brightening an image leaves every
//	                    comparison unchanged. Cheap, and fully explainable:
//	                    the UI can show the exact bit distance.
//	pHash (DCT)      -- More robust to rotation and heavy edits, at roughly
//	                    10x the cost and much harder to explain to a user.
//	                    Deferred; the interface here would accommodate it.
//	embeddings       -- Better semantic recall, but needs a model, is not
//	                    interpretable, and makes "why are these grouped?"
//	                    unanswerable. Also out of scope by requirement.
//
// KNOWN WEAKNESSES, stated plainly rather than discovered later:
//
//   - Not rotation invariant. A rotated copy will not match. Detecting that
//     would mean hashing four rotations and comparing all of them.
//   - Weak on heavy crops, which change the gradient structure everywhere.
//   - Flat, low-detail images (a blank wall, a dark frame) produce
//     near-identical hashes and collide with each other. Quality metrics are
//     considered alongside distance for exactly this reason.
func DHash(img image.Image) uint64 {
	// ApproxBiLinear rather than NearestNeighbor: nearest-neighbour sampling
	// of a 12-megapixel image down to 9x8 reads only 72 source pixels, so the
	// result depends on which 72 pixels happen to land on the grid and is
	// unstable under a small resize. Averaging over the whole area is what
	// makes the hash survive the resizing we are trying to detect.
	small := image.NewRGBA(image.Rect(0, 0, dhashWidth, dhashHeight))
	draw.ApproxBiLinear.Scale(small, small.Bounds(), img, img.Bounds(), draw.Src, nil)

	// Greyscale via luminance weights. A naive (r+g+b)/3 would treat a
	// saturated blue and a mid grey as equally bright, which they are not
	// perceptually.
	var grey [dhashHeight][dhashWidth]float64
	for y := 0; y < dhashHeight; y++ {
		for x := 0; x < dhashWidth; x++ {
			c := small.RGBAAt(x, y)
			grey[y][x] = 0.299*float64(c.R) + 0.587*float64(c.G) + 0.114*float64(c.B)
		}
	}

	var hash uint64
	bit := 0
	for y := 0; y < dhashHeight; y++ {
		for x := 0; x < dhashWidth-1; x++ {
			if grey[y][x] > grey[y][x+1] {
				hash |= 1 << uint(bit)
			}
			bit++
		}
	}
	return hash
}

// DHashReader decodes an image and computes its perceptual hash.
//
// MEMORY NOTE: unlike Sum, this cannot stream. A perceptual hash needs pixels,
// and Go's image/jpeg has no scaled-decode entry point, so a 12-megapixel JPEG
// briefly occupies ~48MB as RGBA. That is the reason pixel work runs under a
// tighter concurrency bound than metadata extraction, which only reads headers.
func DHashReader(r io.Reader) (uint64, error) {
	img, _, err := image.Decode(r)
	if err != nil {
		return 0, fmt.Errorf("hashing: decoding image for perceptual hash: %w", err)
	}
	return DHash(img), nil
}

// HammingDistance counts differing bits between two hashes.
//
// This is the similarity measure the whole feature is built on, and it is one
// instruction: XOR to find differing bits, then population count. That is why
// a brute-force pass over 10,000 photos (50M comparisons) is only a few
// seconds -- see DESIGN_DECISIONS.md on why the bucketing optimisation is
// designed but deliberately not built yet.
func HammingDistance(a, b uint64) int {
	return bits.OnesCount64(a ^ b)
}

// Similar reports whether two hashes are within the given distance.
func Similar(a, b uint64, threshold int) bool {
	return HammingDistance(a, b) <= threshold
}

// DefaultSimilarityThreshold is the default Hamming distance below which two
// photos are considered near-duplicates.
//
// Calibration, on a 64-bit hash:
//
//	0      byte-identical or visually indistinguishable
//	1-6    the same photo, recompressed or resized
//	7-12   the same scene: burst frames, minor edits, small crops
//	13-20  related but distinguishable; a lot of false positives here
//	21+    unrelated (random pairs average ~32, half the bits)
//
// 10 sits inside the "same scene" band. Erring low is deliberate: a missed
// near-duplicate costs the user some disk space, while a false positive puts
// two unrelated photos in a group and invites deleting one of them.
//
// Configurable via SIMILARITY_THRESHOLD, and the API exposes the raw distance
// so a user can see exactly how close a pair was.
const DefaultSimilarityThreshold = 10

// MaxSimilarityThreshold caps configuration. Beyond this the results are noise:
// random unrelated hashes differ by ~32 bits, so a threshold near that groups
// everything with everything.
const MaxSimilarityThreshold = 24

// DHashString renders a hash as 16 hex characters for display.
func DHashString(h uint64) string {
	return fmt.Sprintf("%016x", h)
}
