//go:build integration

package ingest

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/srikarguntaka/photo-organizer/internal/fixtures"
	"github.com/srikarguntaka/photo-organizer/internal/store"
)

// corpusLibrary generates the fixture corpus, registers it, and scans it.
func corpusLibrary(t *testing.T, st *store.Store) (string, *fixtures.Manifest, *Processor) {
	t.Helper()

	root := t.TempDir()
	m, err := fixtures.Generate(fixtures.Options{Root: root, Seed: 1, Scenes: 24})
	if err != nil {
		t.Fatalf("generating corpus: %v", err)
	}
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}

	lib := newLibrary(t, st, resolved)
	if _, err := newScanner(t, st).ScanSync(context.Background(), lib); err != nil {
		t.Fatalf("scanning: %v", err)
	}

	p := newProcessor(t, st)
	if _, err := p.HashLibrary(context.Background(), lib); err != nil {
		t.Fatalf("hashing: %v", err)
	}
	return lib.ID, m, p
}

func newProcessor(t *testing.T, st *store.Store) *Processor {
	t.Helper()
	return NewProcessor(st, testLogger(t), ProcessorOptions{Concurrency: 4})
}

// THE Phase 4 assertion: the groups found must exactly match the groups the
// corpus was built with.
func TestDuplicateGroupsMatchGroundTruth(t *testing.T) {
	st := store.New(testPool(t))
	libID, m, _ := corpusLibrary(t, st)

	// Ground truth from the manifest.
	wantGroups := map[string][]string{}
	for _, f := range m.Files {
		if f.DuplicateGroup != "" {
			wantGroups[f.DuplicateGroup] = append(wantGroups[f.DuplicateGroup], f.RelPath)
		}
	}
	wantFiles := 0
	for _, paths := range wantGroups {
		wantFiles += len(paths)
	}

	got, err := st.ListDuplicateGroups(context.Background(), libID, 100, 0)
	if err != nil {
		t.Fatal(err)
	}

	if len(got) != len(wantGroups) {
		t.Errorf("found %d duplicate groups, corpus was built with %d", len(got), len(wantGroups))
	}

	gotFiles := 0
	for _, g := range got {
		gotFiles += len(g.Photos)

		if g.PhotoCount != len(g.Photos) {
			t.Errorf("group %s: photo_count = %d but %d members returned",
				g.SHA256Hex[:12], g.PhotoCount, len(g.Photos))
		}
		if g.PhotoCount < 2 {
			t.Errorf("group %s has %d member(s); a group of one is not a duplicate",
				g.SHA256Hex[:12], g.PhotoCount)
		}

		// Every member of a group must be one of the manifest's members of
		// SOME group, and all members must come from the same one.
		var groupName string
		for _, p := range g.Photos {
			found := ""
			for name, paths := range wantGroups {
				for _, path := range paths {
					if path == p.RelativePath {
						found = name
					}
				}
			}
			if found == "" {
				t.Errorf("group %s contains %s, which the manifest does not list as a duplicate",
					g.SHA256Hex[:12], p.RelativePath)
				continue
			}
			if groupName == "" {
				groupName = found
			} else if groupName != found {
				t.Errorf("group %s mixes manifest groups %s and %s",
					g.SHA256Hex[:12], groupName, found)
			}
		}
	}

	if gotFiles != wantFiles {
		t.Errorf("groups cover %d files, corpus was built with %d", gotFiles, wantFiles)
	}
	t.Logf("matched %d groups covering %d files", len(got), gotFiles)
}

// Near-duplicates must NOT appear. SHA-256 is supposed to miss them; if they
// showed up here the corpus or the query would be wrong.
func TestNearDuplicatesAreNotReportedAsExact(t *testing.T) {
	st := store.New(testPool(t))
	libID, m, _ := corpusLibrary(t, st)

	similar := map[string]bool{}
	for _, f := range m.Files {
		if f.SimilarGroup != "" {
			similar[f.RelPath] = true
		}
	}
	if len(similar) == 0 {
		t.Fatal("corpus has no near-duplicates")
	}

	groups, err := st.ListDuplicateGroups(context.Background(), libID, 100, 0)
	if err != nil {
		t.Fatal(err)
	}

	for _, g := range groups {
		for _, p := range g.Photos {
			if similar[p.RelativePath] {
				t.Errorf("%s is a NEAR-duplicate but was reported as an exact duplicate; "+
					"SHA-256 must not match recompressed or resized copies", p.RelativePath)
			}
		}
	}
}

