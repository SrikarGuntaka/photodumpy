-- 0009_thumbnails: generated preview images.
--
-- The thumbnail FILE lives on disk in its own volume, named by photo id. These
-- columns record that it exists and what it is; they do not store a path. The
-- path is a pure function of the id (see internal/thumbnail.PathFor), and
-- storing it would create a second source of truth that could disagree with
-- the first -- or, worse, be a column an attacker could point somewhere else.

ALTER TABLE photos
    -- Idempotency marker, mirroring the other per-photo passes. Set on success
    -- AND on permanent failure, so an undecodable file is not retried forever.
    ADD COLUMN thumbnailed_at timestamptz,

    -- Dimensions of the generated thumbnail, already oriented upright. The UI
    -- reserves grid space from these before the image loads, so tiles do not
    -- jump around as thumbnails arrive. NULL when generation failed -- the
    -- same "not measured is not zero" rule as everywhere else.
    ADD COLUMN thumbnail_width  integer CHECK (thumbnail_width  IS NULL OR thumbnail_width  > 0),
    ADD COLUMN thumbnail_height integer CHECK (thumbnail_height IS NULL OR thumbnail_height > 0),
    ADD COLUMN thumbnail_bytes  integer CHECK (thumbnail_bytes  IS NULL OR thumbnail_bytes  > 0),

    -- Both dimensions or neither. A half-recorded size is not a size.
    ADD CONSTRAINT photos_thumbnail_dims_pairing CHECK (
        (thumbnail_width IS NULL) = (thumbnail_height IS NULL)
    );

-- Drives the thumbnail pass. Partial on the pending set so it shrinks as work
-- completes, like its siblings.
CREATE INDEX photos_needs_thumbnail_idx
    ON photos (library_id, id)
    WHERE thumbnailed_at IS NULL;
