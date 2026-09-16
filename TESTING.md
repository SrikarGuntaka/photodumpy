# Testing guide

A single pass that exercises everything the project does, from an empty stack
to crash recovery. Roughly 30 minutes, most of it reading.

Each step says **what you should see** and **what it proves**. Expected output
below was captured from a real run. Ids, timestamps and hostnames will differ;
counts and structure should not. If yours differ in substance, that is a real
finding.

---

## 0. Prerequisites

Docker Desktop must be running. Go 1.25+ is needed for steps 2 and 22–23, and
Node 22+ for the web tests in step 23.

> **If Docker Desktop won't start**, or starts and then every `docker` command
> hangs — this happened several times during development, including after the
> laptop slept mid-benchmark. The error is typically
> `Docker Desktop is unable to start`. The cause is stale runtime sockets from
> an unclean shutdown. Quit Docker Desktop, then:
>
> ```powershell
> Rename-Item "$env:LOCALAPPDATA\Docker\run" "$env:LOCALAPPDATA\Docker\run.stale"
> Rename-Item "$env:LOCALAPPDATA\docker-secrets-engine" "$env:LOCALAPPDATA\docker-secrets-engine.stale"
> ```
>
> Start Docker Desktop again. Both directories are recreated automatically.

### Shell differences — read this first

Commands below are for **PowerShell**, the Windows default. Three things break if
you paste bash commands into it:

- **`grep` does not exist.** Use `Select-String`.
- **`curl` is an alias for `Invoke-WebRequest`**, which takes different
  parameters. Use `Invoke-RestMethod`, or call `curl.exe` explicitly.
- **Docker's progress output goes to stderr**, which PowerShell wraps in a red
  `NativeCommandError` block. That is PowerShell reporting stderr, not a failure;
  read the message inside it.

**In Git Bash instead?** The commands translate directly, but prefix
`docker compose exec` with `MSYS_NO_PATHCONV=1`, or Git Bash rewrites `/photos`
into a Windows path before it reaches the container.

**If port 5432 is taken** — by a native PostgreSQL install, say — set
`POSTGRES_PORT` in `.env`. Note that nothing below connects to Postgres from the
host: every database command goes through `docker compose exec`, precisely so a
second Postgres on the machine can never be queried by mistake.

---

## 1. Start clean

```powershell
cd "C:/Users/srika/Documents/dev/photo dump cleaner"
docker compose down -v
docker compose up -d --build
```

**Expect:** `postgres`, `api` and one `worker` reach healthy/started.

**Proves:** the whole stack — including the React UI, built inside the image —
builds and starts from nothing.

---

## 2. Generate the test corpus

```powershell
go run ./cmd/genfixtures -root ./sample-photos -clean
```

**Expect:**

```
Wrote 46 files to ./sample-photos

  supported images        42
  unsupported (skipped)   4
  corrupt / undecodable   3

  exact duplicate groups  3  (9 files)
  near-duplicate groups   6

  with GPS                24
  without GPS             15
  without EXIF timestamp  6

  intentionally blurry    3
  underexposed            3
  overexposed             3
```

**Proves:** a synthetic corpus with ground truth recorded in
`sample-photos/MANIFEST.json`. Every count later in this guide is checked against
these numbers. The same seed produces byte-identical files.

*(No local Go? `make fixtures` runs it in a container.)*

---

## 3. Health — Phase 1

```powershell
docker compose exec api photo-organizer status
Invoke-RestMethod http://localhost:8080/healthz
Invoke-RestMethod http://localhost:8080/readyz
```

**Expect:**

```
API      http://localhost:8080
Process  OK (up 1m5s)
Database UP (0ms)
Libraries 0
```

and `status: ok` from both endpoints.

---

## 4. Migrations — Phase 1

```powershell
docker compose exec postgres psql -U photo -d photoorganizer -c 'SELECT version, name FROM schema_migrations ORDER BY version'
```

**Expect** nine rows, applied exactly once:

```
 version |    name
---------+------------
       1 | core
       2 | metadata
       3 | duplicates
       4 | jobs
       5 | deferred
       6 | similar
       7 | quality
       8 | clusters
       9 | thumbnails
```

---

## 5. The API survives a database restart — Phase 1