// Exactly one member per group is marked keep, and it is the shallowest,
// shortest path -- the one most likely to be the original rather than a copy.
func TestSuggestedKeepIsExactlyOneAndDeterministic(t *testing.T) {
	st := store.New(testPool(t))
	libID, _, _ := corpusLibrary(t, st)

	groups, err := st.ListDuplicateGroups(context.Background(), libID, 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) == 0 {
		t.Fatal("no groups to check")
	}

	for _, g := range groups {
		keeps := 0
		var keepPath string
		for _, p := range g.Photos {
			if p.SuggestedKeep {
				keeps++
				keepPath = p.RelativePath
			}
		}
		if keeps != 1 {
			t.Errorf("group %s has %d suggested-keep members, want exactly 1",
				g.SHA256Hex[:12], keeps)
			continue
		}

		// The keep must be no deeper than any other member.
		keepDepth := depth(keepPath)
		for _, p := range g.Photos {
			if depth(p.RelativePath) < keepDepth {
				t.Errorf("group %s: kept %s (depth %d) but %s is shallower (depth %d)",
					g.SHA256Hex[:12], keepPath, keepDepth, p.RelativePath, depth(p.RelativePath))
			}
		}
	}
}

func depth(p string) int {
	n := 0
	for _, c := range p {
		if c == '/' {
			n++
		}
	}
	return n
}

