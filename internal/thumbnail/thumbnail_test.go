package thumbnail

import (
	"bytes"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// grid builds a W x H image where every pixel has a unique red/green value
// encoding its own coordinates. After a transform, reading a pixel tells you
// exactly which source pixel it came from.
func grid(w, h int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetRGBA(x, y, color.RGBA{R: uint8(x), G: uint8(y), B: 7, A: 255})
		}
	}
	return img
}

// origin reports which source coordinate a destination pixel came from.
func origin(img *image.RGBA, x, y int) (int, int) {
	c := img.RGBAAt(x, y)
	return int(c.R), int(c.G)
}

// Where the source's top-left pixel lands under each orientation, for a
// non-square source. Non-square matters: a square source cannot distinguish a
// transform that swaps dimensions from one that does not.
func TestOrientCornerPlacement(t *testing.T) {
	const W, H = 5, 3

	cases := []struct {
		orientation int
		wantW       int
		wantH       int
		// where src(0,0) must end up
		cornerX, cornerY int
	}{
		{1, W, H, 0, 0},
		{2, W, H, W - 1, 0},     // mirrored: top-left -> top-right
		{3, W, H, W - 1, H - 1}, // 180: top-left -> bottom-right
		{4, W, H, 0, H - 1},     // flipped: top-left -> bottom-left
		{5, H, W, 0, 0},         // transpose keeps the top-left
		{6, H, W, H - 1, 0},     // 90 CW: top-left -> top-right
		{7, H, W, H - 1, W - 1}, // transverse: top-left -> bottom-right
		{8, H, W, 0, W - 1},     // 90 CCW: top-left -> bottom-left
	}

	for _, tc := range cases {
		got := Orient(grid(W, H), tc.orientation)
		b := got.Bounds()
		if b.Dx() != tc.wantW || b.Dy() != tc.wantH {
			t.Errorf("orientation %d: size %dx%d, want %dx%d",
				tc.orientation, b.Dx(), b.Dy(), tc.wantW, tc.wantH)
			continue
		}
		if sx, sy := origin(got, tc.cornerX, tc.cornerY); sx != 0 || sy != 0 {
			t.Errorf("orientation %d: pixel at (%d,%d) came from src(%d,%d), want src(0,0)",
				tc.orientation, tc.cornerX, tc.cornerY, sx, sy)
		}
	}
}

// Every orientation is a permutation: each source pixel appears exactly once.
// Catches an off-by-one that duplicates an edge row and drops another, which
// a corner check alone would miss.
func TestOrientIsAPermutation(t *testing.T) {
	const W, H = 7, 4
	for o := 1; o <= 8; o++ {
		got := Orient(grid(W, H), o)
		seen := map[[2]int]int{}
		b := got.Bounds()
		for y := 0; y < b.Dy(); y++ {
			for x := 0; x < b.Dx(); x++ {
				sx, sy := origin(got, x, y)
				seen[[2]int{sx, sy}]++
			}
		}
		if len(seen) != W*H {
			t.Errorf("orientation %d: %d distinct source pixels, want %d", o, len(seen), W*H)
		}
		for k, n := range seen {
			if n != 1 {
				t.Errorf("orientation %d: src%v appears %d times", o, k, n)
			}
		}
	}
}

// The standard's inverse pairs. Rotating 90 CW (6) then 90 CCW (8) must be the
// identity; so must applying any mirror or 180 twice. This checks the table is
// self-consistent, independent of the corner expectations above.
func TestOrientInverses(t *testing.T) {
	src := grid(6, 3)
	for _, pair := range [][2]int{{6, 8}, {8, 6}, {2, 2}, {3, 3}, {4, 4}, {5, 5}, {7, 7}} {
		got := Orient(Orient(src, pair[0]), pair[1])
		if !bytes.Equal(got.Pix, src.Pix) || got.Bounds() != src.Bounds() {
			t.Errorf("orientation %d then %d is not the identity", pair[0], pair[1])
		}
	}
}

func TestUnknownOrientationIsIdentity(t *testing.T) {
	src := grid(4, 2)
	for _, o := range []int{0, -1, 9, 255} {
		if got := Orient(src, o); got != src {
			t.Errorf("orientation %d should return the source unchanged", o)
		}
	}
}

func TestRenderScalesLongEdge(t *testing.T) {
	cases := []struct {
		w, h, orientation int
		wantW, wantH      int
	}{
		{4000, 3000, 1, 400, 300}, // landscape
		{3000, 4000, 1, 300, 400}, // portrait
		{4000, 3000, 6, 300, 400}, // landscape pixels, portrait display
		{200, 100, 1, 200, 100},   // already small: NOT enlarged
		{400, 400, 1, 400, 400},   // exactly the limit
		{10000, 10, 1, 400, 1},    // extreme aspect ratio never collapses to 0
	}
	for _, tc := range cases {
		got := Render(image.NewRGBA(image.Rect(0, 0, tc.w, tc.h)), tc.orientation, MaxEdge)
		if b := got.Bounds(); b.Dx() != tc.wantW || b.Dy() != tc.wantH {
			t.Errorf("Render(%dx%d, orient %d) = %dx%d, want %dx%d",
				tc.w, tc.h, tc.orientation, b.Dx(), b.Dy(), tc.wantW, tc.wantH)
		}
	}
}

