// Package duplicates groups photos that are similar but not identical.
//
// Pure: it takes candidate values and returns groups. No database, no
// filesystem. That keeps the grouping algorithm -- which has genuinely subtle
// behaviour around transitivity -- testable with synthetic hashes and exact
// assertions.
package duplicates

import (
	"sort"

	"github.com/srikarguntaka/photo-organizer/internal/hashing"
)

// Candidate is one photo entering the grouping pass.
type Candidate struct {
	PhotoID string
	Hash    uint64

	// Used to rank which copy to suggest keeping. Zero values are tolerated --
	// a photo whose metadata extraction failed can still be grouped.
	Width         int
	Height        int
	FileSizeBytes int64

	// RelativePath breaks ties between copies that are identical in every
	// measurable way. See the ranking in buildGroup.
	RelativePath string

	// QualityScore is the measured quality of the image itself, 0..1, or nil
	// when the photo has not been analysed. When present it OUTRANKS
	// resolution and file size -- see buildGroup.
	QualityScore *float64
}

// qualityEpsilon is the smallest quality difference treated as meaningful.
//
// Two frames from the same burst can differ in the third decimal place for
// reasons that have nothing to do with which one a person would keep. Below
// this, the comparison falls through to resolution and file size, which are at
// least deterministic properties of the file.
const qualityEpsilon = 0.02

// pathDepth counts directory separators, so a file nearer the library root
// sorts ahead of one buried in a backup folder.
func pathDepth(p string) int {
	n := 0
	for _, c := range p {
		if c == '/' {
			n++
		}
	}
	return n
}

// Pixels reports the photo's resolution, or 0 when unknown.
func (c Candidate) Pixels() int64 { return int64(c.Width) * int64(c.Height) }

// Group is a set of mutually-reachable similar photos.
type Group struct {
	// PhotoIDs, ordered best-first by the ranking below.
	PhotoIDs []string

	// SuggestedKeepID is the photo this system recommends retaining.
	// Nothing is ever deleted; this is a suggestion for the user.
	SuggestedKeepID string

	// MaxDistance is the widest Hamming distance between any two members.
	//
	// Worth surfacing: because grouping is transitive (see GroupSimilar), a
	// group can contain two photos further apart than the threshold, reached
	// through a chain of intermediates. A MaxDistance well above the threshold
	// is the signal that chaining has occurred and the group deserves a
	// sceptical look.
	MaxDistance int

	// Distances holds each member's distance from the suggested-keep photo,
	// so the UI can show why a photo is in the group rather than asserting it.
	Distances map[string]int
}

// GroupSimilar partitions candidates into groups of near-duplicates.
//
// ALGORITHM: pairwise Hamming distance, union-find, connected components.
//
// TRANSITIVITY IS THE INTERESTING PART, and it is a real trade-off rather than
// an oversight. Similarity within a threshold is NOT transitive: A and B may be
// 9 apart, B and C 9 apart, while A and C are 18 apart. Connected components
// puts all three in one group anyway.
//
// The alternative is to require every pair in a group to be within the
// threshold (a clique), which is exact but has two problems: finding maximal
// cliques is NP-hard, and it splits genuine burst sequences, where each frame
// resembles its neighbours but the first and last frame differ substantially.
// A user with a 20-shot burst wants one group, not fourteen overlapping ones.
//
// So: connected components, with the chaining risk mitigated rather than
// hidden. The threshold is deliberately tight (12 of 64 bits), MaxDistance is
// reported per group so drift is visible, and every member's distance from the
// suggested keeper is exposed. On the fixture corpus, unrelated photos average
// 30 bits apart, so a chain would need several improbable intermediate hops.
//
// COMPLEXITY: O(n^2) comparisons. Each is an XOR and a popcount -- two
// instructions -- so 10,000 photos is 50M operations, on the order of a second.
// The bucketing optimisation is designed in DESIGN_DECISIONS.md and
// deliberately not built: it would be optimising a bottleneck nobody has
// measured. BenchmarkGroupSimilar exists so the crossover point can be found
// with data when it matters.
func GroupSimilar(candidates []Candidate, threshold int) []Group {
	if threshold < 0 {
		threshold = 0
	}
	if threshold > hashing.MaxSimilarityThreshold {
		threshold = hashing.MaxSimilarityThreshold
	}
	if len(candidates) < 2 {
		return nil
	}

	uf := newUnionFind(len(candidates))

	// The O(n^2) pass. j starts at i+1 because distance is symmetric.
	for i := 0; i < len(candidates); i++ {
		for j := i + 1; j < len(candidates); j++ {
			if hashing.HammingDistance(candidates[i].Hash, candidates[j].Hash) <= threshold {
				uf.union(i, j)
			}
		}
	}

	members := map[int][]int{}
	for i := range candidates {
		root := uf.find(i)
		members[root] = append(members[root], i)
	}

	out := make([]Group, 0, len(members))
	for _, idxs := range members {
		// A component of one is not a duplicate group.
		if len(idxs) < 2 {
			continue
		}
		out = append(out, buildGroup(candidates, idxs))
	}

	// Largest groups first: they are the biggest cleanup wins and what a user
	// reviewing suggestions wants at the top. Ties broken on the keeper's id so
	// repeated runs produce identical ordering.
	sort.Slice(out, func(a, b int) bool {
		if len(out[a].PhotoIDs) != len(out[b].PhotoIDs) {
			return len(out[a].PhotoIDs) > len(out[b].PhotoIDs)
		}
		return out[a].SuggestedKeepID < out[b].SuggestedKeepID
	})
	return out
}

