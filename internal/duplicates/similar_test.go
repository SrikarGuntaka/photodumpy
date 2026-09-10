package duplicates

import (
	"fmt"
	"math/rand"
	"testing"
)

// hashWithBitsFlipped returns base with n low bits flipped, giving a hash at
// exactly Hamming distance n.
func hashWithBitsFlipped(base uint64, n int) uint64 {
	var mask uint64
	for i := 0; i < n; i++ {
		mask |= 1 << uint(i)
	}
	return base ^ mask
}

func ids(g Group) []string { return g.PhotoIDs }

func TestGroupsPhotosWithinThreshold(t *testing.T) {
	base := uint64(0xF0F0F0F0F0F0F0F0)

	candidates := []Candidate{
		{PhotoID: "a", Hash: base, Width: 640, Height: 480, FileSizeBytes: 5000},
		{PhotoID: "b", Hash: hashWithBitsFlipped(base, 3), Width: 640, Height: 480, FileSizeBytes: 4000},
		{PhotoID: "c", Hash: hashWithBitsFlipped(base, 6), Width: 640, Height: 480, FileSizeBytes: 3000},
		// Far away: 40 bits differ.
		{PhotoID: "z", Hash: ^base, Width: 640, Height: 480, FileSizeBytes: 9000},
	}

	groups := GroupSimilar(candidates, 10)
	if len(groups) != 1 {
		t.Fatalf("got %d groups, want 1: %+v", len(groups), groups)
	}
	if len(groups[0].PhotoIDs) != 3 {
		t.Errorf("group has %d members %v, want a, b and c", len(groups[0].PhotoIDs), ids(groups[0]))
	}
	for _, id := range ids(groups[0]) {
		if id == "z" {
			t.Error("the distant photo was grouped with the similar ones")
		}
	}
}

// A photo with no similar partner must not become a group of one.
func TestSingletonsAreNotGroups(t *testing.T) {
	candidates := []Candidate{
		{PhotoID: "a", Hash: 0x0000000000000000},
		{PhotoID: "b", Hash: 0xFFFFFFFFFFFFFFFF},
		{PhotoID: "c", Hash: 0x00000000FFFFFFFF},
	}
	if groups := GroupSimilar(candidates, 5); len(groups) != 0 {
		t.Errorf("got %d groups from three mutually distant photos, want 0", len(groups))
	}
}

func TestEmptyAndSingleInput(t *testing.T) {
	if g := GroupSimilar(nil, 10); g != nil {
		t.Errorf("GroupSimilar(nil) = %+v, want nil", g)
	}
	if g := GroupSimilar([]Candidate{{PhotoID: "a"}}, 10); len(g) != 0 {
		t.Errorf("one candidate produced %d groups, want 0", len(g))
	}
}

// The threshold boundary must be inclusive and exact.
func TestThresholdBoundaryIsInclusive(t *testing.T) {
	base := uint64(0)

	for _, tc := range []struct {
		distance  int
		threshold int
		grouped   bool
	}{
		{5, 5, true},  // exactly at the threshold
		{6, 5, false}, // one past
		{0, 0, true},  // identical hashes at threshold 0
		{1, 0, false}, // one bit apart at threshold 0
	} {
		candidates := []Candidate{
			{PhotoID: "a", Hash: base},
			{PhotoID: "b", Hash: hashWithBitsFlipped(base, tc.distance)},
		}
		groups := GroupSimilar(candidates, tc.threshold)
		got := len(groups) == 1

		if got != tc.grouped {
			t.Errorf("distance %d at threshold %d: grouped=%v, want %v",
				tc.distance, tc.threshold, got, tc.grouped)
		}
	}
}