// Transparent pixels must not turn black. JPEG has no alpha, and encoding a
// transparent PNG straight through reads the premultiplied colour -- zero.
func TestRenderFlattensTransparencyOntoBackground(t *testing.T) {
	src := image.NewNRGBA(image.Rect(0, 0, 10, 10)) // fully transparent

	got := Render(src, 1, MaxEdge)
	if c := got.RGBAAt(5, 5); c != background {
		t.Fatalf("transparent pixel rendered as %v, want the background %v", c, background)
	}

	// And survives the JPEG round trip as something light, not black.
	var buf bytes.Buffer
	if err := Encode(&buf, got); err != nil {
		t.Fatal(err)
	}
	decoded, err := jpeg.Decode(&buf)
	if err != nil {
		t.Fatal(err)
	}
	r, g, b, _ := decoded.At(5, 5).RGBA()
	if r>>8 < 200 || g>>8 < 200 || b>>8 < 200 {
		t.Errorf("transparent area encoded as (%d,%d,%d); it went dark", r>>8, g>>8, b>>8)
	}
}

func TestPathForRejectsAnythingButACanonicalUUID(t *testing.T) {
	dir := t.TempDir()

	good := "8142fe59-1e27-4655-8076-436e8b00d498"
	p, err := PathFor(dir, good)
	if err != nil {
		t.Fatalf("PathFor(%q): %v", good, err)
	}
	if want := filepath.Join(dir, "81", good+".jpg"); p != want {
		t.Errorf("PathFor = %q, want %q", p, want)
	}

	for _, bad := range []string{
		"",
		"../../etc/passwd",
		"..",
		"8142fe59-1e27-4655-8076-436e8b00d498/../../x",
		`8142fe59-1e27-4655-8076-436e8b00d498\..\x`,
		"8142FE59-1E27-4655-8076-436E8B00D498", // uppercase: not canonical
		"8142fe59-1e27-4655-8076-436e8b00d49",  // one short
		"8142fe591e2746558076436e8b00d498",     // no hyphens
		"C:8142fe59-1e27-4655-8076-436e8b00d498",
	} {
		if _, err := PathFor(dir, bad); err == nil {
			t.Errorf("PathFor accepted %q", bad)
		}
	}
}

func TestWriteAtomicLeavesNoTempFiles(t *testing.T) {
	dir := t.TempDir()
	id := "8142fe59-1e27-4655-8076-436e8b00d498"

	w, err := WriteAtomic(dir, id, grid(20, 10))
	if err != nil {
		t.Fatal(err)
	}
	if w.Width != 20 || w.Height != 10 || w.Bytes <= 0 {
		t.Errorf("Written = %+v", w)
	}

	f, err := os.Open(w.Path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := jpeg.Decode(f); err != nil {
		t.Errorf("written thumbnail does not decode: %v", err)
	}

	entries, err := os.ReadDir(filepath.Dir(w.Path))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".tmp-") {
			t.Errorf("temp file left behind: %s", e.Name())
		}
	}
}

// Rewriting is an overwrite, not an error: a job retried after its thumbnail
// already landed must succeed, because handlers are idempotent.
func TestWriteAtomicOverwrites(t *testing.T) {
	dir := t.TempDir()
	id := "8142fe59-1e27-4655-8076-436e8b00d498"

	if _, err := WriteAtomic(dir, id, grid(20, 10)); err != nil {
		t.Fatal(err)
	}
	w, err := WriteAtomic(dir, id, grid(8, 30))
	if err != nil {
		t.Fatalf("second write: %v", err)
	}

	f, _ := os.Open(w.Path)
	defer f.Close()
	cfg, err := jpeg.DecodeConfig(f)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Width != 8 || cfg.Height != 30 {
		t.Errorf("after overwrite the file is %dx%d, want the new 8x30", cfg.Width, cfg.Height)
	}
}

func TestWriteAtomicRejectsBadIDBeforeTouchingDisk(t *testing.T) {
	dir := t.TempDir()
	if _, err := WriteAtomic(dir, "../escape", grid(2, 2)); err == nil {
		t.Fatal("WriteAtomic accepted a traversal id")
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Errorf("a rejected id still created %d entries in the directory", len(entries))
	}
}

