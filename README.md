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
| 3 | Metadata extraction (EXIF, GPS, dimensions) | **Done** |
| 4 | Exact duplicate detection (SHA-256) | **Done** |
| 5 | Distributed job queue: leases, retries, crash recovery | **Done** |
| 6 | Near-duplicate detection (perceptual hashing) | **Done** |
| 7 | Quality analysis (sharpness, exposure, contrast) | **Done** |
| 8 | Time + location clustering | **Done** |
| 9 | Full read API: filtering, sorting, photo detail | **Done** |
| 10 | React frontend | Next |
| 11 | Benchmarks and polish | Planned |

Benchmarks are absent from this README on purpose. They will be added in Phase
11 from measured runs, not estimates.

---

## Requirements

- Docker + Docker Compose
- Go 1.25+ *(optional — only for running tests outside a container)*

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
| `POST /api/libraries/{id}/metadata` | Extract EXIF/GPS/dimensions. Returns 202; poll for progress. |
| `POST /api/libraries/{id}/hash` | Compute SHA-256 and rebuild duplicate groups. Returns 202. |
| `GET /api/libraries/{id}/duplicates` | Exact-duplicate groups, biggest saving first. |
| `GET /api/libraries/{id}/similar` | Near-duplicate groups with per-photo distances. |
| `GET /api/libraries/{id}/quality` | Photos carrying a quality flag, worst first. |
| `GET /api/libraries/{id}/clusters` | Event timeline grouped by time and location. |
| `GET /api/libraries/{id}/photos` | Paginated photo list with metadata. |
| `GET /api/libraries/{id}/photos/search` | Filtered, sorted photo search. |
| `GET /api/photos/{id}` | One photo, with every group it belongs to. |

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

## Extracting metadata

```bash
docker compose exec api photo-organizer process <library-id> -wait
```

```
  42 / 42 extracted. Done.

  EXIF timestamp        33
  filesystem timestamp  7
  no timestamp          2
  GPS coordinates       24
  failed to decode      2
```

Dimensions come from the image header rather than a full decode, so this reads
a few hundred bytes per file instead of decoding every pixel.

**Missing metadata is normal, not an error.** A photo with no EXIF falls back
to filesystem mtime and records that it did so in `captured_at_source`; the
photo listing marks those `F` rather than `E` so a guessed date is never
mistaken for one the camera recorded. Photos with no GPS keep `NULL`
coordinates -- never `(0,0)`, which is a real place in the Gulf of Guinea and
is what a GPS chip emits when it has no fix.

**EXIF timestamps have no timezone.** The wall clock is interpreted as UTC,
uniformly, so the same photo scanned on two machines gets the same
`captured_at`. See [DESIGN_DECISIONS.md](DESIGN_DECISIONS.md) for why that is
the least-wrong option.

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

## Finding exact duplicates

```bash
docker compose exec api photo-organizer hash <library-id> -wait
docker compose exec api photo-organizer duplicates <library-id>
```

```
Group 1  462559bb7813  (3 copies, 99.8 KB reclaimable)
    duplicate  49.9 KB     misc/nested/deep/scene09 (1).jpg
    duplicate  49.9 KB     trip/day2/scene09-copy.jpg
    KEEP       49.9 KB     trip/day2/scene09.jpg
```

Hashes are streamed, so memory does not scale with file size. Detection is
content-based, so copies are found regardless of filename or directory.

**KEEP picks a path, not a photo.** For exact duplicates every copy is
byte-identical, so the heuristic chooses the one most likely to be the original:
shallowest directory, then shortest name, then alphabetical. That last tiebreak
makes the suggestion stable across runs.

**Nothing is deleted.** There is no delete path in this application. The output
is a list for you to act on yourself.

## Quality analysis

```bash
docker compose exec api photo-organizer quality <library-id>
```

```
Analyzed           42
Flagged            12
Mean score         0.51

  low_resolution             3
  possibly_blurry            3
  possibly_overexposed       3
  possibly_underexposed      3
  shadows_clipped            1

SCORE   SHARP   EXPOS   DIMS       FLAGS                          PATH
0.26    0.20    0.42    640x480    underexposed                   scene12-dark.jpg
0.37    0.02    1.00    640x480    blurry                         trip/scene19-blurry.jpg
0.40    0.39    0.55    640x480    underexposed,shadows_clipped   screenshots/scene04-dark.jpg
```

Four measurements, each on 0..1, combined into an overall score by weighted
mean — sharpness 0.45, exposure 0.30, contrast 0.15, resolution 0.10:

- **Sharpness** — variance of the Laplacian, the standard focus metric.
- **Exposure** — mean luminance plus the fraction of pixels clipped to pure
  black or pure white.
- **Contrast** — standard deviation of the luminance histogram.
- **Resolution** — pixel count against a 2 MP reference.

Every image is first scaled to a 512px long edge, *including images already
smaller than that*. Laplacian variance measures detail **density**, so
measuring a thumbnail at its native size inflates it enormously — a 160×120
crop once scored 37× the 640×480 original it came from, and the near-duplicate
ranker duly recommended keeping the thumbnail. Filter `--flag` to one flag,
e.g. `-flag possibly_blurry`.

**These are measurements of pixels, not judgements of merit.** A shallow
depth-of-field portrait is genuinely blurry by Laplacian variance and may be
the best photograph in the library; motion blur is sometimes the entire point.
That is why every flag is hedged in its own name (`possibly_blurry`, never
`blurry`) and why the raw measurements are stored and displayed alongside the
scores — so you can disagree with a threshold rather than be handed a verdict.
No generative model is involved in deciding any of this, by design.