// THE transitivity property, asserted deliberately rather than left implicit.
//
// A-B is 8, B-C is 8, but A-C is 16 -- beyond the threshold. Connected
// components puts all three together anyway. That is the documented trade-off,
// and MaxDistance is what makes it visible.
func TestTransitiveChainingIsGroupedAndReported(t *testing.T) {
	base := uint64(0)
	a := base
	b := hashWithBitsFlipped(base, 8)
	// Flip 8 different bits so c is 8 from b but 16 from a.
	var highMask uint64
	for i := 8; i < 16; i++ {
		highMask |= 1 << uint(i)
	}
	c := b ^ highMask

	candidates := []Candidate{
		{PhotoID: "a", Hash: a, Width: 100, Height: 100},
		{PhotoID: "b", Hash: b, Width: 100, Height: 100},
		{PhotoID: "c", Hash: c, Width: 100, Height: 100},
	}

	groups := GroupSimilar(candidates, 10)
	if len(groups) != 1 {
		t.Fatalf("got %d groups, want 1 (chained through b)", len(groups))
	}
	g := groups[0]
	if len(g.PhotoIDs) != 3 {
		t.Errorf("group has %d members, want 3", len(g.PhotoIDs))
	}
	if g.MaxDistance <= 10 {
		t.Errorf("MaxDistance = %d, want > 10 -- the point of the field is to reveal that "+
			"the group is wider than the threshold because it chained", g.MaxDistance)
	}
	if g.MaxDistance != 16 {
		t.Errorf("MaxDistance = %d, want 16 (a to c)", g.MaxDistance)
	}
}

// Higher resolution wins, because downscaling is irreversible.
func TestSuggestedKeepPrefersResolution(t *testing.T) {
	base := uint64(0xABCDEF0123456789)

	candidates := []Candidate{
		{PhotoID: "small", Hash: base, Width: 320, Height: 240, FileSizeBytes: 90000},
		{PhotoID: "large", Hash: hashWithBitsFlipped(base, 2), Width: 1920, Height: 1080, FileSizeBytes: 50000},
	}

	groups := GroupSimilar(candidates, 10)
	if len(groups) != 1 {
		t.Fatalf("got %d groups, want 1", len(groups))
	}
	if groups[0].SuggestedKeepID != "large" {
		t.Errorf("suggested keeping %q, want the higher-resolution copy even though the "+
			"smaller one has a bigger file", groups[0].SuggestedKeepID)
	}
	// Best-first ordering.
	if groups[0].PhotoIDs[0] != "large" {
		t.Errorf("PhotoIDs[0] = %q, want the keeper first", groups[0].PhotoIDs[0])
	}
}

// At equal resolution, the bigger file kept more detail.
func TestSuggestedKeepPrefersFileSizeAtEqualResolution(t *testing.T) {
	base := uint64(0x1234567890ABCDEF)

	candidates := []Candidate{
		{PhotoID: "recompressed", Hash: base, Width: 640, Height: 480, FileSizeBytes: 18000},
		{PhotoID: "original", Hash: hashWithBitsFlipped(base, 3), Width: 640, Height: 480, FileSizeBytes: 54000},
	}

	groups := GroupSimilar(candidates, 10)
	if len(groups) != 1 {
		t.Fatalf("got %d groups, want 1", len(groups))
	}
	if groups[0].SuggestedKeepID != "original" {
		t.Errorf("suggested keeping %q, want the larger file at equal resolution",
			groups[0].SuggestedKeepID)
	}
}

