-- 0008_clusters: time-and-place event clusters.
--
-- A third grouping table alongside duplicate_groups and similar_groups, and
-- for the same reason they are separate from each other: the three answer
-- different questions and have genuinely different shapes.
--
--   duplicate_groups  identity is a shared sha256. Membership is exact.
--   similar_groups    identity is emergent from a distance threshold.
--   clusters          identity is a contiguous RUN in time. Membership is
--                     ordered and non-overlapping -- a photo belongs to
--                     exactly one event, which is not true of either
--                     duplicate or similar grouping.
--
-- That last difference is why members carry a sequence rather than a rank, and
-- why the join table has a UNIQUE constraint on photo_id: a photo cannot be in
-- two events at once, and the database should say so rather than trusting the
-- pass that writes it.

CREATE TABLE clusters (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    library_id uuid NOT NULL REFERENCES libraries(id) ON DELETE CASCADE,

    photo_count integer NOT NULL CHECK (photo_count >= 1),

    -- The event's extent in time, taken from its members.
    started_at timestamptz NOT NULL,
    ended_at   timestamptz NOT NULL,
    CONSTRAINT clusters_time_order CHECK (ended_at >= started_at),

    -- The thresholds this cluster was built with. Recorded for the same reason
    -- similar_groups records its Hamming threshold: the grouping is only
    -- meaningful relative to them, and a stored cluster with no record of the
    -- parameters that produced it cannot be interpreted later.
    max_gap_seconds    integer NOT NULL CHECK (max_gap_seconds > 0),
    max_radius_meters  double precision NOT NULL CHECK (max_radius_meters > 0),

    -- The cluster's first GPS fix, and the point every member distance is
    -- measured from. NULL when no member had a location, which is the common
    -- case: most photos carry no GPS at all.
    anchor_latitude  double precision CHECK (anchor_latitude  IS NULL OR anchor_latitude  BETWEEN -90  AND 90),
    anchor_longitude double precision CHECK (anchor_longitude IS NULL OR anchor_longitude BETWEEN -180 AND 180),
    -- Both or neither. A half-set coordinate is not a location.
    CONSTRAINT clusters_anchor_pairing CHECK (
        (anchor_latitude IS NULL) = (anchor_longitude IS NULL)
    ),

    -- How far the furthest member sits from the anchor. Surfaced so a reviewer
    -- can see how spread out an event actually is instead of taking the
    -- grouping on trust.
    max_distance_meters double precision NOT NULL DEFAULT 0
        CHECK (max_distance_meters >= 0),

    -- How many members carried a GPS fix, and how many were placed by a
    -- filesystem mtime rather than a camera timestamp.
    --
    -- located_count exists because "this event has a location" and "every
    -- photo in it has a location" are very different claims, and the UI must
    -- be able to tell them apart.
    --
    -- filesystem_dated_count exists because a cluster built entirely from
    -- mtimes describes when FILES WERE WRITTEN, not when photographs were
    -- taken. Copying a folder stamps every file within a second or two of
    -- each other, which would otherwise collapse a whole library into one
    -- confident-looking "event". The count is what lets the API label that
    -- cluster low-confidence instead of hiding the weakness.
    located_count          integer NOT NULL DEFAULT 0 CHECK (located_count >= 0),
    filesystem_dated_count integer NOT NULL DEFAULT 0 CHECK (filesystem_dated_count >= 0),

    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

-- Events are read newest-first: a timeline, which is the whole point.
CREATE INDEX clusters_time_idx
    ON clusters (library_id, started_at DESC);

CREATE TRIGGER clusters_set_updated_at
    BEFORE UPDATE ON clusters
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- ---------------------------------------------------------------------------
-- cluster_members
-- ---------------------------------------------------------------------------
CREATE TABLE cluster_members (
    cluster_id uuid NOT NULL REFERENCES clusters(id) ON DELETE CASCADE,
    photo_id   uuid NOT NULL REFERENCES photos(id)  ON DELETE CASCADE,

    -- Position within the event, 1 being the earliest photo. A SEQUENCE, not a
    -- rank: clusters have no "best" member to keep, because an event is not a
    -- set of alternatives. Nothing here suggests deleting anything.
    sequence integer NOT NULL CHECK (sequence >= 1),

    -- Distance from the cluster anchor, or NULL when this photo has no GPS
    -- (or the cluster has no anchor). NULL rather than 0: "no fix" and "at
    -- the anchor" are different facts, and storing the first as the second
    -- would put unlocated photos at the centre of every map.
    distance_meters double precision CHECK (distance_meters IS NULL OR distance_meters >= 0),

    PRIMARY KEY (cluster_id, photo_id),

    -- One event per photo. Unlike duplicate and similar membership, cluster
    -- membership partitions the library, and the constraint is here so a bug
    -- in the pass surfaces as a failed write rather than as a photo quietly
    -- appearing on two timelines.
    UNIQUE (photo_id)
);

-- Members are read in sequence order.
CREATE INDEX cluster_members_sequence_idx
    ON cluster_members (cluster_id, sequence);
