package photos

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// buildTree creates files from a map of relative path -> contents.
func buildTree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()

	for rel, content := range files {
		full := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

func collect(t *testing.T, root string, opts WalkOptions) ([]Discovered, WalkStats) {
	t.Helper()
	var found []Discovered
	stats, err := Walk(context.Background(), root, opts, func(d Discovered) error {
		found = append(found, d)
		return nil
	})
	if err != nil {
		t.Fatalf("Walk() error = %v", err)
	}
	sort.Slice(found, func(i, j int) bool { return found[i].RelPath < found[j].RelPath })
	return found, stats
}

func TestWalkFindsSupportedImagesRecursively(t *testing.T) {
	root := buildTree(t, map[string]string{
		"top.jpg":                "a",
		"sub/mid.png":            "b",
		"sub/deeper/bottom.webp": "c",
		"sub/deeper/more/x.jpeg": "d",
		"notes.txt":              "e",
		"sub/archive.zip":        "f",
		"sub/deeper/video.mp4":   "g",
	})

	found, stats := collect(t, root, DefaultWalkOptions())

	wantPaths := []string{
		"sub/deeper/bottom.webp",
		"sub/deeper/more/x.jpeg",
		"sub/mid.png",
		"top.jpg",
	}
	if len(found) != len(wantPaths) {
		t.Fatalf("found %d images, want %d: %+v", len(found), len(wantPaths), found)
	}
	for i, want := range wantPaths {
		if found[i].RelPath != want {
			t.Errorf("found[%d].RelPath = %q, want %q", i, found[i].RelPath, want)
		}
	}

	if stats.Supported != 4 {
		t.Errorf("stats.Supported = %d, want 4", stats.Supported)
	}
	if stats.SkippedFormat != 3 {
		t.Errorf("stats.SkippedFormat = %d, want 3 (txt, zip, mp4)", stats.SkippedFormat)
	}
}

// Paths must be forward-slashed regardless of platform, or the same library
// scanned on Windows and Linux produces two disjoint sets of rows.
func TestWalkReturnsForwardSlashRelativePaths(t *testing.T) {
	root := buildTree(t, map[string]string{"a/b/c/deep.jpg": "x"})

	found, _ := collect(t, root, DefaultWalkOptions())
	if len(found) != 1 {
		t.Fatalf("found %d files, want 1", len(found))
	}
	if found[0].RelPath != "a/b/c/deep.jpg" {
		t.Errorf("RelPath = %q, want %q", found[0].RelPath, "a/b/c/deep.jpg")
	}
	if strings.Contains(found[0].RelPath, `\`) {
		t.Error("RelPath contains a backslash; stored paths must be platform-independent")
	}
	if !filepath.IsAbs(found[0].AbsPath) {
		t.Errorf("AbsPath = %q, want an absolute path", found[0].AbsPath)
	}
}

func TestWalkCapturesFileMetadata(t *testing.T) {
	root := buildTree(t, map[string]string{"photo.jpg": "hello world"})

	found, _ := collect(t, root, DefaultWalkOptions())
	if len(found) != 1 {
		t.Fatalf("found %d files, want 1", len(found))
	}
	d := found[0]

	if d.Name != "photo.jpg" {
		t.Errorf("Name = %q, want photo.jpg", d.Name)
	}
	if d.Size != int64(len("hello world")) {
		t.Errorf("Size = %d, want %d", d.Size, len("hello world"))
	}
	if d.Format != FormatJPEG {
		t.Errorf("Format = %q, want jpeg", d.Format)
	}
	if d.ModTime <= 0 {
		t.Errorf("ModTime = %d, want a positive Unix timestamp", d.ModTime)
	}
}

func TestWalkSkipsHiddenFilesAndDirectories(t *testing.T) {
	root := buildTree(t, map[string]string{
		"visible.jpg":           "a",
		".hidden.jpg":           "b",
		".git/objects/blob.jpg": "c",
		".thumbnails/thumb.jpg": "d",
		"sub/.DS_Store":         "e",
	})

	found, stats := collect(t, root, DefaultWalkOptions())

	if len(found) != 1 || found[0].RelPath != "visible.jpg" {
		t.Fatalf("found %+v, want only visible.jpg", found)
	}
	if stats.SkippedHidden == 0 {
		t.Error("stats.SkippedHidden = 0; hidden entries should be counted, not silently dropped")
	}

	// With IncludeHidden the same tree yields more.
	opts := DefaultWalkOptions()
	opts.IncludeHidden = true
	foundAll, _ := collect(t, root, opts)
	if len(foundAll) <= 1 {
		t.Errorf("IncludeHidden found %d files, want more than 1", len(foundAll))
	}
}

// The root must be walked even if the user's photo directory is itself
// dot-prefixed -- otherwise pointing at ~/.photos silently finds nothing.
func TestWalkDoesNotSkipDotPrefixedRoot(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, ".photos")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "a.jpg"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}

	found, _ := collect(t, resolved, DefaultWalkOptions())
	if len(found) != 1 {
		t.Errorf("found %d files in a dot-prefixed root, want 1", len(found))
	}
}

func TestWalkSkipsOversizedFiles(t *testing.T) {
	root := buildTree(t, map[string]string{
		"small.jpg": "tiny",
		"huge.jpg":  strings.Repeat("x", 5000),
	})

	opts := DefaultWalkOptions()
	opts.MaxFileBytes = 1000

	found, stats := collect(t, root, opts)

	if len(found) != 1 || found[0].RelPath != "small.jpg" {
		t.Fatalf("found %+v, want only small.jpg", found)
	}
	if stats.SkippedTooLarge != 1 {
		t.Errorf("stats.SkippedTooLarge = %d, want 1", stats.SkippedTooLarge)
	}
}

func TestWalkHandlesEmptyDirectories(t *testing.T) {
	root := buildTree(t, map[string]string{"a.jpg": "x"})
	if err := os.MkdirAll(filepath.Join(root, "empty", "alsoempty"), 0o755); err != nil {
		t.Fatal(err)
	}

	found, _ := collect(t, root, DefaultWalkOptions())
	if len(found) != 1 {
		t.Errorf("found %d files, want 1; empty directories must not break the walk", len(found))
	}
}

func TestWalkOnEmptyRootReturnsNothingNotAnError(t *testing.T) {
	root := t.TempDir()

	found, stats := collect(t, root, DefaultWalkOptions())
	if len(found) != 0 {
		t.Errorf("found %d files in an empty root, want 0", len(found))
	}
	if stats.Supported != 0 {
		t.Errorf("stats.Supported = %d, want 0", stats.Supported)
	}
}

func TestWalkRejectsMissingOrNonDirectoryRoot(t *testing.T) {
	base := t.TempDir()

	_, err := Walk(context.Background(), filepath.Join(base, "nope"), DefaultWalkOptions(), func(Discovered) error { return nil })
	if err == nil {
		t.Error("walking a nonexistent root succeeded; want an error")
	}

	file := filepath.Join(base, "a.jpg")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Walk(context.Background(), file, DefaultWalkOptions(), func(Discovered) error { return nil }); err == nil {
		t.Error("walking a file as if it were a root succeeded; want an error")
	}
}

// A cancelled scan must stop promptly rather than walking to completion.
func TestWalkStopsOnContextCancellation(t *testing.T) {
	files := map[string]string{}
	for i := 0; i < 200; i++ {
		files[filepath.Join("d", string(rune('a'+i%26))+string(rune('a'+i/26))+".jpg")] = "x"
	}
	root := buildTree(t, files)

	ctx, cancel := context.WithCancel(context.Background())
	seen := 0
	_, err := Walk(ctx, root, DefaultWalkOptions(), func(Discovered) error {
		seen++
		if seen == 5 {
			cancel()
		}
		return nil
	})

	if !errors.Is(err, context.Canceled) {
		t.Errorf("Walk error = %v, want context.Canceled", err)
	}
	if seen > 50 {
		t.Errorf("walk saw %d files after cancelling at 5; it should stop promptly", seen)
	}
}

// An error from the callback is the caller saying stop, which is different
// from the filesystem misbehaving, and must propagate.
func TestWalkPropagatesCallbackError(t *testing.T) {
	root := buildTree(t, map[string]string{"a.jpg": "x", "b.jpg": "y", "c.jpg": "z"})

	sentinel := errors.New("caller said stop")
	_, err := Walk(context.Background(), root, DefaultWalkOptions(), func(Discovered) error {
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Errorf("Walk error = %v, want the callback's error", err)
	}
}

func TestFormatForPathIsCaseInsensitive(t *testing.T) {
	cases := map[string]Format{
		"a.jpg":  FormatJPEG,
		"a.JPG":  FormatJPEG,
		"a.JpEg": FormatJPEG,
		"a.png":  FormatPNG,
		"a.PNG":  FormatPNG,
		"a.webp": FormatWebP,
	}
	for path, want := range cases {
		got, ok := FormatForPath(path)
		if !ok {
			t.Errorf("FormatForPath(%q) reported unsupported", path)
			continue
		}
		if got != want {
			t.Errorf("FormatForPath(%q) = %q, want %q", path, got, want)
		}
	}

	// HEIC is deliberately unsupported; if that changes it should be a
	// conscious decision, not an accident.
	for _, path := range []string{"a.heic", "a.HEIC", "a.mp4", "a.txt", "a", "a.jpg.txt"} {
		if _, ok := FormatForPath(path); ok {
			t.Errorf("FormatForPath(%q) reported supported; it should not be", path)
		}
	}
}
