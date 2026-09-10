-- 0006_similar: perceptual hashes and near-duplicate groups.

-- ---------------------------------------------------------------------------
-- The perceptual hash.
--
-- bigint, not bytea or text: a 64-bit dHash fits exactly, so Hamming distance
-- is expressible in SQL as popcount(a # b) if it is ever needed server-side,
-- and the column is 8 bytes rather than 16 (hex) or 64 (a bit string).
--
-- Postgres has no unsigned integer type, so the Go uint64 is stored as its
-- two's-complement int64. That is a lossless round trip -- the bit pattern is
-- preserved and Hamming distance is computed on bits, not magnitude -- but it
-- does mean the values look like meaningless negative numbers in psql. The
-- API renders them as hex.
-- ---------------------------------------------------------------------------
ALTER TABLE photos
    ADD COLUMN phash bigint,
    -- Idempotency marker, mirroring hashed_at and metadata_extracted_at.
    ADD COLUMN phashed_at timestamptz;

-- Drives the perceptual hashing pass. Partial on the pending set so it shrinks
-- as work completes.
CREATE INDEX photos_needs_phash_idx
    ON photos (library_id, id)
    WHERE phashed_at IS NULL;

-- The grouping pass selects every hashed photo in a library. There is no index
-- on phash itself, and that is deliberate: similarity is Hamming distance, not
-- equality or range, so a B-tree cannot help. Two hashes one bit apart can sit
-- at opposite ends of the integer ordering. The bucketing scheme described in
-- DESIGN_DECISIONS.md is what would make this indexable, and it is not built
-- because the O(n^2) scan is 80ms at 10,000 photos.
CREATE INDEX photos_phash_scan_idx
    ON photos (library_id)
    WHERE phash IS NOT NULL;

-- ---------------------------------------------------------------------------
-- similar_groups: sets of visually similar, non-identical photos.
--
-- Separate from duplicate_groups rather than a shared table with a "kind"
-- column, because the two are genuinely different things:
--
--   duplicate_groups  identity is a shared sha256; membership is exact;
--                     every member is byte-identical, so "which to keep" is
--                     a choice of PATH.
--   similar_groups    identity is emergent from a threshold; membership is
--                     approximate and changes if the threshold changes; the
--                     members genuinely differ, so "which to keep" is a
--                     choice of IMAGE.
--
-- Forcing them into one table would mean a sha256 column that is null half the
-- time and a max_distance column that is meaningless the other half.
-- ---------------------------------------------------------------------------
CREATE TABLE similar_groups (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    library_id uuid NOT NULL REFERENCES libraries(id) ON DELETE CASCADE,

    photo_count integer NOT NULL CHECK (photo_count >= 2),

    -- The threshold this group was built with. Recorded because the group is
    -- only meaningful relative to it -- rebuilding with a different threshold
    -- produces different groups, and a stored group with no record of its
    -- threshold would be uninterpretable.
    threshold integer NOT NULL CHECK (threshold >= 0),

    -- Widest Hamming distance between any two members. Because grouping is
    -- transitive, this can EXCEED the threshold when photos were chained
    -- through intermediates. A value well above the threshold is the signal
    -- that a group drifted and deserves a sceptical look.
    max_distance integer NOT NULL CHECK (max_distance >= 0),

    -- Bytes freed by keeping only the suggested photo. A suggestion, not an
    -- action: nothing is ever deleted.
    reclaimable_bytes bigint NOT NULL CHECK (reclaimable_bytes >= 0),

    suggested_keep_photo_id uuid REFERENCES photos(id) ON DELETE SET NULL,

    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

-- Biggest groups first is what the review UI wants.
CREATE INDEX similar_groups_size_idx
    ON similar_groups (library_id, photo_count DESC, reclaimable_bytes DESC);

CREATE TRIGGER similar_groups_set_updated_at
    BEFORE UPDATE ON similar_groups
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- ---------------------------------------------------------------------------
-- similar_group_members
-- ---------------------------------------------------------------------------
CREATE TABLE similar_group_members (
    group_id uuid NOT NULL REFERENCES similar_groups(id) ON DELETE CASCADE,
    photo_id uuid NOT NULL REFERENCES photos(id) ON DELETE CASCADE,

    -- Hamming distance from this group's suggested-keep photo. Stored so the
    -- UI can show WHY a photo is in the group rather than asserting that it
    -- belongs, which is the difference between an explainable suggestion and
    -- an opaque one.
    distance integer NOT NULL CHECK (distance >= 0),

    -- Position in the keep-ranking, 1 being the suggested keeper.
    rank integer NOT NULL CHECK (rank >= 1),

    PRIMARY KEY (group_id, photo_id)
);

-- A photo can belong to an exact-duplicate group AND a similar group, which is
-- why membership lives in join tables rather than as a column on photos.
CREATE INDEX similar_group_members_photo_idx
    ON similar_group_members (photo_id);

-- Members are read in rank order.
CREATE INDEX similar_group_members_rank_idx
    ON similar_group_members (group_id, rank);
