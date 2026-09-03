-- 0002_metadata: EXIF-derived and header-derived photo metadata.
--
-- Every column here is nullable, and that is load-bearing. A real photo dump
-- contains files with no EXIF at all, EXIF with no GPS, EXIF with a corrupt
-- GPS block, and files that are not images despite their extension. NULL means
-- "not known", which is distinguishable from a real zero -- (0,0) is a genuine
-- location in the Gulf of Guinea, and 0x0 is not a real image size.

ALTER TABLE photos
    -- Pixel dimensions as they should be DISPLAYED, i.e. already swapped when
    -- EXIF orientation indicates a 90/270 degree rotation. Storing display
    -- dimensions means the UI and the aspect-ratio maths do not each have to
    -- remember to consult orientation.
    ADD COLUMN width  integer CHECK (width  IS NULL OR width  > 0),
    ADD COLUMN height integer CHECK (height IS NULL OR height > 0),

    -- Raw EXIF orientation (1-8). Kept because pixel-level work in later
    -- phases -- thumbnails, perceptual hashing, blur detection -- reads the
    -- undecoded image and must apply the rotation itself.
    ADD COLUMN orientation smallint CHECK (orientation IS NULL OR orientation BETWEEN 1 AND 8),

    -- When the photo was taken. See captured_at_source for how much to trust it.
    ADD COLUMN captured_at timestamptz,

    -- 'exif'       -> from EXIF DateTimeOriginal (or DateTimeDigitized)
    -- 'filesystem' -> fell back to file mtime because there was no EXIF date
    -- NULL         -> no usable timestamp at all
    --
    -- Storing the provenance rather than silently backfilling mtime into an
    -- EXIF-shaped field means clustering can weight these differently and the
    -- UI can be honest about which dates are guesses.
    ADD COLUMN captured_at_source text
        CHECK (captured_at_source IS NULL OR captured_at_source IN ('exif', 'filesystem')),

    ADD COLUMN latitude  double precision CHECK (latitude  IS NULL OR latitude  BETWEEN -90  AND 90),
    ADD COLUMN longitude double precision CHECK (longitude IS NULL OR longitude BETWEEN -180 AND 180),

    ADD COLUMN camera_make  text,
    ADD COLUMN camera_model text,

    -- Set when extraction has run to completion for this photo, successfully or
    -- not. This is the idempotency marker: re-running extraction skips rows
    -- that already have it, and clearing it forces a re-extract. Distinct from
    -- "has metadata", because a photo with genuinely no EXIF has been
    -- successfully extracted and still has NULL captured_at.
    ADD COLUMN metadata_extracted_at timestamptz;

-- Clustering (phase 8) sweeps photos in capture order. Partial because photos
-- with no timestamp are excluded from the chronology entirely rather than
-- being sorted as if they were epoch zero.
CREATE INDEX photos_captured_at_idx
    ON photos (library_id, captured_at)
    WHERE captured_at IS NOT NULL;

-- "Which photos still need metadata?" is the processor's driving query, and
-- also what the progress endpoint counts. Partial on the pending set, so the
-- index stays small as a library finishes processing rather than growing with
-- every completed photo.
CREATE INDEX photos_needs_metadata_idx
    ON photos (library_id, id)
    WHERE metadata_extracted_at IS NULL;

-- Geographic queries in phase 8 filter to photos that actually have a fix.
CREATE INDEX photos_location_idx
    ON photos (library_id, latitude, longitude)
    WHERE latitude IS NOT NULL AND longitude IS NOT NULL;
