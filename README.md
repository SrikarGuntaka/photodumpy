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
| 2 | Local folder ingestion (`scan`) | Next |
| 3 | Metadata extraction (EXIF, GPS, dimensions) | Planned |
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

The API is on <http://localhost:8080>. Two endpoints exist in Phase 1:

| Endpoint | Purpose |
|----------|---------|
| `GET /healthz` | Liveness. Does not touch the database. |
| `GET /readyz` | Readiness. Returns 503 if Postgres is unreachable. |

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

## Testing Phase 1

```bash
go test ./...
```

Covers configuration parsing and validation, and the migration loader
(ordering, checksum drift detection, malformed filenames). Neither requires a
database.

To verify the stack end to end:

```bash
docker compose up -d --build
docker compose exec api photo-organizer status          # -> Process OK, Database UP
docker compose exec postgres psql -U photo -d photoorganizer -c '\dt'
```

You should see `libraries`, `photos` and `schema_migrations`.

To verify the API survives a database restart (it should, without being
restarted itself):

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