// A total ordering means repeated runs give identical suggestions. Without the
// id tiebreak, a user told "keep A" yesterday could be told "keep B" today for
// unchanged data.
func TestSuggestionIsDeterministic(t *testing.T) {
	base := uint64(0x5555555555555555)

	build := func() []Candidate {
		return []Candidate{
			{PhotoID: "p3", Hash: hashWithBitsFlipped(base, 2), Width: 640, Height: 480, FileSizeBytes: 1000},
			{PhotoID: "p1", Hash: base, Width: 640, Height: 480, FileSizeBytes: 1000},
			{PhotoID: "p2", Hash: hashWithBitsFlipped(base, 4), Width: 640, Height: 480, FileSizeBytes: 1000},
		}
	}

	first := GroupSimilar(build(), 10)
	if len(first) != 1 {
		t.Fatalf("got %d groups, want 1", len(first))
	}

	for i := 0; i < 20; i++ {
		// Shuffle the input: grouping must not depend on candidate order.
		c := build()
		rand.New(rand.NewSource(int64(i))).Shuffle(len(c), func(a, b int) { c[a], c[b] = c[b], c[a] })

		g := GroupSimilar(c, 10)
		if len(g) != 1 {
			t.Fatalf("run %d: got %d groups, want 1", i, len(g))
		}
		if g[0].SuggestedKeepID != first[0].SuggestedKeepID {
			t.Errorf("run %d suggested %q, first run suggested %q -- the suggestion must not "+
				"depend on input order", i, g[0].SuggestedKeepID, first[0].SuggestedKeepID)
		}
		for k := range g[0].PhotoIDs {
			if g[0].PhotoIDs[k] != first[0].PhotoIDs[k] {
				t.Errorf("run %d ordering differs at %d: %v vs %v",
					i, k, g[0].PhotoIDs, first[0].PhotoIDs)
			}
		}
	}
}

// Distances are reported relative to the keeper so a user can see why each
// photo is in the group.
func TestDistancesAreReportedFromTheKeeper(t *testing.T) {
	base := uint64(0)
	candidates := []Candidate{
		{PhotoID: "keep", Hash: base, Width: 1000, Height: 1000},
		{PhotoID: "near", Hash: hashWithBitsFlipped(base, 3), Width: 500, Height: 500},
		{PhotoID: "far", Hash: hashWithBitsFlipped(base, 9), Width: 400, Height: 400},
	}

	groups := GroupSimilar(candidates, 10)
	if len(groups) != 1 {
		t.Fatalf("got %d groups, want 1", len(groups))
	}
	g := groups[0]

	if g.SuggestedKeepID != "keep" {
		t.Fatalf("keeper = %q, want keep", g.SuggestedKeepID)
	}
	if g.Distances["keep"] != 0 {
		t.Errorf("the keeper's distance from itself = %d, want 0", g.Distances["keep"])
	}
	if g.Distances["near"] != 3 {
		t.Errorf("near distance = %d, want 3", g.Distances["near"])
	}
	if g.Distances["far"] != 9 {
		t.Errorf("far distance = %d, want 9", g.Distances["far"])
	}
}

// Several independent groups must stay independent.
func TestMultipleDisjointGroups(t *testing.T) {
	var candidates []Candidate
	bases := []uint64{
		0x0000000000000000,
		0x00000000FFFFFFFF,
		0xFFFFFFFF00000000,
	}
	for gi, base := range bases {
		for k := 0; k < 3; k++ {
			candidates = append(candidates, Candidate{
				PhotoID:       fmt.Sprintf("g%d-p%d", gi, k),
				Hash:          hashWithBitsFlipped(base, k*2),
				Width:         640,
				Height:        480,
				FileSizeBytes: int64(1000 - k),
			})
		}
	}

	groups := GroupSimilar(candidates, 8)
	if len(groups) != 3 {
		t.Fatalf("got %d groups, want 3 disjoint ones", len(groups))
	}
	for _, g := range groups {
		if len(g.PhotoIDs) != 3 {
			t.Errorf("group %v has %d members, want 3", g.PhotoIDs, len(g.PhotoIDs))
		}
	}
}

