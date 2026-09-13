// Package thumbnail renders small, correctly-oriented previews of photos.
//
// Nearly pure: Render takes an image and returns an image. The one piece of
// I/O, WriteAtomic, is here rather than in the caller because the two
// properties that make it safe -- an id-derived filename and an atomic rename
// -- are easy to get subtly wrong and belong next to each other.
//
// Thumbnails are the ONLY image data this application ever writes, and they
// are written to their own directory, never beside the originals. The photo
// library is mounted read-only; a thumbnail directory that lived inside it
// would be the first thing to break that guarantee.
package thumbnail

import (
	"bufio"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/srikarguntaka/photo-organizer/internal/metadata"

	// Decoders for image.Decode.
	_ "image/gif"
	_ "image/png"

	_ "golang.org/x/image/webp"

	"golang.org/x/image/draw"
)

// MaxEdge is the long-edge length of a generated thumbnail, in pixels.
//
// 400 covers a grid tile of about 200 CSS pixels on a 2x display, which is the
// densest layout the UI uses. Larger costs disk and decode time on every page
// of the grid; smaller goes visibly soft on high-DPI screens.
const MaxEdge = 400

// Quality is the JPEG quality. 82 is past the point where blocking artefacts
// are visible at thumbnail size, and well short of where file size starts
// climbing steeply for no perceptible gain.
const Quality = 82

// background is what transparent pixels are composited onto.
//
// JPEG has no alpha channel. Encoding a transparent PNG directly turns every
// transparent pixel BLACK, because the encoder reads the premultiplied colour,
// which is zero. A screenshot with a transparent border would get a black
// frame. A light neutral grey reads as "empty" in both light and dark UI
// themes, where pure white would glare in dark mode.
var background = color.RGBA{R: 0xee, G: 0xee, B: 0xee, A: 0xff}

// Render produces a thumbnail: oriented upright, scaled so its long edge is at
// most maxEdge, and flattened onto an opaque background.
//
// Images already smaller than maxEdge are NOT scaled up. Enlarging a 240px
// image to 400px invents no detail and just ships a bigger, blurrier file.
func Render(src image.Image, orientation, maxEdge int) *image.RGBA {
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	if w <= 0 || h <= 0 {
		return image.NewRGBA(image.Rect(0, 0, 1, 1))
	}

	tw, th := w, h
	if w > maxEdge || h > maxEdge {
		if w >= h {
			tw, th = maxEdge, max(1, h*maxEdge/w)
		} else {
			tw, th = max(1, w*maxEdge/h), maxEdge
		}
	}

	// Flatten FIRST, then scale. Scaling transparent pixels blends their
	// (black, premultiplied) colour into opaque neighbours, leaving a dark
	// fringe around every transparent edge that no later flatten can remove.
	flat := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.Draw(flat, flat.Bounds(), &image.Uniform{C: background}, image.Point{}, draw.Src)
	draw.Draw(flat, flat.Bounds(), src, b.Min, draw.Over)

	scaled := flat
	if tw != w || th != h {
		scaled = image.NewRGBA(image.Rect(0, 0, tw, th))
		// CatmullRom rather than bilinear: at a 10x+ reduction, bilinear
		// samples only a few source pixels per output pixel and aliases fine
		// texture into moire. CatmullRom's wider kernel averages properly.
		draw.CatmullRom.Scale(scaled, scaled.Bounds(), flat, flat.Bounds(), draw.Src, nil)
	}

	// Orient AFTER scaling. Rotation is a pixel permutation, so it costs the
	// same at any size -- doing it on a 400px thumbnail instead of a 12MP
	// original is hundreds of times less work. Scaling to a long edge is
	// symmetric, so the order does not change the result.
	return Orient(scaled, orientation)
}

// ErrDecode wraps a failure to decode the image's pixels. Permanent: the bytes
// will not improve on retry. Distinguished from I/O errors, which may be
// transient.
var ErrDecode = errors.New("thumbnail: cannot decode image")

// FromReader reads an image file and renders its thumbnail, applying the
// file's own EXIF orientation.
//
// Orientation is read from the same bytes that are decoded, so the result is a
// function of the file alone. A file with no EXIF, or EXIF too broken to
// parse, is ordinary rather than an error: it has no rotation to apply.
func FromReader(r io.ReadSeeker, maxEdge int) (*image.RGBA, error) {
	orientation := 1
	// The zero time is the capture-time fallback, which is not read here.
	if md, err := metadata.Extract(r, time.Time{}); err == nil && md.Orientation > 0 {
		orientation = md.Orientation
	}
	if _, err := r.Seek(0, io.SeekStart); err != nil {
		return nil, fmt.Errorf("thumbnail: rewinding: %w", err)
	}

	img, _, err := image.Decode(r)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrDecode, err)
	}
	return Render(img, orientation, maxEdge), nil
}

