package photos

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Discovered is one supported image file found during a walk. It carries only
// what the filesystem can tell us without opening the file; everything else is
// derived later by workers.
type Discovered struct {
	// RelPath is relative to the walk root, forward slashes.
	RelPath string
	// AbsPath is the full path on this machine. Not stored in the database --
	// it is reconstructed from the library root plus RelPath, so moving the
	// library does not invalidate every row.
	AbsPath string
	Name    string
	Format  Format
	Size    int64
	ModTime int64 // Unix seconds; zero when unavailable
}

// WalkStats records what a walk saw, including what it chose to ignore. The
// skip counts matter: "we found 42 photos" is much less useful than "we found
// 42 photos, skipped 4 unsupported files and could not read 1 directory".
type WalkStats struct {
	FilesSeen       int
	DirsSeen        int
	Supported       int
	SkippedFormat   int
	SkippedHidden   int
	SkippedTooLarge int
	Unreadable      int
}

// WalkOptions tunes a walk.
type WalkOptions struct {
	// MaxFileBytes skips files larger than this. Zero means no limit. A guard
	// against a stray disk image or video sitting in a photo folder being
	// pulled into the pipeline.
	MaxFileBytes int64
	// IncludeHidden walks dot-directories and dot-files. Off by default:
	// .git, .thumbnails, macOS resource forks and similar are noise, and a
	// photo library that hides its photos is not a real case.
	IncludeHidden bool
}

// DefaultWalkOptions are the settings used when a caller has no opinion.
func DefaultWalkOptions() WalkOptions {
	return WalkOptions{
		// 512MB. Far above any real photo, far below a video file or a disk
		// image someone left in their Pictures folder.
		MaxFileBytes:  512 << 20,
		IncludeHidden: false,
	}
}

// Walk recursively finds supported image files under root and calls fn for
// each. It is the local-filesystem implementation of photo discovery; a future
// PhotoKit source produces the same Discovered values from a different origin,
// which is why fn takes a value rather than a path.
//
// Errors reading individual entries are recorded in stats and do NOT abort the
// walk. A single unreadable directory in a 10,000-photo library should not
// cost the user the other 9,999 photos. Errors that indicate the walk itself
// is broken (root unreadable) are returned.
//
// fn returning an error aborts the walk and that error is returned -- that is
// the caller saying stop, which is different from the filesystem misbehaving.
//
// ctx is checked between entries so a cancelled scan stops promptly rather
// than walking to completion and discarding the result.
func Walk(ctx context.Context, root string, opts WalkOptions, fn func(Discovered) error) (WalkStats, error) {
	var stats WalkStats

	rootInfo, err := os.Stat(root)
	if err != nil {
		return stats, fmt.Errorf("photos: cannot read root %q: %w", root, err)
	}
	if !rootInfo.IsDir() {
		return stats, fmt.Errorf("photos: root %q is not a directory", root)
	}

	walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		// Cheap cancellation check. Checked first so a cancelled context stops
		// even if this entry also carries an error.
		if ctx.Err() != nil {
			return ctx.Err()
		}

		if err != nil {
			// A permission error on one subdirectory is survivable; record it
			// and keep going. Returning the error here would abort everything.
			stats.Unreadable++
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}

		name := d.Name()

		if d.IsDir() {
			// Never skip the root itself, even if the user's photo directory
			// happens to be named something starting with a dot.
			if path != root && !opts.IncludeHidden && isHidden(name) {
				stats.SkippedHidden++
				return fs.SkipDir
			}
			stats.DirsSeen++
			return nil
		}

		// Symlinks are not followed. WalkDir does not follow them by default,
		// and that is what we want: a symlink loop would hang the scan, and a
		// symlink out of the library would pull in files from outside the
		// permitted root -- the exact thing ResolveWithin prevents at the API
		// boundary.
		if d.Type()&fs.ModeSymlink != 0 {
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}

		stats.FilesSeen++

		if !opts.IncludeHidden && isHidden(name) {
			stats.SkippedHidden++
			return nil
		}

		format, ok := FormatForPath(name)
		if !ok {
			stats.SkippedFormat++
			return nil
		}

		info, err := d.Info()
		if err != nil {
			// The file existed a moment ago and is gone now. Common enough in
			// a live photo folder to be worth surviving.
			stats.Unreadable++
			return nil
		}

		if opts.MaxFileBytes > 0 && info.Size() > opts.MaxFileBytes {
			stats.SkippedTooLarge++
			return nil
		}

		rel, err := RelativeTo(root, path)
		if err != nil {
			stats.Unreadable++
			return nil
		}

		var modUnix int64
		if mt := info.ModTime(); !mt.IsZero() {
			modUnix = mt.Unix()
		}

		stats.Supported++
		return fn(Discovered{
			RelPath: rel,
			AbsPath: path,
			Name:    name,
			Format:  format,
			Size:    info.Size(),
			ModTime: modUnix,
		})
	})

	if walkErr != nil {
		return stats, walkErr
	}
	return stats, nil
}

// isHidden reports whether a filename is hidden by Unix convention. On Windows
// the hidden *attribute* is the real signal, but dot-prefixed names are still
// the dominant convention for tool droppings (.git, .DS_Store, .thumbnails) and
// checking the attribute would need a syscall per entry.
func isHidden(name string) bool {
	return strings.HasPrefix(name, ".")
}