// Larger groups first, so the biggest cleanup wins are at the top.
func TestGroupsAreSortedLargestFirst(t *testing.T) {
	var candidates []Candidate

	// One group of 4, one of 2.
	for k := 0; k < 4; k++ {
		candidates = append(candidates, Candidate{
			PhotoID: fmt.Sprintf("big%d", k),
			Hash:    hashWithBitsFlipped(0x0, k),
			Width:   640, Height: 480,
		})
	}
	for k := 0; k < 2; k++ {
		candidates = append(candidates, Candidate{
			PhotoID: fmt.Sprintf("small%d", k),
			Hash:    hashWithBitsFlipped(0xFFFFFFFFFFFFFFFF, k),
			Width:   640, Height: 480,
		})
	}

	groups := GroupSimilar(candidates, 6)
	if len(groups) != 2 {
		t.Fatalf("got %d groups, want 2", len(groups))
	}
	if len(groups[0].PhotoIDs) < len(groups[1].PhotoIDs) {
		t.Errorf("groups are not sorted largest-first: %d then %d",
			len(groups[0].PhotoIDs), len(groups[1].PhotoIDs))
	}
}

// A threshold beyond the configured maximum must be clamped, not honoured.
// At distance ~32 every photo resembles every other and the feature becomes
// noise.
func TestThresholdIsClamped(t *testing.T) {
	candidates := []Candidate{
		{PhotoID: "a", Hash: 0x0000000000000000, Width: 100, Height: 100},
		{PhotoID: "b", Hash: 0xFFFFFFFFFFFFFFFF, Width: 100, Height: 100}, // 64 apart
	}

	if groups := GroupSimilar(candidates, 999); len(groups) != 0 {
		t.Errorf("a threshold of 999 grouped photos 64 bits apart; it must be clamped "+
			"to the configured maximum, got %d groups", len(groups))
	}
	// A negative threshold means "identical only".
	same := []Candidate{
		{PhotoID: "a", Hash: 42, Width: 100, Height: 100},
		{PhotoID: "b", Hash: 42, Width: 100, Height: 100},
	}
	if groups := GroupSimilar(same, -5); len(groups) != 1 {
		t.Error("a negative threshold should clamp to 0 and still group identical hashes")
	}
}

// The union-find itself, since the grouping relies on it being correct.
func TestUnionFind(t *testing.T) {
	uf := newUnionFind(10)

	for i := 0; i < 10; i++ {
		if uf.find(i) != i {
			t.Errorf("element %d does not start as its own root", i)
		}
	}

	uf.union(0, 1)
	uf.union(1, 2)
	uf.union(7, 8)

	if uf.find(0) != uf.find(2) {
		t.Error("0 and 2 should share a root after 0-1 and 1-2")
	}
	if uf.find(0) == uf.find(7) {
		t.Error("0 and 7 are in different components but share a root")
	}
	if uf.find(3) == uf.find(0) {
		t.Error("untouched element 3 joined a component")
	}

	// Union is idempotent.
	before := uf.find(0)
	uf.union(0, 2)
	if uf.find(0) != before {
		t.Error("re-unioning an already-joined pair changed the root")
	}
}

// The O(n^2) benchmark. Its purpose is to make the crossover point where
// bucketing becomes worthwhile a measured number rather than a guess.
func BenchmarkGroupSimilar(b *testing.B) {
	for _, n := range []int{100, 1000, 5000} {
		b.Run(fmt.Sprintf("n=%d", n), func(b *testing.B) {
			rng := rand.New(rand.NewSource(1))
			candidates := make([]Candidate, n)
			for i := range candidates {
				candidates[i] = Candidate{
					PhotoID:       fmt.Sprintf("p%06d", i),
					Hash:          rng.Uint64(),
					Width:         4000,
					Height:        3000,
					FileSizeBytes: int64(rng.Intn(5_000_000)),
				}
			}

			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				GroupSimilar(candidates, 10)
			}
		})
	}
}