// buildGroup ranks a component and measures its spread.
func buildGroup(candidates []Candidate, idxs []int) Group {
	// RANKING. Unlike exact duplicates -- where every copy is byte-identical
	// and the only question is which path to keep -- near-duplicates genuinely
	// differ, so this is a choice about which IMAGE is better:
	//
	//   1. Measured quality, when BOTH candidates have been analysed AND their
	//      resolutions are comparable. This describes the image; resolution and
	//      byte count only describe its container. A sharp 8MP frame genuinely
	//      beats a blurred 12MP one, and the earlier ranking could not tell.
	//
	//      Three guards, each earned:
	//
	//      Both sides measured -- otherwise the ranking turns on whether
	//      analysis happened to have run yet rather than on the photos.
	//
	//      Difference above an epsilon -- two frames from the same burst can
	//      differ in the third decimal for no meaningful reason.
	//
	//      (A third guard briefly lived here, restricting quality comparisons
	//      to photos of similar resolution. It existed because sharpness was
	//      being measured on a denser grid for small images, so a thumbnail
	//      measured sharper than its own source. That was a defect in the
	//      measurement, not a fact about images, and it is fixed in
	//      internal/quality by scaling every image to a common size. Guarding
	//      here would have papered over it -- and would have kept a blurred
	//      12MP frame over a sharp 3MP one, which is exactly backwards.)
	//   2. Resolution. More pixels is more information, and downscaling is
	//      lossy in a way that cannot be undone.
	//   3. File size at equal resolution, as a proxy for compression quality.
	//      A 40-quality JPEG and a 92-quality JPEG of the same scene have the
	//      same dimensions; the larger file kept more detail.
	//   4. Path shape -- shallowest directory, then shortest name. This only
	//      matters when the copies are byte-identical, which happens because
	//      an exact duplicate is also a near-duplicate at distance 0. Without
	//      it the tiebreak fell through to a UUID, and this view would suggest
	//      keeping "scene17-copy.jpg" while the exact-duplicate view suggested
	//      "scene17.jpg" -- two features disagreeing about the same files, with
	//      the arbitrary answer coming from here. Matching Phase 4's heuristic
	//      makes them agree and picks the copy that looks like the original.
	//   5. Photo id, so the ordering is total and a rerun over unchanged data
	//      produces the same suggestion.
	//
	sorted := make([]int, len(idxs))
	copy(sorted, idxs)

	sort.Slice(sorted, func(a, b int) bool {
		ca, cb := candidates[sorted[a]], candidates[sorted[b]]

		if ca.QualityScore != nil && cb.QualityScore != nil {
			qa, qb := *ca.QualityScore, *cb.QualityScore
			if diff := qa - qb; diff > qualityEpsilon || diff < -qualityEpsilon {
				return qa > qb
			}
		}

		if ca.Pixels() != cb.Pixels() {
			return ca.Pixels() > cb.Pixels()
		}
		if ca.FileSizeBytes != cb.FileSizeBytes {
			return ca.FileSizeBytes > cb.FileSizeBytes
		}
		if da, db := pathDepth(ca.RelativePath), pathDepth(cb.RelativePath); da != db {
			return da < db
		}
		if len(ca.RelativePath) != len(cb.RelativePath) {
			return len(ca.RelativePath) < len(cb.RelativePath)
		}
		if ca.RelativePath != cb.RelativePath {
			return ca.RelativePath < cb.RelativePath
		}
		return ca.PhotoID < cb.PhotoID
	})

	keeper := candidates[sorted[0]]

	g := Group{
		PhotoIDs:        make([]string, 0, len(sorted)),
		SuggestedKeepID: keeper.PhotoID,
		Distances:       make(map[string]int, len(sorted)),
	}
	for _, i := range sorted {
		g.PhotoIDs = append(g.PhotoIDs, candidates[i].PhotoID)
		g.Distances[candidates[i].PhotoID] = hashing.HammingDistance(keeper.Hash, candidates[i].Hash)
	}

	// Widest pair in the group, which is what reveals chaining.
	for a := 0; a < len(sorted); a++ {
		for b := a + 1; b < len(sorted); b++ {
			d := hashing.HammingDistance(candidates[sorted[a]].Hash, candidates[sorted[b]].Hash)
			if d > g.MaxDistance {
				g.MaxDistance = d
			}
		}
	}
	return g
}

// unionFind is a disjoint-set structure with path compression and union by
// rank, giving effectively constant-time operations. It is the standard way to
// build connected components in one pass without materialising the graph.
type unionFind struct {
	parent []int
	rank   []int
}

func newUnionFind(n int) *unionFind {
	uf := &unionFind{parent: make([]int, n), rank: make([]int, n)}
	for i := range uf.parent {
		uf.parent[i] = i
	}
	return uf
}

// find returns the representative of x's set, flattening the path as it goes so
// later lookups are cheaper.
func (u *unionFind) find(x int) int {
	for u.parent[x] != x {
		u.parent[x] = u.parent[u.parent[x]] // path halving
		x = u.parent[x]
	}
	return x
}

// union merges the sets containing a and b, attaching the shallower tree to the
// deeper one so depth grows logarithmically at worst.
func (u *unionFind) union(a, b int) {
	ra, rb := u.find(a), u.find(b)
	if ra == rb {
		return
	}
	if u.rank[ra] < u.rank[rb] {
		ra, rb = rb, ra
	}
	u.parent[rb] = ra
	if u.rank[ra] == u.rank[rb] {
		u.rank[ra]++
	}
}