// jpegWithOrientation encodes img and splices in a minimal EXIF APP1 segment
// carrying only an Orientation tag.
//
// Hand-built rather than borrowed from the fixture generator, which has no
// orientation support, and deliberately NOT produced by any library this
// package also uses to READ EXIF -- a shared bug would cancel itself out.
//
//	FF E1 <len:BE16> "Exif\0\0"
//	TIFF  "II" 2A 00 <ifd0 offset=8:LE32>
//	IFD0  <count=1:LE16>
//	      <tag=0x0112:LE16> <type=SHORT(3):LE16> <count=1:LE32> <value:LE16> 00 00
//	      <next IFD=0:LE32>
func jpegWithOrientation(t *testing.T, img image.Image, orientation uint16) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 95}); err != nil {
		t.Fatal(err)
	}
	raw := buf.Bytes()

	tiff := []byte{
		'I', 'I', 0x2A, 0x00, 8, 0, 0, 0,
		1, 0,
		0x12, 0x01, 3, 0, 1, 0, 0, 0, byte(orientation), byte(orientation >> 8), 0, 0,
		0, 0, 0, 0,
	}
	payload := append([]byte("Exif\x00\x00"), tiff...)
	segLen := len(payload) + 2
	app1 := append([]byte{0xFF, 0xE1, byte(segLen >> 8), byte(segLen)}, payload...)

	out := append([]byte{}, raw[:2]...) // SOI
	out = append(out, app1...)
	return append(out, raw[2:]...)
}

// markedLandscape is 60x20 with a pure red block in its top-left corner and
// blue everywhere else, so where the red lands after rendering shows which way
// the image was turned. Blocks, not single pixels: JPEG smears single pixels.
func markedLandscape() *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, 60, 20))
	for y := 0; y < 20; y++ {
		for x := 0; x < 60; x++ {
			c := color.RGBA{B: 255, A: 255}
			if x < 15 && y < 10 {
				c = color.RGBA{R: 255, A: 255}
			}
			img.SetRGBA(x, y, c)
		}
	}
	return img
}

func isRed(c color.RGBA) bool { return c.R > 180 && c.B < 80 }

// End to end through real bytes: EXIF written into a JPEG, parsed by the real
// metadata extractor, applied by Render. Orient's unit tests prove the pixel
// arithmetic; only this proves the thumbnail pass actually READS orientation
// from a file, which is the step that would silently do nothing if the
// extractor's output were ever ignored.
func TestFromReaderAppliesFileOrientation(t *testing.T) {
	cases := []struct {
		orientation      uint16
		wantW, wantH     int
		redX, redY       int // a point deep inside where the red block must be
		notRedX, notRedY int // the opposite corner, which must not be red
	}{
		// No rotation: stays landscape, red top-left.
		{1, 60, 20, 5, 5, 55, 15},
		// 90 CW: becomes portrait 20x60, top-left moves to top-right.
		{6, 20, 60, 15, 5, 5, 55},
		// 90 CCW: portrait, top-left moves to bottom-left.
		{8, 20, 60, 5, 55, 15, 5},
		// 180: stays landscape, top-left moves to bottom-right.
		{3, 60, 20, 55, 15, 5, 5},
	}

	for _, tc := range cases {
		data := jpegWithOrientation(t, markedLandscape(), tc.orientation)

		got, err := FromReader(bytes.NewReader(data), MaxEdge)
		if err != nil {
			t.Fatalf("orientation %d: %v", tc.orientation, err)
		}

		if b := got.Bounds(); b.Dx() != tc.wantW || b.Dy() != tc.wantH {
			t.Errorf("orientation %d: rendered %dx%d, want %dx%d",
				tc.orientation, b.Dx(), b.Dy(), tc.wantW, tc.wantH)
			continue
		}
		if c := got.RGBAAt(tc.redX, tc.redY); !isRed(c) {
			t.Errorf("orientation %d: (%d,%d) = %v, want the red marker there",
				tc.orientation, tc.redX, tc.redY, c)
		}
		if c := got.RGBAAt(tc.notRedX, tc.notRedY); isRed(c) {
			t.Errorf("orientation %d: (%d,%d) is red; the image was turned the wrong way",
				tc.orientation, tc.notRedX, tc.notRedY)
		}
	}
}

func TestFromReaderWithoutEXIFIsUpright(t *testing.T) {
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, markedLandscape(), nil); err != nil {
		t.Fatal(err)
	}
	got, err := FromReader(bytes.NewReader(buf.Bytes()), MaxEdge)
	if err != nil {
		t.Fatal(err)
	}
	if b := got.Bounds(); b.Dx() != 60 || b.Dy() != 20 {
		t.Errorf("no-EXIF image rendered %dx%d, want 60x20 unchanged", b.Dx(), b.Dy())
	}
}

func TestFromReaderReportsDecodeFailureDistinctly(t *testing.T) {
	_, err := FromReader(bytes.NewReader([]byte("definitely not an image")), MaxEdge)
	if !errors.Is(err, ErrDecode) {
		t.Fatalf("got %v, want an ErrDecode so the caller marks it permanent", err)
	}
}
