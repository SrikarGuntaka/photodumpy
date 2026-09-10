-- 0007_quality: objective image quality metrics.
--
-- Every column here describes PIXELS, not merit. See internal/quality for why
-- that distinction is enforced rather than merely stated: a shallow
-- depth-of-field portrait is genuinely blurry by Laplacian variance and may be
-- the best photograph in the library.

ALTER TABLE photos
    -- Normalised 0..1 scores. Nullable: a photo that has not been analysed, or
    -- whose pixels could not be decoded, legitimately has none of these.
    ADD COLUMN sharpness_score  double precision CHECK (sharpness_score  IS NULL OR sharpness_score  BETWEEN 0 AND 1),
    ADD COLUMN exposure_score   double precision CHECK (exposure_score   IS NULL OR exposure_score   BETWEEN 0 AND 1),
    ADD COLUMN contrast_score   double precision CHECK (contrast_score   IS NULL OR contrast_score   BETWEEN 0 AND 1),
    ADD COLUMN resolution_score double precision CHECK (resolution_score IS NULL OR resolution_score BETWEEN 0 AND 1),
    ADD COLUMN quality_score    double precision CHECK (quality_score    IS NULL OR quality_score    BETWEEN 0 AND 1),

    -- The raw measurements, stored alongside the normalised scores.
    --
    -- Not redundant: normalisation involves a saturation constant and a curve,
    -- both of which are judgement calls that may be revised. The raw numbers
    -- are facts about the image. Keeping them means a future re-normalisation
    -- is a SQL update rather than a full re-decode of every photo in the
    -- library.
    ADD COLUMN raw_laplacian_variance double precision,
    ADD COLUMN mean_luminance         double precision CHECK (mean_luminance IS NULL OR mean_luminance BETWEEN 0 AND 255),
    ADD COLUMN shadow_clipping        double precision CHECK (shadow_clipping    IS NULL OR shadow_clipping    BETWEEN 0 AND 1),
    ADD COLUMN highlight_clipping     double precision CHECK (highlight_clipping IS NULL OR highlight_clipping BETWEEN 0 AND 1),

    -- Hedged flags: possibly_blurry, possibly_underexposed, and so on. An
    -- array rather than a boolean per flag, because the set will grow and each
    -- new one would otherwise be a migration plus a column that is false for
    -- every existing row.
    ADD COLUMN quality_flags text[] NOT NULL DEFAULT '{}',

    -- Idempotency marker, mirroring the other passes.
    ADD COLUMN quality_analyzed_at timestamptz;

-- Drives the analysis pass. Partial on the pending set so it shrinks as work
-- completes.
CREATE INDEX photos_needs_quality_idx
    ON photos (library_id, id)
    WHERE quality_analyzed_at IS NULL;

-- "Show me the worst photos first" is the review screen's primary query.
CREATE INDEX photos_quality_score_idx
    ON photos (library_id, quality_score)
    WHERE quality_score IS NOT NULL;

-- Finding every photo carrying a given flag. GIN because the predicate is
-- array containment (`quality_flags @> ARRAY['possibly_blurry']`), which a
-- B-tree cannot answer.
CREATE INDEX photos_quality_flags_idx
    ON photos USING GIN (quality_flags)
    WHERE quality_flags <> '{}';
