# photo-organizer

A local-first tool for making sense of a large, messy folder of photos.

Point it at a directory and it finds exact duplicates, near-duplicates (resized,
recompressed, burst shots), likely-bad photos (blurry, over/under-exposed), and
groups everything into events by time and location.

**Nothing is ever deleted, moved or modified.** Every cleanup action is a
suggestion you review. The photo directory is mounted read-only, so the
application is structurally incapable of writing to it.

---

## Status

Built in phases, each independently runnable and testable.

| Phase | Scope | Status |
|-------|-------|--------|
| 1 | Skeleton: Postgres, migrations, config, Docker Compose, health endpoints, CLI | **Done** |
| 2 | Local folder ingestion (`scan`) | **Done** |
| 3 | Metadata extraction (EXIF, GPS, dimensions) | Next |
| 4 | Exact duplicate detection (SHA-256) | Planned |
| 5 | Distributed job queue: leases, retries, crash recovery | Planned |
| 6 | Near-duplicate detection (perceptual hashing) | Planned |
| 7 | Quality analysis (sharpness, exposure, contrast) | Planned |
| 8 | Time + location clustering | Planned |
| 9 | Full read API | Planned |
| 10 | React frontend | Planned |
| 11 | Benchmarks and polish | Planned |

Benchmarks are absent from this README on purpose. They will be added in Phase
11 from measured runs, not estimates.

---

## Requirements

- Docker + Docker Compose
- Go 1.23+ *(optional — only for running tests outside a container)*

## Running it

```bash
cp .env.example .env
```

Edit `.env` and set `HOST_PHOTOS_DIR` to the folder you want to organise:

```
HOST_PHOTOS_DIR=C:/Users/you/Pictures      # Windows (forward slashes)
HOST_PHOTOS_DIR=/home/you/Pictures          # Linux / macOS
```

Then:

```bash
docker compose up -d --build
```

Check it came up:

```bash
docker compose exec api photo-organizer status
```

Expected output:

```
API      http://localhost:8080
Process  OK (up 12s)
Database UP (2ms)
```

The API is on <http://localhost:8080>:

| Endpoint | Purpose |
|----------|---------|
| `GET /healthz` | Liveness. Does not touch the database. |
| `GET /readyz` | Readiness. Returns 503 if Postgres is unreachable. |
| `POST /api/libraries` | Register a folder. Idempotent: same path returns the existing library. |
| `GET /api/libraries` | List libraries. |
| `GET /api/libraries/{id}` | One library, with photo counts by state. |
| `POST /api/libraries/{id}/scan` | Start a scan. Returns 202; poll the library for progress. |
| `GET /api/libraries/{id}/photos` | Paginated photo list. |

## Scanning a folder

```bash
docker compose exec api photo-organizer scan /photos -wait
```

That registers the folder as a library (if it is not already one) and scans it:

```
Created library 6f2c1e08-...
Root  /photos

Scan started.
  46 photos discovered. Scan complete.
```

Then inspect what it found:

```bash
docker compose exec api photo-organizer libraries
docker compose exec api photo-organizer photos <library-id>
```

**Scanning is idempotent.** Running the same scan twice discovers the same
files and inserts none of them the second time — the API reports `discovered`
and `inserted` separately so that is visible rather than merely claimed.

**Paths are validated.** The API refuses any path outside the configured
`PHOTO_ROOT`, including via `..` traversal or a symlink pointing out of the
tree. In Docker that root is the read-only `/photos` mount.

## Common tasks

```bash
make up        # build + start everything
make logs      # tail all logs
make status    # run the CLI health check
make psql      # psql shell against the running database
make test      # run Go unit tests
make down      # stop, keeping data
make reset     # stop and DELETE the database volume
```

`make` targets work with only Docker installed; a local Go toolchain just makes
them faster.

## Test photos

The repo ships a generator that writes a synthetic photo corpus with **known
ground truth**, so tests can assert exact numbers:

```bash
go run ./cmd/genfixtures -root ./sample-photos -clean
```

It produces 46 files including exact duplicates (byte-identical, across
different directories), near-duplicates (recompressed and resized — different
bytes, same image), deliberately blurred and over/under-exposed frames, JPEGs
with full EXIF+GPS, with EXIF but no GPS, and with no EXIF at all, plus
unsupported extensions and three kinds of corrupt file. Ground truth is written
to `sample-photos/MANIFEST.json`.

Generation is deterministic: the same `-seed` always produces byte-identical
output, and different seeds produce different corpora.

Why synthetic first: pointed at a real folder, "found 12 duplicate groups" is
unfalsifiable, because nobody knows how many that folder actually contains. The
generator decides, so a test can assert. Real photo libraries are the better
*validation* input — and the Phase 11 benchmarks use one — but they cannot
verify correctness.

To use your own photos instead, set `HOST_PHOTOS_DIR` in `.env`.

## Testing

### Unit tests — no database needed

```bash
go test ./...
```

Covers config parsing and validation, the migration loader (ordering, checksum
drift, malformed filenames), the fixture generator's ground truth, and — most
importantly — **path containment**: `..` traversal, absolute paths outside the
root, prefix confusion (`/photos-evil` vs `/photos`), and symlinks inside the
root pointing out of it.

Two symlink tests skip on Windows, where creating a symlink needs elevation.
They run on Linux via `make test`.

### Integration tests — real Postgres

The scan's guarantees *are* database behaviour (ON CONFLICT idempotency, the
claim-by-update scan guard, batch insert counts), so mocking the store would
just test the mock.

```bash
make test-integration
```

### End to end

```bash
docker compose up -d --build
docker compose exec api photo-organizer status
docker compose exec postgres psql -U photo -d photoorganizer -c '\dt'
```

You should see `libraries`, `photos` and `schema_migrations`.

The API survives a database restart without being restarted itself:

```bash
docker compose restart postgres
docker compose exec api photo-organizer status          # -> Database UP again
```

## Architecture

See [ARCHITECTURE.md](ARCHITECTURE.md) for how ingestion, the job queue, worker
lifecycle and the processing stages fit together, and
[DESIGN_DECISIONS.md](DESIGN_DECISIONS.md) for why each significant choice was
made — including the ones that were rejected.

The short version:

```
React UI / CLI  ->  Go API  ->  PostgreSQL  <-  Go workers (N)  ->  photos (read-only)
                                     ^
                              also the job queue
```

Three processes: `api`, `worker` (scaled to N), `postgres`. The job queue is
built directly on Postgres — no Redis, no broker. Workers claim jobs with
`SELECT ... FOR UPDATE SKIP LOCKED` under a lease; if a worker dies, its lease
expires and the work is picked up by someone else.

## Known limitations

- **HEIC is not supported.** iPhone photos in HEIC are skipped, not processed.
  Pure-Go HEIC decoding is not practical today; adding it means a CGo dependency
  on libheif/libvips. Deferred deliberately, tracked as a real limitation.
- **Single user, no authentication.** This is a local tool. Do not expose the
  API to a network you do not control.
- **Photos must live under one directory tree.** The application can only read
  inside the configured `PHOTO_ROOT`, by design.

## Privacy

Photos never leave your machine. There are no external API calls, no telemetry,
and no AI services involved in any part of the analysis — the quality scoring
and similarity detection are classical image processing, not a model.
