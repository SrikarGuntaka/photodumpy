# Architecture

How the system is put together. For *why* each choice was made, see
[DESIGN_DECISIONS.md](DESIGN_DECISIONS.md).

> Sections describing phases that are not yet built are marked **(planned)**.
> They are documented now because the Phase 1 schema and package boundaries were
> shaped by them.

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
knowing the tree before walking it. Instead the API streams the walk, inserting
rows and fanning out per-photo jobs as it goes.

Paths are stored **relative** to `libraries.root_path` and normalised to forward
slashes. Moving or remounting the library is then a one-row update rather than a
table rewrite, and Windows/Linux path differences stop at the ingestion
boundary.

**PhotoKit (future).** `libraries.source_kind` already distinguishes
`local_fs` from `ios_photokit`. For an iOS source, `relative_path` holds a
PhotoKit local identifier instead of a path. Everything downstream operates on
rows and on an `io.Reader`, never on `os.Open(absolutePath)`, so the swap is one
interface implementation rather than a rewrite.

## 3. Job execution **(planned — Phase 5)**

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
        └───────────      dead       (manual requeue → pending)
```

Four resting states: `pending`, `running`, `succeeded`, `dead`. A retryable
failure returns the row to `pending` with a future `next_attempt_at` rather than
sitting in a separate `failed` state — which is what keeps the claim query a
single indexed read.

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

## 4. Worker lifecycle **(planned — Phase 5)**

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

1. **Semaphore** of `WORKER_CONCURRENCY` (default `NumCPU`) around job execution
   — caps goroutines and, more importantly, caps how many decoded images are
   resident at once.
2. **Connection pool** of `WORKER_CONCURRENCY + 4` — caps database contention.
   The `+4` reserves headroom so heartbeat, renewal and reaper cannot be starved
   by job work.
3. **Claim batch size** = free slots only.

There is never one goroutine per photo. The queue is the backpressure mechanism.

## 5. Data persistence

Postgres holds paths and derived data. **Image bytes are never stored in the
database.** Originals stay on disk, read-only; thumbnails are written to a
separate volume.

Current schema (Phase 1):

```
libraries ──< photos
```

Target schema:

```
libraries ──< photos ──< duplicate_group_members >── duplicate_groups
                 │
                 ├──< cluster_photos >── clusters
                 │
                 └──< jobs (target_type = 'photo' | 'library')      workers
```

Derived metrics (sharpness, exposure, hashes) live as columns on `photos` rather
than in side tables: the relationship is strictly 1:1, there is no history to
keep, and every read query wants them.

Columns are added by the migration for the phase that first writes them, so the
schema never contains columns no code touches.

## 6. Processing stages **(planned)**

### Per-photo jobs — parallel, one row each

| Job | Phase | Writes |
|-----|-------|--------|
| `EXTRACT_METADATA` | 3 | dimensions, capture time, GPS, format |
| `COMPUTE_FILE_HASH` | 4 | `sha256` (streamed, never fully buffered) |
| `GENERATE_THUMBNAIL` | 6 | `thumbnail_path` |
| `COMPUTE_PERCEPTUAL_HASH` | 6 | `phash` |
| `ANALYZE_QUALITY` | 7 | sharpness, exposure, contrast, quality score |

Each is a deterministic function of file bytes whose completion is an
`UPDATE photos SET ... WHERE id = $1`. Running one twice writes identical
values. That is what makes at-least-once delivery safe.

### Aggregate stages — one job per *library*

| Stage | Phase | Why not per-photo |
|-------|-------|-------------------|
| `BUILD_DUPLICATE_GROUPS` | 4 | Needs a global `GROUP BY sha256`; sharding means merging partial groups |
| `BUILD_PHOTO_CLUSTERS` | 8 | Inherently sequential — a cluster boundary depends on neighbouring photos |
| `RANK_SIMILAR_PHOTOS` | 6 | Operates on already-formed groups |

These are still ordinary rows in the same queue, with the same leases, retries
and crash recovery — just `target_type = 'library'`. They achieve idempotency by
rebuilding their output for the library inside one transaction
(`DELETE WHERE library_id = $1`, then insert).

## 7. Clustering **(planned — Phase 8)**

Sort by capture time, sweep, and cut a boundary when either:

- the time gap to the previous photo exceeds `CLUSTER_TIME_GAP` (default 4h), or
- the Haversine distance exceeds `CLUSTER_DISTANCE_KM` (default 25km)

Both thresholds are configurable.

**Photos without GPS** — a large fraction of any real library — are clustered on
time alone and inherit a location from temporally adjacent photos that do have
one, but only when those neighbours agree with each other. Where they do not,
the cluster is left without a location rather than guessing.

**Photos without any timestamp** (no EXIF, unusable mtime) go into a dedicated
"undated" bucket rather than being forced into a chronology they would corrupt.

## 8. Failure behaviour

| Failure | Expected behaviour |
|---------|--------------------|
| Worker crashes mid-job | Lease expires; reaper returns the job to `pending`; another worker runs it. Handlers are idempotent, so partial work is overwritten. |
| Worker shut down cleanly | Leases released immediately; work reassigned without waiting out the lease. |
| API restarts | Stateless. In-flight HTTP requests drain within the shutdown grace period. Jobs already enqueued are unaffected. |
| Postgres restarts | Connection pool retries with capped backoff. Workers stop claiming and resume; jobs in flight lose their lease renewal and are reclaimed. The API stays up — `/healthz` deliberately does not check the database. |
| Malformed image | Job fails permanently after `max_attempts`; photo marked `failed` with the reason. Other photos are unaffected. |
| Corrupt or absent EXIF | Not an error. `captured_at` falls back to filesystem mtime, recorded via `captured_at_source`. |
| Missing GPS | Not an error. Latitude/longitude stay `NULL` — never `(0,0)`, which is a real location. |
| Missing timestamp | Photo is excluded from chronological clustering and placed in the undated bucket. |
| Duplicate job execution | Safe by construction — see idempotency above. |
| File deleted after scan | Photo marked `missing`, job **succeeds**. A deleted file is not a retryable condition, and retrying it five times is waste. |
| File unreadable (permissions) | Retried — this *is* transient. Marked `failed` after `max_attempts`. |
| Job exceeds its lease | Reclaimed and re-run. The original worker detects its failed renewal and abandons its own execution. |
| Scan interrupted midway | Inserts are idempotent on `(library_id, relative_path)`; re-running resumes. `last_scan_finished_at` stays NULL so the UI can say the scan was incomplete. |

## 9. Package layout

```
cmd/{api,worker,cli}          process entry points
internal/config               env -> typed config, validated at startup
internal/database             pgx pool, migration runner
internal/store                ALL SQL. Nothing else imports pgx.
internal/jobs                 queue: claim, lease, retry, reap, registry
internal/worker               runtime: pool, heartbeat, renewal, shutdown
internal/photos               domain types + PhotoSource seam
internal/{metadata,hashing,quality,duplicates,clustering}
                              pure algorithms — no database import
internal/api                  HTTP handlers only
migrations/                   embedded .sql
```

The rule that makes this testable: the algorithm packages take values and return
values. Haversine, clustering, blur detection and hash comparison are unit
tested with synthetic input and no Postgres anywhere.