// Orient applies an EXIF orientation, returning an upright image.
//
// Neither image/jpeg nor x/image applies orientation on decode, so a phone
// photo held upright is decoded lying on its side. Without this step every
// portrait photo in the grid is rotated 90 degrees -- the most visible bug a
// photo tool can have.
//
// The eight values are the EXIF standard's. For each, the destination pixel
// (x, y) is read from a source coordinate; W and H are the SOURCE dimensions.
//
//	1  identity                        src(x,       y)
//	2  mirror horizontal               src(W-1-x,   y)
//	3  rotate 180                      src(W-1-x,   H-1-y)
//	4  mirror vertical                 src(x,       H-1-y)
//	5  mirror horizontal, rotate 270   src(y,       x)        dims swap
//	6  rotate 90 clockwise             src(y,       H-1-x)    dims swap
//	7  mirror horizontal, rotate 90    src(W-1-y,   H-1-x)    dims swap
//	8  rotate 270 clockwise            src(W-1-y,   x)        dims swap
//
// Any other value, including 0 for "no EXIF", is treated as 1. An unknown
// orientation is far more likely to be a missing tag than a genuine request
// for a transform this function does not know.
func Orient(src *image.RGBA, orientation int) *image.RGBA {
	if orientation < 2 || orientation > 8 {
		return src
	}

	b := src.Bounds()
	W, H := b.Dx(), b.Dy()

	dw, dh := W, H
	if orientation >= 5 {
		dw, dh = H, W
	}
	dst := image.NewRGBA(image.Rect(0, 0, dw, dh))

	for y := 0; y < dh; y++ {
		for x := 0; x < dw; x++ {
			var sx, sy int
			switch orientation {
			case 2:
				sx, sy = W-1-x, y
			case 3:
				sx, sy = W-1-x, H-1-y
			case 4:
				sx, sy = x, H-1-y
			case 5:
				sx, sy = y, x
			case 6:
				sx, sy = y, H-1-x
			case 7:
				sx, sy = W-1-y, H-1-x
			case 8:
				sx, sy = W-1-y, x
			}
			si := src.PixOffset(b.Min.X+sx, b.Min.Y+sy)
			di := dst.PixOffset(x, y)
			copy(dst.Pix[di:di+4], src.Pix[si:si+4])
		}
	}
	return dst
}

// Encode writes img as a JPEG.
func Encode(w io.Writer, img image.Image) error {
	return jpeg.Encode(w, img, &jpeg.Options{Quality: Quality})
}

// uuidPattern is the canonical textual UUID form.
var uuidPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// ErrInvalidID is returned for an id that is not a canonical UUID.
var ErrInvalidID = errors.New("thumbnail: photo id is not a canonical uuid")

// PathFor returns where the thumbnail for a photo id lives.
//
// The filename is derived ONLY from the photo id, and the id must be a
// canonical lowercase UUID. That is the whole path-safety argument: a string
// matching this pattern contains no separator, no dot and no drive letter, so
// no value that passes can name anything outside dir. There is no traversal
// check to get wrong because there is nothing to traverse with.
//
// Never derived from the photo's filename or relative path. Those come from
// the user's disk, can contain anything the filesystem allows, and two photos
// in different folders can share a name.
func PathFor(dir, photoID string) (string, error) {
	if !uuidPattern.MatchString(photoID) {
		return "", ErrInvalidID
	}
	// Two levels of fan-out on the id's first characters. A flat directory of
	// a hundred thousand files makes every listing -- and some filesystems'
	// lookups -- slow; 256 buckets keeps each to a few hundred.
	return filepath.Join(dir, photoID[:2], photoID+".jpg"), nil
}

// Written describes a thumbnail that was stored.
type Written struct {
	Path   string
	Width  int
	Height int
	Bytes  int64
}

// WriteAtomic encodes img and stores it as the thumbnail for photoID.
//
// ATOMIC: the JPEG is written to a temporary file in the same directory, synced,
// and renamed into place. A rename within one filesystem is atomic, so a reader
// sees either the complete previous thumbnail or the complete new one -- never
// a half-written file. Without this, a worker killed mid-write (the exact crash
// the job queue is designed to survive) would leave a truncated JPEG that the
// API serves forever, because the job that retries it would find a file
// already there.
//
// The temp file lives beside the target rather than in os.TempDir because a
// rename across filesystems is not atomic, and in Docker /tmp and the
// thumbnail volume are different mounts.
func WriteAtomic(dir, photoID string, img image.Image) (*Written, error) {
	final, err := PathFor(dir, photoID)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(final), 0o755); err != nil {
		return nil, fmt.Errorf("thumbnail: creating directory: %w", err)
	}

	tmp, err := os.CreateTemp(filepath.Dir(final), ".tmp-"+photoID+"-*")
	if err != nil {
		return nil, fmt.Errorf("thumbnail: creating temp file: %w", err)
	}
	// Removes the temp file on every failure path. After a successful rename
	// the name no longer exists and Remove is a harmless no-op.
	defer os.Remove(tmp.Name())

	bw := bufio.NewWriter(tmp)
	if err := Encode(bw, img); err != nil {
		tmp.Close()
		return nil, fmt.Errorf("thumbnail: encoding: %w", err)
	}
	if err := bw.Flush(); err != nil {
		tmp.Close()
		return nil, fmt.Errorf("thumbnail: flushing: %w", err)
	}
	// Sync before rename. Rename is atomic for the directory entry, but
	// without a sync the rename can reach disk before the data does, and a
	// power loss leaves a correctly-named empty file.
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return nil, fmt.Errorf("thumbnail: syncing: %w", err)
	}
	info, err := tmp.Stat()
	if err != nil {
		tmp.Close()
		return nil, fmt.Errorf("thumbnail: stat: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return nil, fmt.Errorf("thumbnail: closing: %w", err)
	}
	if err := os.Rename(tmp.Name(), final); err != nil {
		return nil, fmt.Errorf("thumbnail: renaming into place: %w", err)
	}

	bounds := img.Bounds()
	return &Written{
		Path:   final,
		Width:  bounds.Dx(),
		Height: bounds.Dy(),
		Bytes:  info.Size(),
	}, nil
}
