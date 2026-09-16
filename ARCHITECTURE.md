# Architecture

How the system is put together. For *why* each choice was made, see
[DESIGN_DECISIONS.md](DESIGN_DECISIONS.md).

> Every phase described here is built. Section headings still note the phase
> that implemented each part, which is useful for reading the git history
> alongside this document.

---

## 1. Processes

Three, and only three:

```
                    ┌──────────────┐   HTTP    ┌──────────────────────────┐
                    │ React + Vite │──────────▶│  api  (cmd/api)          │
                    │ CLI          │           │  - libraries, scan       │
                    └──────────────┘           │  - read models           │
                                               │  - enqueues jobs         │
                                               │  - owns migrations       │
                                               └────────────┬─────────────┘
                                                            │ SQL
                                                   ┌────────▼─────────┐
                                                   │   PostgreSQL     │
                                                   │  state + queue   │
                                                   └────────┬─────────┘
                             claim: SELECT ... FOR UPDATE SKIP LOCKED
                          ┌─────────────────────────┼─────────────────────────┐
                    ┌─────▼─────┐             ┌─────▼─────┐             ┌─────▼─────┐
                    │ worker-1  │             │ worker-2  │     ...     │ worker-N  │
                    └─────┬─────┘             └───────────┘             └───────────┘
                          │ read-only
                    ┌─────▼──────────────┐
                    │ photos on disk     │
                    └────────────────────┘
```

`api` and `worker` are built from the same image and share almost all of their
code. The worker handles *every* job type; job type is a column, not a
deployment unit.

## 2. Ingestion

**Local filesystem (today).** A `library` points at a directory. Scanning walks
it recursively, and for each supported file inserts a `photos` row keyed by
`(library_id, relative_path)`.

The scan is *not* a job. Splitting a directory walk into jobs would require
knowing the tree before walking it. Instead the API streams the walk and inserts
rows. Work is fanned out separately, by `POST /api/libraries/{id}/process`,
which enqueues one job per photo per stage that has not yet completed, plus the
library-wide aggregate stages. Keeping the two apart means a rescan never
enqueues anything, and processing always sees the whole library rather than
whatever fraction a scan-in-progress had reached.

### How a scan actually runs (implemented)

```
POST /api/libraries          -> resolve path, verify inside PHOTO_ROOT, upsert row
POST /api/libraries/{id}/scan
      |
      |-- re-validate the stored root (PHOTO_ROOT may have changed since)
      |-- MarkScanStarted: UPDATE ... WHERE (no scan running)   <- the guard
      |     0 rows -> 409, a scan is already running
      |-- spawn goroutine, return 202 immediately
      |
      +-> photos.Walk streams entries
            accumulate 500 -> INSERT ... SELECT unnest(...) ON CONFLICT DO NOTHING
            repeat until the walk ends
          MarkScanFinished (always, even on cancellation)
```

Three properties this shape buys:

- **Bounded memory.** The walk streams and flushes every 500 entries, so peak
  memory is the same for a 100-photo library and a 100,000-photo one.
- **Bounded round trips.** Batched inserts make a 10,000-photo scan ~20 round
  trips instead of 10,000. Arrays are passed as parameters and unnested
  server-side, not interpolated into SQL.
- **Idempotency comes from the database.** The unique index on
  `(library_id, relative_path)` is the authority on "have we seen this file".
  `ON CONFLICT DO NOTHING` means a rescan inserts nothing, and `RowsAffected`
  proves it rather than the code asserting it.

Scanning is asynchronous because a large library takes longer than a sensible
HTTP timeout. Progress is polled from `GET /api/libraries/{id}`, which reads the
same counts the scan is writing — there is no separate progress channel that
could disagree with the database.

**Crash recovery for scans.** A process that dies mid-scan leaves its library
marked `scanning` forever, which would make every future scan return 409. The
API reconciles this at startup: a fresh process owns no scans by definition, so
any library still marked running was interrupted. `?force=true` is the manual
override for the same situation.

### Path containment

Every path from a client passes `photos.ResolveWithin` before anything touches
the filesystem. Both the root and the target are made absolute and
symlink-resolved *before* comparison, and containment is tested with
`filepath.Rel` rather than a string prefix.

Each of those details is load-bearing:

| Attack | Defeated by |
|--------|-------------|
| `../../etc/passwd` | resolution + `Rel` segment check |
| absolute path elsewhere | same |
| `/photos-evil` vs `/photos` | `Rel` instead of `HasPrefix` |
| symlink inside root -> outside | `EvalSymlinks` **before** comparing |
| Windows vs POSIX separators | `filepath` throughout, stored as forward slashes |

The error returned never echoes the resolved path, because on a traversal
attempt that would confirm what exists outside the root.

