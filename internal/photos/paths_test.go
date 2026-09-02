package photos

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// setupRoot builds a small tree:
//
//	root/
//	  inside.jpg
//	  sub/nested.jpg
//	outside/secret.txt      (sibling of root, must never be reachable)
func setupRoot(t *testing.T) (root, outside string) {
	t.Helper()
	base := t.TempDir()

	root = filepath.Join(base, "root")
	outside = filepath.Join(base, "outside")

	for _, d := range []string{root, filepath.Join(root, "sub"), outside} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []string{
		filepath.Join(root, "inside.jpg"),
		filepath.Join(root, "sub", "nested.jpg"),
		filepath.Join(outside, "secret.txt"),
	} {
		if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// EvalSymlinks the root so comparisons match what ResolveWithin computes
	// (macOS /var -> /private/var is the classic trap here).
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	return resolved, outside
}

func TestResolveWithinAcceptsPathsInsideRoot(t *testing.T) {
	root, _ := setupRoot(t)

	tests := []struct {
		name   string
		target string
	}{
		{"the root itself", root},
		{"a file at the top level", filepath.Join(root, "inside.jpg")},
		{"a subdirectory", filepath.Join(root, "sub")},
		{"a nested file", filepath.Join(root, "sub", "nested.jpg")},
		{"a relative path", "sub"},
		{"a relative file", "sub/nested.jpg"},
		{"a path that does not exist yet", filepath.Join(root, "future", "photo.jpg")},
		{"redundant separators", filepath.Join(root, "sub") + string(filepath.Separator)},
		{"an interior .. that stays inside", filepath.Join(root, "sub", "..", "inside.jpg")},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ResolveWithin(root, tc.target)
			if err != nil {
				t.Fatalf("ResolveWithin(%q) error = %v, want success", tc.target, err)
			}
			if !strings.HasPrefix(got, root) {
				t.Errorf("resolved to %q, which is not under root %q", got, root)
			}
		})
	}
}

// The security test. Every one of these is a way to read files the user never
// granted access to.
func TestResolveWithinRejectsEscapes(t *testing.T) {
	root, outside := setupRoot(t)

	tests := []struct {
		name   string
		target string
	}{
		{"parent directory", filepath.Join(root, "..")},
		{"sibling directory", outside},
		{"file in a sibling", filepath.Join(outside, "secret.txt")},
		{"relative traversal", "../outside"},
		{"relative traversal to a file", "../outside/secret.txt"},
		{"deep traversal", "sub/../../outside"},
		{"many levels up", "../../../../../../.."},
		{"absolute elsewhere", filepath.Dir(root)},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ResolveWithin(root, tc.target)
			if err == nil {
				t.Fatalf("ResolveWithin(%q) succeeded; it must be rejected", tc.target)
			}
			if !errors.Is(err, ErrOutsideRoot) {
				t.Errorf("error = %v, want ErrOutsideRoot so the API can map it to 400", err)
			}
		})
	}
}

// Prefix confusion: "/tmp/root-evil" shares a string prefix with "/tmp/root"
// but is a different directory. A HasPrefix check would wrongly allow it.
func TestResolveWithinRejectsPrefixConfusion(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "photos")
	evil := filepath.Join(base, "photos-evil")

	for _, d := range []string{root, evil} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(evil, "steal.jpg"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	for _, target := range []string{evil, filepath.Join(evil, "steal.jpg")} {
		if _, err := ResolveWithin(root, target); !errors.Is(err, ErrOutsideRoot) {
			t.Errorf("ResolveWithin(root=%q, %q) error = %v, want ErrOutsideRoot "+
				"-- a string-prefix check would wrongly allow this", root, target, err)
		}
	}
}

// The subtle one: a symlink *inside* the root pointing *outside* it. A check
// that compares unresolved paths passes this and hands over the target.
func TestResolveWithinRejectsSymlinkEscape(t *testing.T) {
	if runtime.GOOS == "windows" {
		// Creating symlinks on Windows needs elevation or developer mode;
		// skipping is honest rather than pretending the case is covered here.
		t.Skip("symlink creation requires elevated privileges on Windows")
	}

	root, outside := setupRoot(t)

	link := filepath.Join(root, "escape")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("cannot create symlink: %v", err)
	}

	for _, target := range []string{link, filepath.Join(link, "secret.txt")} {
		if _, err := ResolveWithin(root, target); !errors.Is(err, ErrOutsideRoot) {
			t.Errorf("ResolveWithin(%q) error = %v, want ErrOutsideRoot "+
				"-- a symlink inside root must not grant access outside it", target, err)
		}
	}
}

// A symlink inside root pointing to another location inside root is fine.
func TestResolveWithinAllowsInternalSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires elevated privileges on Windows")
	}

	root, _ := setupRoot(t)

	link := filepath.Join(root, "shortcut")
	if err := os.Symlink(filepath.Join(root, "sub"), link); err != nil {
		t.Skipf("cannot create symlink: %v", err)
	}

	if _, err := ResolveWithin(root, link); err != nil {
		t.Errorf("ResolveWithin(%q) error = %v; a symlink staying inside root is legitimate", link, err)
	}
}

func TestResolveWithinRejectsEmptyInput(t *testing.T) {
	root, _ := setupRoot(t)

	if _, err := ResolveWithin(root, ""); err == nil {
		t.Error("empty target was accepted")
	}
	if _, err := ResolveWithin(root, "   "); err == nil {
		t.Error("whitespace-only target was accepted")
	}
	if _, err := ResolveWithin("", root); err == nil {
		t.Error("empty root was accepted")
	}
}

// The error must not echo back the resolved path: on a traversal attempt that
// would confirm what does and does not exist outside the root.
func TestResolveWithinErrorDoesNotLeakPaths(t *testing.T) {
	root, outside := setupRoot(t)

	_, err := ResolveWithin(root, filepath.Join(outside, "secret.txt"))
	if err == nil {
		t.Fatal("expected rejection")
	}
	if strings.Contains(err.Error(), "secret.txt") || strings.Contains(err.Error(), outside) {
		t.Errorf("error %q leaks the attempted path", err)
	}
}

func TestRelativeToNormalisesToForwardSlashes(t *testing.T) {
	root := filepath.Join("a", "b")
	target := filepath.Join("a", "b", "c", "d.jpg")

	got, err := RelativeTo(root, target)
	if err != nil {
		t.Fatalf("RelativeTo error = %v", err)
	}
	if got != "c/d.jpg" {
		t.Errorf("RelativeTo = %q, want %q", got, "c/d.jpg")
	}
	if strings.Contains(got, `\`) {
		t.Errorf("RelativeTo returned a backslash path %q; stored paths must be "+
			"platform-independent or the same library rescanned on another OS duplicates every row", got)
	}
}

func TestRelativeToRejectsEscapes(t *testing.T) {
	root := filepath.Join("a", "b")

	if _, err := RelativeTo(root, filepath.Join("a", "other", "x.jpg")); !errors.Is(err, ErrOutsideRoot) {
		t.Errorf("error = %v, want ErrOutsideRoot", err)
	}
	if _, err := RelativeTo(root, root); err == nil {
		t.Error("the root itself is not a file within the root; expected an error")
	}
}