```powershell
docker compose stop postgres
try { $null = Invoke-RestMethod http://localhost:8080/healthz -ErrorAction Stop; "healthz: 200" } catch { "healthz: $([int]$_.Exception.Response.StatusCode)" }
try { $null = Invoke-RestMethod http://localhost:8080/readyz  -ErrorAction Stop; "readyz:  200" } catch { "readyz:  $([int]$_.Exception.Response.StatusCode)" }
docker compose start postgres
Start-Sleep -Seconds 5
docker compose exec api photo-organizer status
```

**Expect:** `healthz: 200` and `readyz: 503` while Postgres is down; then
`Database UP` again, with the API's uptime still climbing — it never restarted.

**Proves:** liveness deliberately ignores the database, so a database blip does
not trigger a restart loop. Readiness does check it, and recovers by itself.

---

## 6. Applied migrations are immutable — Phase 1

```powershell
Add-Content migrations/0001_core.sql "-- tampered"
docker compose up -d --build api
Start-Sleep -Seconds 10
docker compose logs api --tail 5
```

**Expect** the API to refuse to start:

```
fatal: database: migration 0001_core was already applied but its contents
changed (recorded fa8dd4b31cad, on disk ...). Applied migrations are
immutable -- add a new one instead
```

**Restore it:**

```powershell
git checkout migrations/0001_core.sql
docker compose up -d --build api
```

---

## 7. Originals cannot be modified — Phase 1

```powershell
docker compose exec api sh -c "touch /photos/EVIL.txt"
docker compose exec api id
```

**Expect:** `Read-only file system`, and `uid=10001(photouser)`.

**Proves:** "never modify original photos" is enforced by the kernel through a
read-only mount, not by convention. The container runs as a non-root user.

---

## 8. Scan the folder — Phase 2

```powershell
docker compose exec api photo-organizer scan /photos -wait
```

**Expect:**

```
Created library <uuid>
Root  /photos

Scan started.
  42 photos discovered. Scan complete.
```

**42 must match step 2's `supported images`.**

Save the library id for the rest of the guide:

```powershell
$lib = (Invoke-RestMethod http://localhost:8080/api/libraries).libraries[0].id
```

---

## 9. Rescanning is idempotent — Phase 2

```powershell
docker compose exec api photo-organizer scan /photos -wait
docker compose logs api | Select-String "scan complete"
```

**Expect** two log lines — the second is the one that matters:

```
... discovered=42 inserted=42 already_known=0  skipped_unsupported=4 ...
... discovered=42 inserted=0  already_known=42 skipped_unsupported=4 ...
```

**Proves:** a rescan discovers the same files and inserts none of them — and the
count reports it rather than the code asserting it.

---

## 10. Path traversal is refused — Phase 2

```powershell
foreach ($p in @('/etc','/photos/../etc','../../../../etc/passwd','/var/lib/postgresql','/photos')) {
  $body = "{`"path`":`"$p`"}"
  try {
    $null = Invoke-RestMethod -Uri http://localhost:8080/api/libraries -Method Post `
      -ContentType 'application/json' -Body $body -ErrorAction Stop
    "{0,-28} -> ACCEPTED" -f $p
  } catch {
    "{0,-28} -> {1} rejected" -f $p, [int]$_.Exception.Response.StatusCode
  }
}
```

**Expect:** the four escape attempts rejected with `400`, and `/photos` accepted.
The error body names no filesystem path — echoing one would confirm what exists
outside the root.

---

## 11. Interrupted scans recover — Phase 2

```powershell
docker compose exec postgres psql -U photo -d photoorganizer -c "UPDATE libraries SET last_scan_started_at = now(), last_scan_finished_at = NULL"
docker compose restart api
Start-Sleep -Seconds 8
docker compose logs api | Select-String reconciled
docker compose exec api photo-organizer libraries
```

**Expect:** a log line `reconciled scans interrupted by a previous shutdown`, and
the library back to `complete`.

**Proves:** a crashed scan cannot lock a library out forever. A fresh process
owns no scans, so anything still marked running was interrupted.

---

## 12. Process the library with a worker fleet — Phase 5

```powershell
docker compose up -d --scale worker=4
docker compose exec api photo-organizer queue $lib -wait
docker compose exec api photo-organizer workers
```

**Expect** every job to succeed:

```
Enqueued 213 new jobs.

  213/213 done  (213 succeeded, 0 dead)