Paths are stored **relative** to `libraries.root_path` and normalised to forward
slashes. Moving or remounting the library is then a one-row update rather than a
table rewrite, and Windows/Linux path differences stop at the ingestion
boundary.

**PhotoKit (future).** `libraries.source_kind` already distinguishes
`local_fs` from `ios_photokit`. For an iOS source, `relative_path` holds a
PhotoKit local identifier instead of a path. Everything downstream operates on
rows and on an `io.Reader`, never on `os.Open(absolutePath)`, so the swap is one
interface implementation rather than a rewrite.

## 3. Job execution **(implemented — Phase 5)**

### State machine

```
                        enqueue
                           │
                           ▼
        ┌──────────────▶ pending ◀───────────────┐
        │                  │                     │
        │      claim (FOR UPDATE SKIP LOCKED)    │
        │                  ▼                     │
        │               running ─────────────────┤ lease expired
        │              /   │   \                 │ (reaper: attempt++,
        │  success    /    │    \  failure       │  next_attempt_at = backoff)
        │            ▼     │     ▼               │
        │      succeeded   │   attempt < max ────┘
        │                  │
        │                  ▼
        │            attempt >= max
        │                  ▼
        └───────────      dead
```

Four resting states: `pending`, `running`, `succeeded`, `dead`. A retryable
failure returns the row to `pending` with a future `next_attempt_at` rather than
sitting in a separate `failed` state — which is what keeps the claim query a
single indexed read.

A `dead` job is terminal; there is no requeue operation. Recovery comes from
calling `POST /process` again: its dedupe index covers only pending and running
jobs, and a photo whose stage never completed still has that stage's marker
column unset, so it simply receives a fresh job.

### Claiming

```sql
UPDATE jobs SET
    status           = 'running',
    leased_by        = $1,
    lease_expires_at = now() + $2::interval,
    attempt_count    = attempt_count + 1,
    started_at       = COALESCE(started_at, now())
WHERE id IN (
    SELECT id FROM jobs
    WHERE status = 'pending' AND next_attempt_at <= now()
    ORDER BY priority, id
    LIMIT $3
    FOR UPDATE SKIP LOCKED     -- concurrent workers skip each other's rows
                               -- instead of blocking on them
)
RETURNING *;
```

`SKIP LOCKED` is what makes N workers scale: without it, every worker queues
behind the same first row and throughput is flat regardless of worker count.

### Retries

`next_attempt_at = now() + min(base * 2^attempt, cap)` with jitter.
Base 2s, cap 5m. The jitter matters because a Postgres restart fails every
in-flight job at the same instant; without it they would all retry together and
do it again.

## 4. Worker lifecycle **(implemented — Phase 5)**

```
start
  ├─ INSERT INTO workers (id, hostname, pid, started_at)
  ├─ go heartbeat loop      (5s:  UPDATE workers SET last_heartbeat_at = now())
  ├─ go lease renewal loop  (5s:  extend lease_expires_at for held jobs)
  ├─ go reaper loop         (15s: reclaim expired leases; mark dead workers)
  └─ claim loop:
       wait for a free slot  ──────────────── bounded by a semaphore
       claim up to (free slots) jobs         a worker never holds a lease on
       dispatch each into the pool           work it is not actually running

SIGTERM
  ├─ stop claiming
  ├─ cancel in-flight job contexts, wait up to ~30s
  ├─ release leases still held  ->  instant reassignment, no lease wait
  ├─ UPDATE workers SET status = 'stopped'
  └─ exit 0
```

Lease is 60s, renewed every 5s — a 12x margin. A worker that is alive but cannot
reach Postgres will fail its renewals, notice, and abandon its own work rather
than double-writing while another worker legitimately takes over.

### Bounded concurrency

Three independent limits, because photo processing saturates different
resources:

1. **Semaphore** of `PROCESS_CONCURRENCY` (default `NumCPU`) around job execution
   — caps goroutines and, more importantly, caps how many decoded images are
   resident at once.
2. **Connection pool** of `PROCESS_CONCURRENCY + 4` — caps database contention.
   The `+4` reserves headroom so heartbeat, renewal and reaper cannot be starved
   by job work.
3. **Claim batch size** = free slots only.

There is never one goroutine per photo. The queue is the backpressure mechanism.

## 5. Data persistence

Postgres holds paths and derived data. **Image bytes are never stored in the
database.** Originals stay on disk, read-only; thumbnails are written to a
separate volume.

Schema after all nine migrations:

```
libraries ─┬─< photos ─┬─< duplicate_group_members >─ duplicate_groups ─┐
           │           ├─< similar_group_members   >─ similar_groups   ─┤
           │           └─< cluster_members         >─ clusters         ─┤
           │                                                            │
           ├─< duplicate_groups / similar_groups / clusters  <──────────┘
           │
           └─< jobs ──< job_executions
                 │
                 └── leased_by >── workers

schema_migrations   (applied versions + checksums; see migrations/)
```

