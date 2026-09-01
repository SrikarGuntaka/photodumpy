-- 0001_core: libraries and photos.
--
-- Scope note: this migration creates only the columns that the ingestion scan
-- itself can populate. Metadata, hashes, quality metrics and their indexes are
-- added by the migrations for the phases that actually write them, so the
-- schema never contains columns that no code touches.

-- ---------------------------------------------------------------------------
-- Shared helper: keep updated_at honest without relying on every caller to set
-- it. A trigger is the only way to guarantee it, since rows are updated from
-- several places (API, workers, aggregate stages).
-- ---------------------------------------------------------------------------
CREATE OR REPLACE FUNCTION set_updated_at() RETURNS trigger AS $$
BEGIN
    NEW.updated_at = now();
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

-- ---------------------------------------------------------------------------
-- libraries: one user-chosen source of photos.
--
-- source_kind exists from day one so that the future iOS client is a new value
-- ('ios_photokit') rather than a schema change. root_path is meaningful only
-- for local_fs sources.
-- ---------------------------------------------------------------------------
CREATE TABLE libraries (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name        text        NOT NULL CHECK (length(btrim(name)) > 0),
    source_kind text        NOT NULL DEFAULT 'local_fs'
                            CHECK (source_kind IN ('local_fs', 'ios_photokit')),
    -- Absolute path on the machine running the API, already validated to sit
    -- inside PHOTO_ROOT. Normalised to forward slashes with no trailing
    -- separator so equality comparisons are meaningful across platforms.
    root_path   text        NOT NULL,

    -- Scan bookkeeping. last_scan_started_at being non-null while
    -- last_scan_finished_at is null means a scan was interrupted -- the UI can
    -- say so, and a later scan simply resumes (inserts are idempotent).
    last_scan_started_at  timestamptz,
    last_scan_finished_at timestamptz,

    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now()
);

-- The same folder should not be registered twice; a second `scan` of the same
-- path must find the existing library rather than creating a duplicate.
CREATE UNIQUE INDEX libraries_source_idx ON libraries (source_kind, root_path);

CREATE TRIGGER libraries_set_updated_at
    BEFORE UPDATE ON libraries
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- ---------------------------------------------------------------------------
-- photos: one row per discovered image file.
--
-- state is a coarse lifecycle marker, deliberately NOT a mirror of job status.
-- Job progress lives in the jobs table; this column answers "is this photo
-- usable in the UI yet, and if not, why".
-- ---------------------------------------------------------------------------
CREATE TYPE photo_state AS ENUM (
    'discovered',  -- row exists, nothing derived yet
    'processing',  -- at least one derivation job has run
    'ready',       -- all required derivations succeeded
    'missing',     -- file was gone when a worker tried to read it
    'failed'       -- a derivation exhausted its retries
);

CREATE TABLE photos (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    library_id uuid NOT NULL REFERENCES libraries(id) ON DELETE CASCADE,

    -- Identity within the library. Relative to libraries.root_path, normalised
    -- to forward slashes. Storing it relative (not absolute) means moving or
    -- remounting the folder is a single-row update, and it is the same column
    -- that will hold a PhotoKit local identifier for an ios_photokit library.
    relative_path     text   NOT NULL CHECK (length(relative_path) > 0),
    original_filename text   NOT NULL,

    -- Filesystem facts, available from the scan itself without opening the
    -- file. file_modified_at is the fallback capture time when EXIF is absent.
    file_size_bytes  bigint      NOT NULL CHECK (file_size_bytes >= 0),
    file_modified_at timestamptz,

    -- Extension-derived guess, refined by content sniffing in phase 3.
    detected_format text,

    state      photo_state NOT NULL DEFAULT 'discovered',
    last_error text,

    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

-- Rescan idempotency: re-scanning a folder must not insert the same file twice.
-- This is the constraint the scan's ON CONFLICT targets.
CREATE UNIQUE INDEX photos_library_path_idx ON photos (library_id, relative_path);

-- Progress counts ("2,438 discovered, 1,982 ready") are a GROUP BY over this.
CREATE INDEX photos_library_state_idx ON photos (library_id, state);

CREATE TRIGGER photos_set_updated_at
    BEFORE UPDATE ON photos
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();
