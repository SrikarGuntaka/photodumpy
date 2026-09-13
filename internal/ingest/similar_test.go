//go:build integration

package ingest

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/srikarguntaka/photo-organizer/internal/fixtures"
	"github.com/srikarguntaka/photo-organizer/internal/hashing"
	"github.com/srikarguntaka/photo-organizer/internal/store"
)

// similarLibrary builds, scans, hashes and perceptually hashes a corpus.
// mutate runs after generation and before the scan, so a test can plant files.
func similarLibrary(t *testing.T, st *store.Store, mutate func(root string, m *fixtures.Manifest)) (string, *fixtures.Manifest) {
	t.Helper()
	ctx := context.Background()

	root := t.TempDir()
	m, err := fixtures.Generate(fixtures.Options{Root: root, Seed: 1, Scenes: 24})
	if err != nil {
		t.Fatalf("generating corpus: %v", err)
	}
	if mutate != nil {
		mutate(root, m)
	}
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}

	lib := newLibrary(t, st, resolved)
	if _, err := newScanner(t, st).ScanSync(ctx, lib); err != nil {
		t.Fatalf("scanning: %v", err)
	}
	p := newProcessor(t, st)
	// File hashes FIRST: similarity candidates are collapsed by sha256, so
	// grouping before they exist would leave copies uncollapsed.
	if _, err := p.HashLibrary(ctx, lib); err != nil {
		t.Fatalf("hashing: %v", err)
	}
	if _, err := p.PHashLibrary(ctx, lib, hashing.DefaultSimilarityThreshold); err != nil {
		t.Fatalf("perceptual hashing: %v", err)
	}
	return lib.ID, m
}

// The mirror of TestNearDuplicatesAreNotReportedAsExact: byte-identical copies
// must not be reported as a NEAR-duplicate group.
//
// Regression. The API once reported 9 near-duplicate groups on this corpus
// where the generator built 6 -- the other 3 were the exact-duplicate groups,
// whose copies sit at Hamming distance 0 from each other. The review UI then
// described three identical files as "visually similar but not identical", and
// counted their reclaimable bytes on both tabs.
func TestExactCopiesAreNotReportedAsNearDuplicates(t *testing.T) {
	st := store.New(testPool(t))
	libID, m := similarLibrary(t, st, nil)

	groups, err := st.ListSimilarGroups(context.Background(), libID, 200, 0)
	if err != nil {
		t.Fatal(err)
	}

	if want := m.Summary.NearDuplicateGroups; len(groups) != want {
		t.Errorf("found %d near-duplicate groups, corpus was built with %d", len(groups), want)
	}

	dupGroupOf := map[string]string{}
	for _, f := range m.Files {
		if f.DuplicateGroup != "" {
			dupGroupOf[f.RelPath] = f.DuplicateGroup
		}
	}
	for _, g := range groups {
		seen := map[string]string{}
		for _, p := range g.Photos {
			dg, ok := dupGroupOf[p.RelativePath]
			if !ok {
				continue
			}
			if other, dup := seen[dg]; dup {
				t.Errorf("near-duplicate group contains two byte-identical copies: %s and %s",
					other, p.RelativePath)
			}
			seen[dg] = p.RelativePath
		}
	}
}

// When a file is BOTH exactly duplicated and has a near-duplicate variant, the
// near-duplicate group must contain exactly one of the identical copies -- and
// it must be the same copy the exact-duplicate group suggests keeping, so the
// two Review tabs never recommend keeping different copies of the same bytes.
//
// The corpus has no such file, so this test makes one: it copies a
// near-duplicate into two new locations, one shallower and one deeper.
func TestRepresentativeIsTheExactDuplicateKeeper(t *testing.T) {
	st := store.New(testPool(t))
	ctx := context.Background()

	var source string
	libID, _ := similarLibrary(t, st, func(root string, m *fixtures.Manifest) {
		for _, f := range m.Files {
			// A near-duplicate that is not already exactly duplicated.
			if f.SimilarGroup != "" && f.DuplicateGroup == "" {
				source = f.RelPath
				break
			}
		}
		if source == "" {
			t.Fatal("corpus has no near-duplicate to copy")
		}
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(source)))
		if err != nil {
			t.Fatal(err)
		}
		// "top.jpg" is at the root: fewer separators than the source, so the
		// keeper rule must pick it over the original location.
		for _, rel := range []string{"top.jpg", "backup/old/deep/copy.jpg"} {
			dst := filepath.Join(root, filepath.FromSlash(rel))
			if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(dst, data, 0o644); err != nil {
				t.Fatal(err)
			}
		}
	})

	exact, err := st.ListDuplicateGroups(ctx, libID, 200, 0)
	if err != nil {
		t.Fatal(err)
	}
	var keeper string
	copies := map[string]bool{}
	for _, g := range exact {
		for _, p := range g.Photos {
			if p.RelativePath == source {
				for _, q := range g.Photos {
					copies[q.RelativePath] = true
					if q.SuggestedKeep {
						keeper = q.RelativePath
					}
				}
			}
		}
	}
	if len(copies) != 3 {
		t.Fatalf("expected an exact-duplicate group of 3 copies of %s, got %v", source, copies)
	}
	if keeper != "top.jpg" {
		t.Fatalf("exact keeper = %q, want the shallowest copy top.jpg", keeper)
	}

	similar, err := st.ListSimilarGroups(ctx, libID, 200, 0)
	if err != nil {
		t.Fatal(err)
	}
	var found []string
	for _, g := range similar {
		for _, p := range g.Photos {
			if copies[p.RelativePath] {
				found = append(found, p.RelativePath)
			}
		}
	}
	if len(found) != 1 {
		t.Fatalf("near-duplicate groups contain %d copies of the same file %v, want exactly 1", len(found), found)
	}
	if found[0] != keeper {
		t.Errorf("near-duplicate group represents the file with %q, but the exact-duplicate group suggests keeping %q",
			found[0], keeper)
	}
}
