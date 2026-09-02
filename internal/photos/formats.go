package photos

import (
	"path/filepath"
	"strings"
)

// Format is a supported image container.
type Format string

const (
	FormatJPEG Format = "jpeg"
	FormatPNG  Format = "png"
	FormatWebP Format = "webp"
)

// supportedExtensions maps a lowercased file extension to its format.
//
// Scanning classifies by extension alone, deliberately. Sniffing content means
// opening every file in the library during the walk, which turns a fast
// directory traversal into an I/O-bound crawl for information we do not need
// yet. Phase 3 opens each file anyway to read EXIF, and corrects
// detected_format there if the extension lied.
//
// The consequence is that a text file named "photo.jpg" is discovered as a
// photo and fails later during decode. That is the correct trade: a scan should
// be fast and complete, and a file that cannot be decoded is a real condition
// the pipeline must handle regardless (the fixture corpus includes three).
var supportedExtensions = map[string]Format{
	".jpg":  FormatJPEG,
	".jpeg": FormatJPEG,
	".jpe":  FormatJPEG,
	".png":  FormatPNG,
	".webp": FormatWebP,
}

// FormatForPath returns the format implied by a path's extension, and whether
// it is one this system supports.
//
// HEIC is intentionally absent. Pure-Go HEIC decoding is not practical, and
// adding it means a CGo dependency on libheif or libvips. Discovering HEIC
// files we cannot process would create photo rows that permanently fail, which
// is worse than skipping them and saying so in the limitations.
func FormatForPath(path string) (Format, bool) {
	ext := strings.ToLower(filepath.Ext(path))
	f, ok := supportedExtensions[ext]
	return f, ok
}

// IsSupported reports whether a path looks like an image this system handles.
func IsSupported(path string) bool {
	_, ok := FormatForPath(path)
	return ok
}

// SupportedExtensions lists every recognised extension, for help text and for
// the API to advertise.
func SupportedExtensions() []string {
	out := make([]string, 0, len(supportedExtensions))
	for ext := range supportedExtensions {
		out = append(out, ext)
	}
	// Sorted so output is stable across runs (Go map iteration is randomised).
	for i := 0; i < len(out); i++ {
		for j := i + 1; j < len(out); j++ {
			if out[j] < out[i] {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}
