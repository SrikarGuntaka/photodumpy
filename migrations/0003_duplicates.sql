-- 0003_duplicates: content hashing and exact-duplicate grouping.

-- ---------------------------------------------------------------------------
-- The content hash.
--
-- bytea, not hex text: 32 bytes instead of 64, and B-tree comparison on
-- fixed-width binary rather than character data. At 100,000 photos that is 3MB
-- of index instead of 6MB, and every comparison is a memcmp.
-- ---------------------------------------------------------------------------
ALTER TABLE photos
    ADD COLUMN sha256 bytea CHECK (sha256 IS NULL OR length(sha256) = 32),
    -- Idempotency marker, mirroring metadata_extracted_at. Set on completion
    -- whether or not a hash was produced, so an unreadable file is not retried
    -- forever.
    ADD COLUMN hashed_at timestamptz;

-- THE duplicate-detection index. The grouping query is
-- GROUP BY sha256 HAVING count(*) > 1, scoped to a library.
CREATE INDEX photos_sha256_idx
    ON photos (library_id, sha256)
    WHERE sha256 IS NOT NULL;

-- Drives the hashing pass. Partial on the pending set, so it shrinks as work
-- completes rather than growing with the library.
CREATE INDEX photos_needs_hash_idx
    ON photos (library_id, id)
    WHERE hashed_at IS NULL;

-- ---------------------------------------------------------------------------
-- duplicate_groups: one row per set of byte-identical files.
--
-- This is DERIVED data -- it can always be rebuilt from photos.sha256 -- and it
-- is stored anyway because the UI needs to page through groups, and recomputing
-- a GROUP BY on every request would not survive a large library.
--
-- The tradeoff is that it can go stale after a rescan, which is why rebuilding
-- is cheap, idempotent, and re-run rather than incrementally patched.
-- ---------------------------------------------------------------------------
CREATE TABLE duplicate_groups (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    library_id uuid NOT NULL REFERENCES libraries(id) ON DELETE CASCADE,

    -- The shared content hash. This IS the group's identity: two files are in
    -- the same group precisely because their bytes hash identically.
    sha256 bytea NOT NULL CHECK (length(sha256) = 32),

    -- Denormalised counts, so listing groups does not need a join+aggregate per
    -- row. Recomputed on every rebuild, so they cannot drift.
    photo_count      integer NOT NULL CHECK (photo_count >= 2),
    total_bytes      bigint  NOT NULL CHECK (total_bytes >= 0),
    -- Bytes that would be freed by keeping one copy. The number the user
    -- actually cares about.
    reclaimable_bytes bigint NOT NULL CHECK (reclaimable_bytes >= 0),

    -- Suggested photo to keep. For EXACT duplicates the files are identical, so
    -- this is a choice of PATH, not of image quality -- see the rebuild query
    -- for the heuristic. Nothing is ever deleted; this is a suggestion.
    suggested_keep_photo_id uuid REFERENCES photos(id) ON DELETE SET NULL,

    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

-- A library has at most one group per hash. Also the conflict target that makes
-- rebuilding idempotent.
CREATE UNIQUE INDEX duplicate_groups_library_hash_idx
    ON duplicate_groups (library_id, sha256);

-- The UI lists groups worst-offender first.
CREATE INDEX duplicate_groups_reclaimable_idx
    ON duplicate_groups (library_id, reclaimable_bytes DESC);

CREATE TRIGGER duplicate_groups_set_updated_at
    BEFORE UPDATE ON duplicate_groups
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- ---------------------------------------------------------------------------
-- duplicate_group_members: which photos are in which group.
--
-- A join table rather than a group_id column on photos, because a photo will
-- later belong to both an exact-duplicate group and a near-duplicate group
-- (Phase 6). One column could not express both.
-- ---------------------------------------------------------------------------
CREATE TABLE duplicate_group_members (
    group_id uuid NOT NULL REFERENCES duplicate_groups(id) ON DELETE CASCADE,
    photo_id uuid NOT NULL REFERENCES photos(id) ON DELETE CASCADE,
    PRIMARY KEY (group_id, photo_id)
);

-- "Which group is this photo in?" -- the reverse lookup the photo detail view
-- needs. The primary key covers the forward direction.
CREATE INDEX duplicate_group_members_photo_idx
    ON duplicate_group_members (photo_id);
