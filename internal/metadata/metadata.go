// Package metadata extracts what a photo file can tell us about itself:
// dimensions, capture time, location, and camera.
//
// It is a pure package -- no database import, no filesystem walking. Extract
// takes a reader and returns a value, which is what lets it be tested
// exhaustively against the fixture corpus (files with EXIF+GPS, EXIF without
// GPS, no EXIF at all, and three kinds of corruption) without Postgres.
//
// The governing principle is that missing metadata is normal, not an error. A
// real photo dump is full of screenshots with no EXIF, images stripped by
// messaging apps, and files whose EXIF is subtly malformed. Extract returns
// what it could determine and reports what it could not, rather than failing
// the whole photo because one tag was unreadable.
package metadata

import (
	"errors"
	"fmt"
	"image"
	"io"
	"strings"
	"time"

	"github.com/rwcarlsen/goexif/exif"

	// Registers the decoders used by image.DecodeConfig. These read only the
	// file header, not the pixels, so getting dimensions costs a few hundred
	// bytes of I/O rather than decoding a 12-megapixel image.
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"

	_ "golang.org/x/image/webp"
)

// ErrNotAnImage is returned when the file cannot be recognised as any
// supported image format. Callers treat this as a permanent failure -- retrying
// will not make a text file into a JPEG.
var ErrNotAnImage = errors.New("metadata: file is not a decodable image")

// TimeSource records where a capture timestamp came from, so downstream
// consumers know how much to trust it.
type TimeSource string

const (
	// SourceEXIF means the timestamp came from EXIF DateTimeOriginal or
	// DateTimeDigitized. Reliable, subject to the timezone caveat below.
	SourceEXIF TimeSource = "exif"
	// SourceFilesystem means there was no EXIF date and we fell back to the
	// file's modification time. This is often wrong -- copying a photo can
	// reset mtime -- which is exactly why it is labelled.
	SourceFilesystem TimeSource = "filesystem"
)

// Metadata is everything Extract could determine. Pointer fields are nil when
// the information genuinely is not present in the file.
type Metadata struct {
	// Width and Height are DISPLAY dimensions: already swapped when
	// Orientation indicates a 90 or 270 degree rotation.
	Width  int
	Height int

	// Format as determined by decoding the header, not by the file extension.
	// A file named .jpg containing a PNG reports "png".
	Format string

	// Orientation is the raw EXIF value 1-8, or 0 when absent. Pixel-level
	// work in later phases must apply this itself.
	Orientation int

	CapturedAt       *time.Time
	CapturedAtSource TimeSource

	Latitude  *float64
	Longitude *float64

	CameraMake  *string
	CameraModel *string

	// Warnings records non-fatal problems: EXIF present but unparseable, a GPS
	// block that decoded to an impossible coordinate, and so on. Surfaced
	// rather than swallowed, because "why does this photo have no date" is a
	// question the user will eventually ask.
	Warnings []string
}

// HasLocation reports whether a usable GPS fix was found.
func (m *Metadata) HasLocation() bool { return m.Latitude != nil && m.Longitude != nil }

// Extract reads metadata from r.
//
// fallbackModTime is used as the capture time when the file carries no EXIF
// date. Pass the file's mtime; pass the zero time to disable the fallback.
//
// r must be seekable: dimensions come from the header and EXIF from an APP1
// segment, and the reader is rewound between the two passes.
//
// An error is returned only when the file is not a decodable image at all.
// Everything else -- absent EXIF, absent GPS, malformed tags -- yields a
// partially-populated Metadata plus warnings.
func Extract(r io.ReadSeeker, fallbackModTime time.Time) (*Metadata, error) {
	m := &Metadata{}

	// --- pass 1: dimensions and true format, from the header only ---------
	if _, err := r.Seek(0, io.SeekStart); err != nil {
		return nil, fmt.Errorf("metadata: seeking to start: %w", err)
	}

	cfg, format, err := image.DecodeConfig(r)
	if err != nil {
		// This is the corrupt/not-an-image path. It is a permanent condition,
		// so it is reported distinctly from a transient read failure.
		return nil, fmt.Errorf("%w: %v", ErrNotAnImage, err)
	}
	m.Width, m.Height = cfg.Width, cfg.Height
	m.Format = format

	// --- pass 2: EXIF ------------------------------------------------------
	// Absence of EXIF is not an error. PNGs never have it, and plenty of JPEGs
	// have had it stripped.
	if _, err := r.Seek(0, io.SeekStart); err != nil {
		return nil, fmt.Errorf("metadata: rewinding for exif: %w", err)
	}

	x, err := exif.Decode(r)
	if err != nil {
		// Distinguish "no EXIF" (normal) from "EXIF present but broken"
		// (worth a warning), because the latter may indicate a damaged file.
		if !exif.IsCriticalError(err) {
			m.Warnings = append(m.Warnings, "exif partially unreadable: "+err.Error())
		}
		if x == nil {
			m.applyTimeFallback(fallbackModTime)
			return m, nil
		}
	}

	m.Orientation = readOrientation(x, m)
	m.applyOrientation()

	m.CameraMake = readString(x, exif.Make)
	m.CameraModel = readString(x, exif.Model)

	if t, ok := readCaptureTime(x, m); ok {
		m.CapturedAt = &t
		m.CapturedAtSource = SourceEXIF
	} else {
		m.applyTimeFallback(fallbackModTime)
	}

	readLocation(x, m)

	return m, nil
}