JOB TYPE                    PEND     RUN      OK    DEAD   TOTAL
ANALYZE_QUALITY                0       0      42       0      42
BUILD_CLUSTERS                 0       0       1       0       1
BUILD_DUPLICATE_GROUPS         0       0       1       0       1
BUILD_SIMILAR_GROUPS           0       0       1       0       1
COMPUTE_FILE_HASH              0       0      42       0      42
COMPUTE_PERCEPTUAL_HASH        0       0      42       0      42
EXTRACT_METADATA               0       0      42       0      42
GENERATE_THUMBNAIL             0       0      42       0      42
```

and four active workers:

```
Active 4   Slots 88   Running jobs 0

STATUS      HOSTNAME          PID      SLOTS   JOBS  LAST SEEN
active      29a43c7045ad      1           22      0  2s ago
...
```

`-all` also lists workers from earlier runs; `stopped` ones shut down cleanly,
`dead` ones stopped heartbeating and had their leases reclaimed.

**Proves:** 213 jobs — five per photo, plus three library-wide stages —
distributed across four processes claiming from one Postgres table with
`FOR UPDATE SKIP LOCKED`, with no broker.

Run `queue` again and it reports `Enqueued 3 new jobs.` — only the three
library-wide stages, which rebuild their groups from scratch. No per-photo work
is repeated, because every photo's stages are already recorded as complete.

---

## 13. Metadata — Phase 3

```powershell
Invoke-RestMethod "http://localhost:8080/api/libraries/$lib" | Select-Object -ExpandProperty metadata
```

**Expect:**

```
total          : 42
extracted      : 42
pending        : 0
with_exif_date : 33
with_file_date : 7
with_no_date   : 2
with_gps       : 24
failed         : 2
```

`with_gps` must match step 2. `failed 2` is `misc/empty.jpg` and
`misc/truncated.jpg`, which cannot be read at all. The third corrupt fixture,
`misc/half-written.jpg`, is cut off mid-stream but has a readable header, so its
metadata succeeds and it fails later, at pixel decoding — it gets no thumbnail
and no quality score.

**Proves:** missing EXIF and missing GPS are normal, not errors. A photo with no
camera date falls back to its file's modification time and records that it did;
a photo with no GPS keeps `NULL` coordinates, never `(0,0)`.

---

## 14. Exact duplicates — Phase 4

```powershell
docker compose exec api photo-organizer duplicates $lib -limit 1
```

**Expect:**

```
Duplicate groups   3
Duplicate files    9
Reclaimable        323.9 KB

Group 1  60cd23b46e19  (3 copies, 120.3 KB reclaimable)
    duplicate  60.1 KB     misc/nested/deep/scene01 (1).jpg
    duplicate  60.1 KB     trip/scene01-copy.jpg
    KEEP       60.1 KB     trip/scene01.jpg

These are suggestions. No files have been modified, moved or deleted.
KEEP marks the least-nested copy, then the shortest name, then alphabetical.
Every copy is byte-identical, so this picks a path, not a photo.
```

**3 groups and 9 files must match step 2.**

---

## 15. Near-duplicates — Phase 6

```powershell
docker compose exec api photo-organizer similar $lib -limit 1
```

**Expect:**

```
Similar groups     6
Photos involved    15
Reclaimable        290.0 KB

Group 1  (3 photos, 44.9 KB reclaimable)
  KEEP     dist  0  66.9 KB     640x480      screenshots/scene10.jpg
  similar  dist  1  26.3 KB     640x480      screenshots/scene10-recompressed.jpg
  similar  dist  5  18.6 KB     320x240      screenshots/scene10-small.jpg
```

**6 groups must match step 2.** None of them is an exact-duplicate group:
byte-identical copies are collapsed to one representative before similarity
grouping, so the two reports never count the same bytes twice.

**Proves:** recompressed and resized copies — different bytes, same picture — are
found by perceptual hashing, which SHA-256 alone would miss.

---

## 16. Quality — Phase 7

```powershell
docker compose exec api photo-organizer quality $lib -limit 3
```

**Expect:**

```
Analyzed           42
Flagged            12
Mean score         0.51

  low_resolution             3
  possibly_blurry            3
  possibly_overexposed       3
  possibly_underexposed      3
  shadows_clipped            1