## Grouping into events

```bash
docker compose exec api photo-organizer clusters <library-id>
```

```
Events             5
Photos placed      40
With GPS           24
Largest event      16 photos
Low confidence     1  (dated from file mtimes, not the camera)

Event 3  2026-03-14 22:19  (4h42m)  8 photos
         32.7767, -96.7970  (7 of 8 located)
    -- 2026-03-14 --
  22:19:00      0m  misc/nested/deep/scene17.jpg
  23:06:00      0m  scene18.jpg
  23:53:00      0m  trip/scene19-blurry.jpg
    -- 2026-03-15 --
  03:01:00       -  misc/nested/deep/scene23-nogps.jpg
```

A single ordered pass cuts between consecutive photos when either test fails:
more than `CLUSTER_MAX_GAP` (default 4h) of silence, or further than
`CLUSTER_MAX_RADIUS_METERS` (default 1500m) from the event's first GPS fix.

The two thresholds stay separate rather than being fed to a general-purpose
clusterer, because combining them into one distance would require an exchange
rate between "one hour" and "one kilometre", and there is no honest such number.

**GPS is optional, and its absence never splits an event.** A photo with no
coordinates joins on time alone — it simply does not take part in the distance
test. In the run above, the split between the Austin and Dallas events was made
by *distance*: the gap between them is 1h34m, well inside the 4-hour threshold,
but they are 293km apart.

**Distance is measured from the event's first fix, not the previous photo.**
Comparing consecutive photos permits unbounded drift — a picture every 200m
along a coast road never exceeds the threshold, so fifty kilometres becomes one
"place". Anchoring bounds an event's radius by construction. The trade is that a
genuine walking tour gets cut into segments, which is the better failure.

**Photos with no timestamp are not clustered, and not hidden.** They are
counted as `undated`. The numbers reconcile exactly: clustered + undated +
missing/failed = every photo in the library.

**Events built from filesystem mtimes are labelled low confidence.** Copying a
folder stamps every file within a second or two, which would otherwise collapse
a library into one enormous, confident-looking "event" — visible as Event 1 in
the sample above.

## Searching the library

```bash
docker compose exec api photo-organizer find <library-id> -sort quality -limit 5
```

```
42 matching, showing 1-5, sorted by quality asc

PATH                                    SIZE        DIMS         QUAL   CAPTURED          MARKS
scene12-dark.jpg                        52.3 KB     640x480      0.263  2026-03-14 09:00  similar possibly_underexposed
trip/day1/scene20-bright.jpg            48.2 KB     640x480      0.352  2026-03-14 09:01  similar possibly_overexposed
trip/scene19-blurry.jpg                 15.8 KB     640x480      0.373  2026-03-14 23:53  gps possibly_blurry
```

Filters compose: `-q <substring>`, `-flag <quality-flag>`, `-gps true|false`,
`-from`/`-to` (YYYY-MM-DD), `-sort path|captured_at|file_size|quality|created_at`,
`-desc`. The HTTP equivalent is
`GET /api/libraries/{id}/photos/search`, which also accepts `state`,
`has_duplicates` and `has_similar`.

**Bad input is a 400, never a silent no-op.** An unknown sort key, a misspelled
flag, or `to` before `from` is rejected with a message. A filter that quietly
does nothing is worse than an error — the caller sees a plausible result set
with no way to know it was unfiltered.

**Sort keys are a closed allow-list.** `ORDER BY` cannot take a bind parameter,
which makes sorting the one place a read API is tempted to interpolate user
text into SQL. It doesn't: the key is looked up in a map and the caller's
string never reaches the query. `sort=relative_path` — a real column — is
rejected too.

**Unmeasured is not zero.** A photo that failed to decode shows `-` for
quality, not `0.000`. Sorting worst-first puts `NULLS LAST` so a page of
unanalysed photos cannot masquerade as the worst photos in the library.

**`total` counts what matched the filter**, not the library, so a paginator
built on it cannot offer pages that do not exist. The count and the page share
one predicate builder.

## Inspecting one photo

```bash
docker compose exec api photo-organizer photo <photo-id>
```

```
trip/day1/scene02.jpg
  captured      2026-03-14T10:34:00Z (exif)
  location      30.26720, -97.74310

  QUALITY (measurements of pixels, not judgements of merit)
    sharpness   0.340
    overall     0.506

  sha256        580340268479370d4bde1835a6cb980a482c459a4b591411cf51b6ab5afbbfe8
  phash         85c9946bd6ac0857

  NEAR-DUPLICATES (3)
  -> KEEP 61.4 KB   640x480      dist   0  trip/day1/scene02.jpg
          24.2 KB   640x480      dist   3  trip/day1/scene02-recompressed.jpg
          17.0 KB   320x240      dist   7  trip/day1/scene02-small.jpg
```

One request returns everything the pipeline learned about a file plus every
group it belongs to, with `->` marking the photo you asked about so it stays in
place among its siblings. The distance column means different things per
group — Hamming bits for near-duplicates, metres for an event — and exact
duplicates have none, because they are identical.

## Testing

**[TESTING.md](TESTING.md) is a step-by-step walkthrough** covering everything
built so far, with expected output for each step and what it proves.

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