// Byte-identical copies must be ranked by path, not by a UUID lottery.
//
// An exact duplicate is also a near-duplicate at distance 0, so the same files
// appear in both the duplicate view and the similar view. Before the path
// tiebreak, resolution and file size tied and the ordering fell through to
// photo id -- so this view suggested keeping "scene17-copy.jpg" while the
// exact-duplicate view suggested "scene17.jpg". Two features disagreeing about
// the same files, with the arbitrary answer coming from here.
func TestIdenticalCopiesAreRankedByPathNotID(t *testing.T) {
	h := uint64(0xDEADBEEFCAFEBABE)

	// Deliberately ordered so photo-id sorting would pick the wrong one.
	candidates := []Candidate{
		{PhotoID: "aaa-first-uuid", Hash: h, Width: 640, Height: 480,
			FileSizeBytes: 27000, RelativePath: "misc/nested/deep/scene17-copy.jpg"},
		{PhotoID: "zzz-last-uuid", Hash: h, Width: 640, Height: 480,
			FileSizeBytes: 27000, RelativePath: "misc/nested/deep/scene17.jpg"},
		{PhotoID: "mmm-mid-uuid", Hash: h, Width: 640, Height: 480,
			FileSizeBytes: 27000, RelativePath: "misc/nested/deep/scene17 (1).jpg"},
	}

	groups := GroupSimilar(candidates, 10)
	if len(groups) != 1 {
		t.Fatalf("got %d groups, want 1", len(groups))
	}

	// Shortest name at equal depth wins: "scene17.jpg" beats "scene17-copy.jpg"
	// and "scene17 (1).jpg".
	if got := groups[0].SuggestedKeepID; got != "zzz-last-uuid" {
		t.Errorf("suggested keeping %q, want the copy at scene17.jpg -- identical files "+
			"must be ranked by path so this agrees with the exact-duplicate view", got)
	}
}

// Shallower paths win over deeper ones for identical copies.
func TestIdenticalCopiesPreferShallowerPaths(t *testing.T) {
	h := uint64(0x1111222233334444)

	candidates := []Candidate{
		{PhotoID: "deep", Hash: h, Width: 100, Height: 100, FileSizeBytes: 500,
			RelativePath: "a/b/c/photo.jpg"},
		{PhotoID: "shallow", Hash: h, Width: 100, Height: 100, FileSizeBytes: 500,
			RelativePath: "photo.jpg"},
	}

	groups := GroupSimilar(candidates, 10)
	if len(groups) != 1 {
		t.Fatalf("got %d groups, want 1", len(groups))
	}
	if groups[0].SuggestedKeepID != "shallow" {
		t.Errorf("suggested keeping %q, want the shallower path",
			groups[0].SuggestedKeepID)
	}
}

// Resolution still outranks path: a deeply-nested 4K copy beats a top-level
// thumbnail, because path is only a tiebreak for otherwise-equal files.
func TestPathDoesNotOutrankResolution(t *testing.T) {
	base := uint64(0xABCD1234ABCD1234)

	candidates := []Candidate{
		{PhotoID: "thumb", Hash: base, Width: 320, Height: 240, FileSizeBytes: 9000,
			RelativePath: "photo.jpg"},
		{PhotoID: "full", Hash: base ^ 0b11, Width: 3840, Height: 2160, FileSizeBytes: 900,
			RelativePath: "a/b/c/d/e/photo.jpg"},
	}

	groups := GroupSimilar(candidates, 10)
	if len(groups) != 1 {
		t.Fatalf("got %d groups, want 1", len(groups))
	}
	if groups[0].SuggestedKeepID != "full" {
		t.Errorf("suggested keeping %q, want the 4K copy -- path is only a tiebreak for "+
			"files that are otherwise equal", groups[0].SuggestedKeepID)
	}
}

// ---------------------------------------------------------------------------
// Quality-aware ranking (Phase 7)
// ---------------------------------------------------------------------------

func qf(v float64) *float64 { return &v }