```

**Blurry, underexposed and overexposed must each be 3, matching step 2.** The 3
low-resolution flags are the 320×240 resized copies, and the one shadow-clipped
photo is also an underexposed one.

**Proves:** measurements, not verdicts. Every flag is hedged in its name, because
a shallow-focus portrait genuinely measures as blurry and may be the best photo
in the library.

---

## 17. Events — Phase 8

```powershell
docker compose exec api photo-organizer clusters $lib
```

**Expect:**

```
Events             5
Photos placed      40
With GPS           24
Largest event      16 photos
Low confidence     1  (dated from file mtimes, not the camera)
```

**40 placed + 2 unreadable = 42.** No photo is silently dropped.

**Proves:** photos are grouped by time gaps and, where GPS exists, by distance.
Photos without GPS join an event on time alone. The low-confidence event is the
set of files dated only by their modification times — which were all written by
the generator within the same second.

---

## 18. Search and photo detail — Phase 9

```powershell
docker compose exec api photo-organizer find $lib -sort quality -limit 4
```

**Expect** the worst-measured photos first:

```
42 matching, showing 1-4, sorted by quality asc

PATH                                    SIZE        DIMS         QUAL   CAPTURED          MARKS
scene12-dark.jpg                        52.3 KB     640x480      0.263  2026-03-14 09:00  similar possibly_underexposed
trip/day1/scene20-bright.jpg            48.2 KB     640x480      0.352  2026-03-14 09:01  similar possibly_overexposed
trip/day1/scene20-dark.jpg              48.9 KB     640x480      0.358  2026-03-14 09:01  similar possibly_underexposed
trip/scene19-blurry.jpg                 15.8 KB     640x480      0.373  2026-03-14 23:53  gps possibly_blurry
```

One photo in full, with every group it belongs to:

```powershell
$photo = (Invoke-RestMethod "http://localhost:8080/api/libraries/$lib/photos/search?q=scene02.jpg&limit=1").photos[0].id
docker compose exec api photo-organizer photo $photo
```

A bad sort key is an error, never a silent default:

```powershell
try { Invoke-RestMethod "http://localhost:8080/api/libraries/$lib/photos/search?sort=nonsense" -ErrorAction Stop } catch { $_.ErrorDetails.Message }
```

**Expect:**

```json
{"error":{"code":"invalid_request","message":"unknown sort \"nonsense\"; valid: captured_at, created_at, file_size, path, quality"}}
```

**Proves:** `ORDER BY` cannot take a bind parameter, so sort keys are a closed
allow-list — the caller's string never reaches SQL.

---

## 19. Thumbnails and originals — Phase 10

```powershell
curl.exe -s -D - -o NUL "http://localhost:8080/api/photos/$photo/thumbnail" | Select-String "HTTP/|Content-Type|ETag|Cache-Control|X-Content-Type"
```

**Expect:**

```
HTTP/1.1 200 OK
Cache-Control: private, max-age=86400
Content-Type: image/jpeg
Etag: "<photo-id>-<timestamp>"
X-Content-Type-Options: nosniff
```

Now try to escape. The dots are **percent-encoded on purpose**:

```powershell
foreach ($id in @('%2e%2e%2f%2e%2e%2fetc%2fpasswd','not-a-uuid','00000000-0000-0000-0000-000000000000')) {
  "{0,-34} thumbnail {1}  original {2}" -f $id,
    (curl.exe -s -o NUL -w "%{http_code}" "http://localhost:8080/api/photos/$id/thumbnail"),
    (curl.exe -s -o NUL -w "%{http_code}" "http://localhost:8080/api/photos/$id/original")
}
```

**Expect `404` for all six.**

> **Why encoded.** Written as plain `../../etc/passwd`, curl resolves the dots
> *before sending*, so the request is really for `/etc/passwd/thumbnail` — which
> the web UI's router answers with its own page and a `200`. That tests curl, not
> this server, and the `200` looks alarming. Encoded dots reach the photo
> handlers unchanged, which is the case worth testing. (Sent raw with
> `--path-as-is`, Go's router redirects before any handler runs.)

**Proves:** files are only ever addressed by photo id. Thumbnail filenames are
derived from a validated UUID, and originals re-check that they resolve inside
the library on every request.

---

## 20. The web UI — Phase 10

Open **<http://localhost:8080>**.

- The library from step 8 is listed. Adding `/etc` is refused with the API's own
  message.
- **Overview** shows the same counts as steps 12–17: 42 photos, 3 exact and 6
  near-duplicate groups, 12 flagged, 5 events.
- **Review → Near-duplicates** shows each group's suggested keep with the
  measurements behind it. There is no delete button — the only action copies the
  other paths to your clipboard.
- **Photos** — click *Any quality flag* and sort by quality; 12 photos. Click one:
  a detail panel opens, the URL gains `?photo=`, and the browser's back button
  closes it.
- **Timeline** — 5 events, one labelled *Low confidence*.

**Proves:** the whole pipeline, end to end, as a user sees it — served by the API
from the same origin under a strict Content-Security-Policy.

---

## 21. Reset the fleet

```powershell
docker compose up -d --scale worker=1
```

---

## 22. Crash recovery and benchmarks — Phase 11

The benchmark tool generates a separate 12-megapixel corpus under
`sample-photos/_bench`, kills a worker holding leases with SIGKILL partway through
a run, and verifies that nothing was lost. Takes a few minutes; generating the
corpus is most of it the first time. **Keep the machine awake** — a laptop that
sleeps mid-run can take Docker Desktop down with it.

```powershell
go run ./cmd/bench crash
```

**Expect** a report ending like this, with `Lost 0`, `Dead jobs 0` and
**Verified**:

```
| Executions interrupted by the kill | 4 |
| Re-run and completed by another worker | 4 |
| Lost | 0 |
| Kill to reclaim, per job | median 70.2 s (range 70.2–70.2 s) |
| Dead jobs at the end | 0 of 343 |
```

The reclaim time should fall between about 57 and 80 seconds; the report derives
that window step by step from the lease, reaper and backoff timings.

The throughput benchmark takes about 13 minutes:

```powershell
go run ./cmd/bench pipeline -workers 1,2,4,8,16
```

Afterwards, **remove the benchmark corpus** — otherwise a rescan of `/photos`
would include its 68 large images:

```powershell
go run ./cmd/bench clean
```

Results from the reference machine, and how to read them, are in
[benchmarks/README.md](benchmarks/README.md).

---

## 23. Automated tests

**Unit tests** — no database:

```powershell
go test ./...
```

**Expect:** 14 packages `ok`. Two symlink tests skip on Windows, where creating a
symlink needs elevation; `make test` runs them in a Linux container.

**Web tests:**

```powershell
cd web
npm ci
npm test
npm run build
cd ..
```

**Expect:** `Tests 48 passed`, then a successful build — which typechecks first.

**Integration tests** — against a real Postgres, in a separate
`photoorganizer_test` database so they never touch your library. They run in a Go
container on the Compose network:

```powershell
docker compose exec postgres psql -U photo -d postgres -c "CREATE DATABASE photoorganizer_test"
docker run --rm --network photo-organizer_default -v "${PWD}:/src" -v photo-organizer-gomod:/go/pkg/mod -w /src -e TEST_DATABASE_URL="postgres://photo:photo@postgres:5432/photoorganizer_test?sslmode=disable" golang:1.25-alpine go test -tags integration -count=1 -p 1 -timeout 15m ./...
```

(`database "photoorganizer_test" already exists` from the first command is
harmless.)

**Expect:** 16 packages `ok`, now including `internal/ingest` and
`internal/worker` — the job queue's claim, lease, reaper and crash-recovery
tests.

> **Why a container rather than `go test` on the host:** if another PostgreSQL
> is listening on the host's port 5432, a host-side test connects to *that*
> server instead and fails authentication — which is exactly what happened on the
> development machine.

---

## Teardown

```powershell
docker compose down       # keep data
docker compose down -v    # also delete the database and thumbnail volumes
```