Every library-scoped table cascades from `libraries`, so removing a library
removes its photos, groups, clusters, jobs and job history in one statement.
`workers` and `schema_migrations` are global.

`jobs.target_id` names a photo or the library itself, depending on
`target_type`; it is deliberately not a foreign key, because one column refers
to two tables. `jobs.leased_by` is one, with `ON DELETE SET NULL`: removing a
worker row must never delete the work it held. `job_executions.worker_id` is
not a foreign key at all — the execution audit trail must outlive the worker
rows it refers to.

`cluster_members.photo_id` is `UNIQUE` — a photo belongs to at most one event —
while a photo may be in a duplicate group *and* a similar group, which is why
membership lives in join tables rather than columns on `photos`.

Derived metrics (sharpness, exposure, hashes) live as columns on `photos` rather
than in side tables: the relationship is strictly 1:1, there is no history to
keep, and every read query wants them.

Columns are added by the migration for the phase that first writes them, so the
schema never contains columns no code touches.

## 6. Processing stages

### Metadata extraction (implemented, Phase 3)

```
POST /api/libraries/{id}/metadata
      |
      +-> claim 200 rows WHERE metadata_extracted_at IS NULL   <- partial index
          for each: acquire semaphore slot  (cap = PROCESS_CONCURRENCY)
                    open file, re-stat, read header + EXIF
                    UPDATE photos SET ... , metadata_extracted_at = now()
          repeat until no rows remain
```

Dimensions come from `image.DecodeConfig`, which reads the header only -- a few
hundred bytes rather than decoding a 12-megapixel image.

The semaphore slot is acquired *before* the goroutine is spawned, so the number
of live goroutines is capped rather than merely their throughput. Batching the
row claim keeps memory independent of library size.

`ExtractOne` is a standalone method precisely so the `EXTRACT_METADATA`
handler can call it unchanged, which is what it now does; the queue supplies
the leases, retries and crash recovery that the local loop provided before.

Failure handling separates three cases that look alike:

| Condition | Outcome | Why |
|-----------|---------|-----|
| File deleted since scan | state `missing`, job **succeeds** | Retrying a file that does not exist is waste |
| Undecodable image | state `failed`, permanent | Retrying will not change the bytes |
| Permission denied | returned for retry | May genuinely resolve |

`metadata_extracted_at` is set in every terminal case, including failure --
otherwise the processor would retry the same corrupt file forever.

### Per-photo jobs — parallel, one row each

| Job | Phase | Writes |
|-----|-------|--------|
| `EXTRACT_METADATA` | 3 | dimensions, capture time, GPS, format *(implemented)* |
| `COMPUTE_FILE_HASH` | 4 | `sha256` (streamed, never fully buffered) *(implemented)* |
| `COMPUTE_PERCEPTUAL_HASH` | 6 | `phash` *(implemented)* |
| `ANALYZE_QUALITY` | 7 | sharpness, exposure, contrast, resolution, quality score, flags *(implemented)* |
| `GENERATE_THUMBNAIL` | 10 | a 400px oriented JPEG on the thumbnail volume, named by photo id; `thumbnail_width/height/bytes` *(implemented)* |

Each is a deterministic function of file bytes whose completion is an
`UPDATE photos SET ... WHERE id = $1`. Running one twice writes identical
values. That is what makes at-least-once delivery safe.

### Aggregate stages — one job per *library*

| Stage | Phase | Why not per-photo |
|-------|-------|-------------------|
| `BUILD_DUPLICATE_GROUPS` | 4 | Needs a global `GROUP BY sha256`; sharding means merging partial groups *(implemented)* |
| `BUILD_CLUSTERS` | 8 | Inherently sequential — a cluster boundary depends on neighbouring photos *(implemented)* |
| `BUILD_SIMILAR_GROUPS` | 6 | Grouping is transitive; needs every hash at once *(implemented)* |

These are still ordinary rows in the same queue, with the same leases, retries
and crash recovery — just `target_type = 'library'`. They achieve idempotency by
rebuilding their output for the library inside one transaction
(`DELETE WHERE library_id = $1`, then insert).

## 7. Clustering **(implemented — Phase 8)**

Sort by capture time, sweep, and cut a boundary when either:

- the time gap **to the previous photo** exceeds `CLUSTER_MAX_GAP` (default 4h), or
- the Haversine distance **from the cluster's first GPS fix** exceeds
  `CLUSTER_MAX_RADIUS_METERS` (default 1500m)