// The Phase 7 payoff: a sharp lower-resolution frame beats a blurred
// higher-resolution one. Resolution and byte count describe the container;
// quality describes the image.
func TestQualityOutranksResolution(t *testing.T) {
	base := uint64(0x0F0F0F0F0F0F0F0F)

	candidates := []Candidate{
		{PhotoID: "big-but-blurry", Hash: base, Width: 4000, Height: 3000,
			FileSizeBytes: 8_000_000, RelativePath: "a.jpg", QualityScore: qf(0.21)},
		{PhotoID: "smaller-but-sharp", Hash: base ^ 0b111, Width: 2000, Height: 1500,
			FileSizeBytes: 2_000_000, RelativePath: "b.jpg", QualityScore: qf(0.83)},
	}

	groups := GroupSimilar(candidates, 12)
	if len(groups) != 1 {
		t.Fatalf("got %d groups, want 1", len(groups))
	}
	if groups[0].SuggestedKeepID != "smaller-but-sharp" {
		t.Errorf("suggested keeping %q, want the sharp 3MP frame over the blurred 12MP one -- "+
			"quality describes the image, resolution only describes the container",
			groups[0].SuggestedKeepID)
	}
}

// Quality must only decide when BOTH sides have been measured. Otherwise the
// ranking would turn on whether analysis happened to have run yet.
func TestUnmeasuredQualityFallsBackToResolution(t *testing.T) {
	base := uint64(0x3333333333333333)

	candidates := []Candidate{
		// Measured but mediocre, and small.
		{PhotoID: "small-measured", Hash: base, Width: 640, Height: 480,
			FileSizeBytes: 100, RelativePath: "a.jpg", QualityScore: qf(0.9)},
		// Not yet measured, but much larger.
		{PhotoID: "large-unmeasured", Hash: base ^ 0b11, Width: 4000, Height: 3000,
			FileSizeBytes: 100, RelativePath: "b.jpg"},
	}

	groups := GroupSimilar(candidates, 12)
	if len(groups) != 1 {
		t.Fatalf("got %d groups, want 1", len(groups))
	}
	if groups[0].SuggestedKeepID != "large-unmeasured" {
		t.Errorf("suggested keeping %q; with only one side measured the ranking must fall "+
			"back to resolution rather than rewarding whichever photo was analysed first",
			groups[0].SuggestedKeepID)
	}
}

// Quality differences below the epsilon are noise, not signal, and must not
// override a real resolution difference.
func TestTinyQualityDifferenceDoesNotOverrideResolution(t *testing.T) {
	base := uint64(0x7777777777777777)

	candidates := []Candidate{
		{PhotoID: "small-hair-better", Hash: base, Width: 640, Height: 480,
			FileSizeBytes: 100, RelativePath: "a.jpg", QualityScore: qf(0.805)},
		{PhotoID: "large", Hash: base ^ 0b1, Width: 4000, Height: 3000,
			FileSizeBytes: 100, RelativePath: "b.jpg", QualityScore: qf(0.800)},
	}

	groups := GroupSimilar(candidates, 12)
	if len(groups) != 1 {
		t.Fatalf("got %d groups, want 1", len(groups))
	}
	if groups[0].SuggestedKeepID != "large" {
		t.Errorf("suggested keeping %q; a 0.005 quality difference is below the noise floor "+
			"and must not outweigh a 10x resolution difference", groups[0].SuggestedKeepID)
	}
}

// A clear quality difference above the epsilon must decide.
func TestClearQualityDifferenceDecides(t *testing.T) {
	base := uint64(0x9999999999999999)

	candidates := []Candidate{
		{PhotoID: "good", Hash: base, Width: 640, Height: 480,
			FileSizeBytes: 100, RelativePath: "a.jpg", QualityScore: qf(0.80)},
		{PhotoID: "bad", Hash: base ^ 0b1, Width: 4000, Height: 3000,
			FileSizeBytes: 100, RelativePath: "b.jpg", QualityScore: qf(0.30)},
	}

	groups := GroupSimilar(candidates, 12)
	if groups[0].SuggestedKeepID != "good" {
		t.Errorf("suggested keeping %q, want the measurably better image", groups[0].SuggestedKeepID)
	}
}