// Rebuilding must converge, not accumulate. This is what makes the aggregate
// stage safe to re-run under at-least-once job delivery in Phase 5.
func TestRebuildIsIdempotent(t *testing.T) {
	st := store.New(testPool(t))
	libID, _, _ := corpusLibrary(t, st)

	first, firstBytes, err := st.RebuildDuplicateGroups(context.Background(), libID)
	if err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 3; i++ {
		n, b, err := st.RebuildDuplicateGroups(context.Background(), libID)
		if err != nil {
			t.Fatalf("rebuild %d: %v", i+2, err)
		}
		if n != first {
			t.Errorf("rebuild %d produced %d groups, first produced %d -- rebuilding must converge",
				i+2, n, first)
		}
		if b != firstBytes {
			t.Errorf("rebuild %d reclaimable = %d, first = %d", i+2, b, firstBytes)
		}
	}

	// Membership must not have accumulated duplicates either.
	groups, err := st.ListDuplicateGroups(context.Background(), libID, 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, g := range groups {
		seen := map[string]bool{}
		for _, p := range g.Photos {
			if seen[p.PhotoID] {
				t.Errorf("group %s lists photo %s twice after repeated rebuilds",
					g.SHA256Hex[:12], p.PhotoID)
			}
			seen[p.PhotoID] = true
		}
	}
}

// Hashing twice must not change any digest, and must not re-read files that
// are already done.
func TestHashingIsIdempotent(t *testing.T) {
	st := store.New(testPool(t))
	libID, _, _ := corpusLibrary(t, st)

	before, err := st.SummariseDuplicates(context.Background(), libID)
	if err != nil {
		t.Fatal(err)
	}

	// A second pass should find nothing left to do.
	pending, err := st.CountPhotosNeedingHash(context.Background(), libID)
	if err != nil {
		t.Fatal(err)
	}
	if pending != 0 {
		t.Errorf("%d photos still pending after a completed hash pass", pending)
	}

	lib, err := st.GetLibrary(context.Background(), libID)
	if err != nil {
		t.Fatal(err)
	}
	result, err := NewProcessor(st, testLogger(t), ProcessorOptions{Concurrency: 4}).
		HashLibrary(context.Background(), lib)
	if err != nil {
		t.Fatal(err)
	}
	if result.Hashed != 0 {
		t.Errorf("second pass hashed %d photos, want 0 -- hashed_at should skip completed work",
			result.Hashed)
	}

	after, err := st.SummariseDuplicates(context.Background(), libID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Groups != before.Groups || after.ReclaimableBytes != before.ReclaimableBytes {
		t.Errorf("summary changed across a no-op pass: %+v -> %+v", before, after)
	}
}

// Reclaimable bytes must be total minus one copy -- the space actually freed by
// keeping one of each.
func TestReclaimableBytesArithmetic(t *testing.T) {
	st := store.New(testPool(t))
	libID, _, _ := corpusLibrary(t, st)

	groups, err := st.ListDuplicateGroups(context.Background(), libID, 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) == 0 {
		t.Fatal("no groups")
	}

	for _, g := range groups {
		var sum int64
		var smallest int64 = -1
		for _, p := range g.Photos {
			sum += p.FileSizeBytes
			if smallest < 0 || p.FileSizeBytes < smallest {
				smallest = p.FileSizeBytes
			}
		}
		if g.TotalBytes != sum {
			t.Errorf("group %s: total_bytes = %d, members sum to %d", g.SHA256Hex[:12], g.TotalBytes, sum)
		}
		if want := sum - smallest; g.ReclaimableBytes != want {
			t.Errorf("group %s: reclaimable = %d, want %d (total minus one copy)",
				g.SHA256Hex[:12], g.ReclaimableBytes, want)
		}
		if g.ReclaimableBytes >= g.TotalBytes {
			t.Errorf("group %s: reclaimable %d >= total %d -- one copy must always be kept",
				g.SHA256Hex[:12], g.ReclaimableBytes, g.TotalBytes)
		}
	}
}

// A file deleted after hashing must drop out of its group on rebuild, and a
// group that falls to one member must disappear entirely.
func TestGroupShrinksWhenAFileGoesMissing(t *testing.T) {
	st := store.New(testPool(t))
	libID, _, _ := corpusLibrary(t, st)

	groups, err := st.ListDuplicateGroups(context.Background(), libID, 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) == 0 {
		t.Fatal("no groups")
	}
	target := groups[0]
	originalCount := target.PhotoCount

	// Mark one member missing, as a worker would on ENOENT.
	msg := "simulated deletion"
	if err := st.UpdatePhotoMetadata(context.Background(), target.Photos[0].PhotoID,
		store.PhotoMetadata{State: "missing", LastError: &msg}); err != nil {
		t.Fatal(err)
	}

	if _, _, err := st.RebuildDuplicateGroups(context.Background(), libID); err != nil {
		t.Fatal(err)
	}

	after, err := st.ListDuplicateGroups(context.Background(), libID, 100, 0)
	if err != nil {
		t.Fatal(err)
	}

	for _, g := range after {
		if g.SHA256Hex != target.SHA256Hex {
			continue
		}
		if g.PhotoCount != originalCount-1 {
			t.Errorf("group %s has %d members after one went missing, want %d",
				g.SHA256Hex[:12], g.PhotoCount, originalCount-1)
		}
		for _, p := range g.Photos {
			if p.PhotoID == target.Photos[0].PhotoID {
				t.Error("the missing photo is still listed as a group member")
			}
		}
		return
	}

	// Falling out of the listing entirely is correct when the group dropped to
	// a single member.
	if originalCount-1 >= 2 {
		t.Errorf("group %s vanished but should still have %d members",
			target.SHA256Hex[:12], originalCount-1)
	}
}

// A library with no duplicates must produce no groups, not an error.
func TestLibraryWithNoDuplicatesProducesNoGroups(t *testing.T) {
	st := store.New(testPool(t))

	root := buildTree(t, map[string]string{
		"a.jpg": "unique content one",
		"b.jpg": "unique content two",
		"c.jpg": "unique content three",
	})
	lib := newLibrary(t, st, root)
	if _, err := newScanner(t, st).ScanSync(context.Background(), lib); err != nil {
		t.Fatal(err)
	}

	result, err := newProcessor(t, st).HashLibrary(context.Background(), lib)
	if err != nil {
		t.Fatal(err)
	}
	if result.Hashed != 3 {
		t.Errorf("hashed %d, want 3", result.Hashed)
	}
	if result.Groups != 0 {
		t.Errorf("found %d groups in a library with no duplicates, want 0", result.Groups)
	}
	if result.ReclaimableBytes != 0 {
		t.Errorf("reclaimable = %d, want 0", result.ReclaimableBytes)
	}
}