Both thresholds are configurable. The asymmetry between them is deliberate and
is explained in [DESIGN_DECISIONS.md](DESIGN_DECISIONS.md) §18: time is
inherently a gap between neighbours, while anchoring the distance is what stops
small steps chaining into a cluster that spans a county.

Two details differ from the original sketch, and the sketch was wrong:

- The distance threshold is **1500m, not 25km**. 25km would place two different
  towns in one event. 1500m is roughly a fifteen-minute walk — wide enough that
  consumer GPS error never splits anything, narrow enough to separate two stops
  on the same trip.
- The default is expressed in **metres, not kilometres**, because the values
  that matter are all under a kilometre.

**Photos without GPS** — a large fraction of any real library — are clustered on
time alone. They join the event their timestamp puts them in and take no part in
the distance test.

They do **not** inherit a location from their neighbours. The original sketch
proposed that; it is deliberately not implemented. Inheriting would mean
recording where a photo was taken when the camera never recorded it, in a column
indistinguishable from a real fix. The cluster's own anchor already lets the UI
say "this event was around here" without making a claim about any individual
photo. Per-member distance is `NULL` for an unlocated photo, never `0`.

**Photos without any timestamp** (no EXIF, unusable mtime) are not clustered and
not dropped. They are counted as `undated`, so the totals reconcile exactly:
clustered + undated + missing/failed = every photo in the library.

**Clusters dated from mtimes are labelled.** Each cluster counts how many members
came from a camera timestamp rather than a filesystem mtime and reports
`confidence` as high, mixed or low — because a folder copy stamps every file
within a second or two, which would otherwise present a whole library as one
confident "event".

## 8. Failure behaviour

| Failure | Expected behaviour |
|---------|--------------------|
| Worker crashes mid-job | Lease expires; reaper returns the job to `pending`; another worker runs it. Handlers are idempotent, so partial work is overwritten. |
| Worker shut down cleanly | Leases released immediately; work reassigned without waiting out the lease. |
| API restarts | Stateless. In-flight HTTP requests drain within the shutdown grace period. Jobs already enqueued are unaffected. |
| Postgres restarts | Connection pool retries with capped backoff. Workers stop claiming and resume; jobs in flight lose their lease renewal and are reclaimed. The API stays up — `/healthz` deliberately does not check the database. |
| Malformed image | Recorded on the **first** attempt, with no retries: the photo is marked `failed` with the reason and the job completes. Retrying cannot change the bytes, so spending `max_attempts` on it would only delay the report. Other photos are unaffected. |
| Corrupt or absent EXIF | Not an error. `captured_at` falls back to filesystem mtime, recorded via `captured_at_source`. |
| Missing GPS | Not an error. Latitude/longitude stay `NULL` — never `(0,0)`, which is a real location. |
| Missing timestamp | Photo is excluded from chronological clustering and counted as `undated`. Not dropped -- the totals reconcile. |
| Duplicate job execution | Safe by construction — see idempotency above. |
| File deleted after scan | Photo marked `missing`, job **succeeds**. A deleted file is not a retryable condition, and retrying it five times is waste. |
| File unreadable (permissions) | Retried — this *is* transient. Marked `failed` after `max_attempts`. |
| Job exceeds its lease | Reclaimed and re-run. The original worker detects its failed renewal and abandons its own execution. |
| Scan interrupted midway | Inserts are idempotent on `(library_id, relative_path)`; re-running resumes. `last_scan_finished_at` stays NULL so the UI can say the scan was incomplete. |

## 9. Package layout

```
cmd/{api,worker,cli}          process entry points
cmd/genfixtures               writes a synthetic corpus with known ground truth
cmd/bench                     pipeline benchmark and crash-recovery demo
internal/config               env -> typed config, validated at startup
internal/database             pgx pool, migration runner
internal/store                ALL SQL lives here
internal/jobs                 queue vocabulary: types, states, backoff, dedupe keys
internal/worker               runtime: claim loop, heartbeat, renewal, reaper, handlers
internal/ingest               scanning and the per-photo units the handlers call
internal/photos               domain types, path containment, directory walk
internal/{metadata,hashing,quality,duplicates,clustering,thumbnail}
                              pure algorithms — no database import
internal/fixtures             corpus generator shared by tests and benchmarks
internal/api                  HTTP handlers, media serving, the web UI's static host
migrations/                   embedded .sql
web/                          React + TypeScript UI, built by Vite
```

All SQL is in `internal/store`. The only other packages that import pgx are
`internal/database`, which owns the pool and migrations, and `internal/api`,
which holds the pool solely to ping it for `/readyz`.

The rule that makes this testable: the algorithm packages take values and return
values. Haversine, clustering, blur detection, hash comparison and thumbnail
orientation are unit tested with synthetic input and no Postgres anywhere.