// applyTimeFallback uses filesystem mtime when EXIF gave us nothing, and
// records that it did so.
func (m *Metadata) applyTimeFallback(modTime time.Time) {
	if modTime.IsZero() {
		return
	}
	t := modTime.UTC()
	m.CapturedAt = &t
	m.CapturedAtSource = SourceFilesystem
}

// applyOrientation swaps width and height for the rotated orientations, so the
// stored dimensions are what a viewer would display.
//
// EXIF orientations 5-8 involve a 90 or 270 degree rotation; 1-4 do not.
func (m *Metadata) applyOrientation() {
	if m.Orientation >= 5 && m.Orientation <= 8 {
		m.Width, m.Height = m.Height, m.Width
	}
}

// readCaptureTime pulls the capture timestamp and interprets it as UTC.
//
// THE TIMEZONE DECISION, in code: EXIF DateTimeOriginal is a naive local
// timestamp with no zone. Parsers therefore guess -- goexif attaches
// time.Local -- which means the same file decodes to a different instant on a
// developer laptop in US/Central than in a UTC CI container. Storing that guess
// would make captured_at depend on which machine ran the scan, and clustering
// would silently disagree with itself.
//
// So the wall clock is interpreted as UTC, uniformly. That does not recover the
// real local time, and it is not pretending to: it is consistent,
// machine-independent, and preserves ordering within a library, which is what
// clustering actually needs. captured_at_source records that the value came
// from EXIF so the imprecision stays visible.
//
// DateTimeOriginal (when the shutter fired) is preferred over DateTimeDigitized
// (when it was scanned/imported), which differ for digitised film.
func readCaptureTime(x *exif.Exif, m *Metadata) (time.Time, bool) {
	if x == nil {
		return time.Time{}, false
	}

	for _, name := range []exif.FieldName{exif.DateTimeOriginal, exif.DateTimeDigitized, exif.DateTime} {
		tag, err := x.Get(name)
		if err != nil {
			continue
		}
		s, err := tag.StringVal()
		if err != nil {
			continue
		}
		s = strings.TrimRight(strings.TrimSpace(s), "\x00")
		if s == "" {
			continue
		}

		// EXIF format is "YYYY:MM:DD HH:MM:SS". ParseInLocation with UTC is
		// what makes this machine-independent -- time.Parse would also default
		// to UTC, but stating it is the point.
		t, err := time.ParseInLocation("2006:01:02 15:04:05", s, time.UTC)
		if err != nil {
			m.Warnings = append(m.Warnings, fmt.Sprintf("unparseable %s %q", name, s))
			continue
		}
		// Cameras with a dead clock battery emit 1970 or 0000. A timestamp
		// that old is noise, and letting it through would drag a cluster
		// boundary across the entire library.
		if t.Year() < 1900 {
			m.Warnings = append(m.Warnings, fmt.Sprintf("implausible %s %q, ignoring", name, s))
			continue
		}
		return t, true
	}
	return time.Time{}, false
}

// readLocation pulls GPS coordinates, rejecting the ones that are technically
// valid but practically meaningless.
func readLocation(x *exif.Exif, m *Metadata) {
	if x == nil {
		return
	}

	lat, lon, err := x.LatLong()
	if err != nil {
		// No GPS block at all is the common case and not worth a warning. A
		// GPS block that fails to parse is worth one.
		if !strings.Contains(err.Error(), "not found") {
			m.Warnings = append(m.Warnings, "gps unreadable: "+err.Error())
		}
		return
	}

	// Out-of-range values mean a malformed GPS block, not a real place.
	if lat < -90 || lat > 90 || lon < -180 || lon > 180 {
		m.Warnings = append(m.Warnings, fmt.Sprintf("gps out of range (%.4f, %.4f), ignoring", lat, lon))
		return
	}

	// Exactly (0,0) is Null Island: the canonical output of a GPS chip that
	// has no fix, not a photo taken in the Gulf of Guinea. Treating it as a
	// real location would drag clustering toward a point nobody visited.
	if lat == 0 && lon == 0 {
		m.Warnings = append(m.Warnings, "gps reported (0,0), treating as no fix")
		return
	}

	m.Latitude, m.Longitude = &lat, &lon
}

func readOrientation(x *exif.Exif, m *Metadata) int {
	if x == nil {
		return 0
	}
	tag, err := x.Get(exif.Orientation)
	if err != nil {
		return 0
	}
	v, err := tag.Int(0)
	if err != nil {
		return 0
	}
	if v < 1 || v > 8 {
		m.Warnings = append(m.Warnings, fmt.Sprintf("invalid orientation %d, ignoring", v))
		return 0
	}
	return v
}

func readString(x *exif.Exif, name exif.FieldName) *string {
	if x == nil {
		return nil
	}
	tag, err := x.Get(name)
	if err != nil {
		return nil
	}
	s, err := tag.StringVal()
	if err != nil {
		return nil
	}
	// EXIF strings are NUL-terminated and frequently space-padded.
	s = strings.TrimSpace(strings.TrimRight(s, "\x00"))
	if s == "" {
		return nil
	}
	return &s
}
