package photos

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
)

// ErrOutsideRoot is returned when a path escapes the configured photo root.
// Callers translate it into a 400, never a 500 -- it is a client mistake (or an
// attack), not a server fault.
var ErrOutsideRoot = errors.New("path is outside the permitted photo root")

// ResolveWithin resolves target and verifies it sits inside root.
//
// This is a security boundary, not a convenience. The API accepts a directory
// to scan from a client; without this check that is an arbitrary-file-read
// primitive. It has to survive all of:
//
//   - relative paths and "..' traversal        ("../../etc")
//   - absolute paths pointing elsewhere        ("/etc", "C:\\Windows")
//   - symlinks inside root pointing outside    (the subtle one)
//   - prefix confusion                         ("/photos-evil" vs "/photos")
//   - Windows separators and case               ("C:/Photos" vs "c:\\photos")
//
// Both root and target are fully resolved (absolute, symlinks evaluated) before
// comparison, because a prefix check against an unresolved path is exactly the
// bug this function exists to prevent.
//
// root must already exist. target need not: a caller may be validating a path
// before creating it. When target does not exist, the deepest existing ancestor
// is resolved instead, so a symlinked parent is still caught.
func ResolveWithin(root, target string) (string, error) {
	if root == "" {
		return "", errors.New("photos: root must not be empty")
	}
	if strings.TrimSpace(target) == "" {
		return "", errors.New("photos: path must not be empty")
	}

	resolvedRoot, err := resolveExisting(root)
	if err != nil {
		return "", fmt.Errorf("photos: resolving root %q: %w", root, err)
	}

	// A relative target is interpreted against the root rather than the
	// process working directory. "trip/day1" means "inside the library", which
	// is what a caller means, and it removes any dependence on where the
	// process happens to have been started.
	if !filepath.IsAbs(target) {
		target = filepath.Join(resolvedRoot, target)
	}

	resolvedTarget, err := resolveExisting(target)
	if err != nil {
		return "", fmt.Errorf("photos: resolving %q: %w", target, err)
	}

	if !within(resolvedRoot, resolvedTarget) {
		// The error deliberately does not echo the resolved path back to the
		// caller: on a failed traversal attempt that would confirm what does
		// and does not exist on the filesystem.
		return "", ErrOutsideRoot
	}

	return resolvedTarget, nil
}

// within reports whether child is root or sits beneath it.
//
// filepath.Rel is used rather than strings.HasPrefix because a prefix test
// reports "/photos-evil" as being inside "/photos". Rel gives a path-segment
// aware answer.
func within(root, child string) bool {
	rel, err := filepath.Rel(root, child)
	if err != nil {
		// Rel fails when the paths are on different volumes (different Windows
		// drives), which means child is definitively not inside root.
		return false
	}
	if rel == "." {
		return true // child IS root; scanning the root itself is legitimate
	}
	// ".." as the first segment means child climbed out of root. Checking the
	// segment (not the string prefix) avoids rejecting a legitimate directory
	// literally named "..foo".
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false
	}
	return !filepath.IsAbs(rel)
}

// resolveExisting returns an absolute, symlink-free path. When the path does
// not exist, it resolves the deepest ancestor that does and re-appends the
// missing segments, so a symlinked parent directory is still detected.
func resolveExisting(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	abs = filepath.Clean(abs)

	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return filepath.Clean(resolved), nil
	}

	// Walk up to the nearest existing ancestor.
	var missing []string
	current := abs
	for {
		parent := filepath.Dir(current)
		if parent == current {
			// Reached the volume root without finding anything that exists.
			return abs, nil
		}
		missing = append([]string{filepath.Base(current)}, missing...)
		current = parent

		if resolved, err := filepath.EvalSymlinks(current); err == nil {
			return filepath.Clean(filepath.Join(append([]string{resolved}, missing...)...)), nil
		}
	}
}

// RelativeTo returns path expressed relative to root, normalised to forward
// slashes.
//
// Forward slashes are stored regardless of platform so that a library scanned
// on Windows and the same library scanned on Linux produce identical
// relative_path values -- otherwise the unique index on
// (library_id, relative_path) would treat "trip/a.jpg" and "trip\a.jpg" as two
// different photos and every rescan across platforms would duplicate the
// library.
func RelativeTo(root, path string) (string, error) {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return "", fmt.Errorf("photos: %q is not relative to %q: %w", path, root, err)
	}
	if rel == "." {
		return "", fmt.Errorf("photos: %q is the root itself, not a file within it", path)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", ErrOutsideRoot
	}
	return filepath.ToSlash(rel), nil
}
