# Design decisions

Each entry records what was decided, what the alternatives were, and what the
decision costs. Decisions with no cost listed are usually decisions that were
not thought through.

---

## 1. PostgreSQL as the job queue, not Redis / RabbitMQ / Kafka

**Decision.** The queue is a `jobs` table. Workers claim rows with
`SELECT ... FOR UPDATE SKIP LOCKED` under a time-bounded lease.

**Why.**

- **One consistency domain.** Job state and photo state live in the same
  database, so "mark this job done and write its result" is one transaction. A
  separate broker makes that two systems with no shared transaction, which
  reintroduces the dual-write problem that most of this design exists to avoid.
- **Queue state is queryable.** "Show me every job that failed and why" is a
  `SELECT`. Progress reporting for the UI is a `GROUP BY`. With a broker, that
  needs a parallel bookkeeping table — at which point Postgres is already the
  source of truth and the broker is redundant.
- **Durability is the default.** A job survives a restart of everything, with
  no additional configuration.
- **Postgres is already a hard dependency.** Adding a broker means a second
  stateful service to run, monitor and reason about, for a workload measured in
  thousands of jobs.

**Cost.** Throughput ceiling. A Postgres-backed queue tops out in the low tens
of thousands of jobs/second; a dedicated broker does far more. That ceiling is
several orders of magnitude above this workload — a 10,000-photo library is
50,000 jobs *total*, and each takes milliseconds-to-seconds of CPU. The
bottleneck here is image decoding, not queue throughput. If that ever changed,
the `internal/jobs` boundary is where a broker would slot in.

**Rejected.** Redis (not durable by default; a second stateful service),
RabbitMQ/Kafka (operationally heavy, and Kafka's partitioned log is the wrong
shape for "claim one unit of work with a lease"), and an in-process channel
(does not survive a crash, which defeats the point).

---

## 2. At-least-once execution, not exactly-once

**Decision.** A job may run more than once. Handlers are idempotent.

**Why.** Exactly-once across a filesystem read, an image computation and a
database write would require a distributed transaction spanning all three. It is
not achievable here, and systems that claim it usually mean "at-least-once plus
deduplication" — which is what this is, stated honestly.

The unavoidable window: a worker finishes the work, commits the result, and dies
before marking the job succeeded. The lease expires and the job runs again.

**Consequence — this drove the data model.** Every per-photo job is a
deterministic function of file bytes whose completion is
`UPDATE photos SET ... WHERE id = $1`. Re-running writes identical values.
Aggregate stages rebuild their output for a library inside one transaction, so
re-running converges to the same state. Idempotency is not a property bolted on
afterwards; it is why the per-photo / aggregate split exists at all.

**Cost.** Wasted CPU on the rare double execution, and every future handler must
be written with this constraint in mind. A handler that appends rather than
overwrites would be a bug that only shows up under crash conditions.

---

## 3. Leases rather than "claimed" flags

**Decision.** A claimed job carries `leased_by` and `lease_expires_at`. Workers
renew every 5s against a 60s lease.

**Why.** A boolean `claimed` flag has no recovery story: if the worker that set
it dies, the row stays claimed forever and the job is lost. A lease makes
liveness explicit and time-bounded — the job becomes claimable again on its own,
with no operator intervention.

Renewal (rather than one long lease) means the lease can be short enough that
recovery is fast, while still supporting jobs that legitimately run for minutes.
The 12x margin between renewal interval and lease duration tolerates transient
database slowness without spuriously losing a lease.

**Cost.** A background renewal loop per worker, and a reaper that must run
somewhere. Both are implemented, and the crash-recovery integration test exists
specifically to prove they work.

**Also.** Clean shutdown *releases* leases rather than letting them expire, so
`docker compose stop` reassigns work immediately. `docker compose kill` is the
crash path, and is what the recovery test uses.

---

## 4. Original photos stay on disk, read-only

**Decision.** The database stores paths and derived data. Image bytes are never
stored in Postgres. The photo directory is mounted `:ro`.

**Why.**

- A user's photo library is tens to hundreds of gigabytes. Copying it into
  Postgres would multiply storage for no benefit, and make backup and restore
  absurd.
- Photos are read sequentially by path and never queried by content. There is
  nothing a database gives us here.
- **The read-only mount is the real enforcement of "never modify originals."**
  Documentation and code review can be wrong; the kernel cannot. A bug that
  tries to write to an original fails with `EROFS` rather than destroying
  someone's photos.

**Cost.** The database can hold a path that no longer exists. Handled
explicitly: every handler re-`stat`s before reading, and a missing file marks
the photo `missing` rather than retrying.

---

## 5. Cleanup is suggestion-only; nothing is ever deleted

**Decision.** Duplicates and low-quality photos are surfaced with scores and
reasons. The application has no delete path.

**Why.** The cost asymmetry is extreme and one-directional. Failing to flag a
duplicate wastes a few megabytes. Deleting the only copy of a photo someone
cared about is unrecoverable. No heuristic is good enough to justify that risk
automatically, and "possibly blurry" is a statement about Laplacian variance,
not about whether a photo matters to its owner.

**Cost.** The product does not actually free disk space on its own — it tells
you what you could remove. That is the correct trade for a tool operating on
irreplaceable personal data.

---

## 6. Perceptual hashing for near-duplicates

**Decision.** dHash (difference hash): downsample to 9x8 greyscale, compare
horizontally adjacent pixels, emit 64 bits. Similarity is Hamming distance,
exposed in the API, with a configurable threshold (default 10).

**Why dHash over the alternatives.**

| Approach | Verdict |
|----------|---------|
| **aHash** (average) | Simplest, but too sensitive to brightness/contrast shifts — exactly what recompression and light editing change. |
| **dHash** (gradient) | **Chosen.** Compares *relative* pixel changes, so it survives brightness and contrast adjustment. Cheap, fully interpretable, no training data, no dependencies. |
| **pHash** (DCT) | More robust to rotation and heavier edits, at ~10x the cost and much harder to explain. Deferred; the seam exists if it is needed. |
| **Learned embeddings** | Better recall on semantic similarity, but requires a model, is not interpretable, and would make "why are these two grouped?" unanswerable. Also out of scope by requirement. |

**Interpretability was the deciding factor.** When the UI says two photos are
similar, it can show the exact bit distance. A user can move the threshold and
watch groupings change predictably. That is impossible with an embedding.

**Known weaknesses, stated plainly.** dHash is not rotation-invariant — a
rotated copy will not match. It is weak on heavily cropped images. It can
collide on flat, low-detail images (blank walls, dark frames), which is why
quality metrics are considered alongside it rather than distance alone.

---

## 7. Near-duplicate search: brute force first, bucketing second

**Decision.** Phase 6 ships O(n²) comparison. The optimisation is designed but
not built.

**Why.** 10,000 photos is 50M comparisons, each a 64-bit XOR and a popcount.
That is single-digit seconds in Go. Building an index first would be optimising
a bottleneck that has not been measured and may not exist at this scale.

**The optimisation path, when needed.** Split each 64-bit hash into 4x16-bit
bands and index each band. Two hashes within Hamming distance 3 must share at
least one identical band (pigeonhole principle: 4 bands, at most 3 differing
bits, so one band is untouched). Candidate generation becomes 4 indexed lookups
instead of a full scan, with no false negatives below the threshold.

**Cost, stated honestly.** At the scales this project can realistically test,
the speedup may be "3s to 0.2s" rather than order-of-magnitude. The O(n²)
behaviour will be documented and measured rather than hidden, and the
optimisation will only be built if a measurement justifies it.

---

## 8. Objective defects, not aesthetic judgement

**Decision.** Quality analysis reports sharpness (Laplacian variance), exposure
(histogram clipping), contrast and resolution. It flags `possibly_blurry`,
`possibly_underexposed`, and similar. It never says a photo is good or bad.

**Why.** Blur is measurable. Beauty is not. A shallow depth-of-field portrait
scores as "blurry" on Laplacian variance because most of the frame *is* blurry —
and it may be the best photo in the library. Motion blur is sometimes the point.

Conflating "this frame has low high-frequency energy" with "this is a bad photo"
would produce confidently wrong recommendations about someone's memories.

**Consequence.** Every flag is hedged in its own name (`possibly_`), thresholds
are configurable, raw scores are always exposed alongside flags, and the UI
presents these as candidates for review rather than as verdicts.

---

## 9. Clustering thresholds are configurable, and the algorithm stays simple

**Decision.** A chronological sweep that cuts on a time gap (default 4h) or a
Haversine distance jump (default 25km).

**Why not DBSCAN or a learned model.** A single-pass sweep is explainable to a
user in one sentence and debuggable from the data. "Why are these separate?"
has a concrete answer: the gap was 6 hours and the threshold is 4. DBSCAN's
`eps` and `minPts` interact in ways that are hard to explain and hard to tune by
hand, for output that would not be meaningfully better on this data.

**The genuinely hard part is missing data, not the algorithm.** Real libraries
have photos with no GPS, no EXIF, or timestamps that are plainly wrong. The
policy is deliberately conservative: cluster on time alone when GPS is absent;
inherit a location from temporal neighbours only when those neighbours agree;
leave clusters unlocated rather than guess; and put photos with no usable
timestamp in a separate undated bucket rather than forcing them into a
chronology they would corrupt.

**Known limitation.** EXIF timestamps carry no timezone. A photo taken at 10am
in Tokyo and one at 10am in Austin are indistinguishable in ordering. This is not
solved, it is documented — `captured_at_source` records what we actually know.

---

## 10. Migrations run in the API, guarded by an advisory lock

**Decision.** The API applies migrations at startup, holding
`pg_advisory_lock`. Workers never migrate.

**Why.** A separate migration container has to be sequenced by hand and is easy
to forget. Running them in-process means the schema is always current when the
API serves its first request. The advisory lock makes concurrent API replicas
safe: the loser blocks, then finds nothing to apply.

Workers are excluded so schema changes have exactly one owner, and so scaling to
16 workers cannot cause a migration stampede.

**Also.** Applied migrations are checksummed. Editing one that already ran is a
startup error, not a silent divergence between the schema and the repo.

**Cost.** Startup does slightly more work, and a migration that takes a table
lock would delay it. Acceptable for a local single-user tool; a large deployment
would split this out.

---

## 11. The CLI is an HTTP client, not a second implementation

**Decision.** `photo-organizer` talks to the API. It has no database
connection.

**Why.** Two code paths to the same data diverge — the CLI grows a slightly
different definition of "duplicate" and the two disagree. Going through the API
means one implementation, and the CLI keeps working unchanged if the API moves
to another machine. It also exercises the same endpoints the frontend uses, so
CLI testing is API testing.

**Cost.** The API must be running to use the CLI. For a tool whose entire
purpose is orchestrating a running system, that is not a real constraint.

---

## 12. Path containment as a security boundary

**Decision.** `PHOTO_ROOT` is resolved to an absolute, symlink-free path at
startup. Every library path is validated to sit inside it. In Docker, that root
is a read-only mount.

**Why.** The API accepts a directory to scan. Without validation, that is an
arbitrary-file-read primitive — `../../` or a symlink pointing at `/etc` or a
home directory. This is a local tool today, but "it's only local" is how these
become vulnerabilities later, and the future iOS client means the API will not
always be local.

Symlinks are resolved at config time specifically because a symlink *inside* the
root pointing *outside* it would otherwise pass a naive prefix check.

**Cost.** Photos must live under one tree. Multiple disparate folders need
multiple mounts. Worth it.

---

## 13. Derived metrics as columns on `photos`, not side tables

**Decision.** `sharpness_score`, `sha256`, `phash` and the rest are columns on
`photos`.

**Why.** The relationship is strictly 1:1, there is no history to retain, and
essentially every read query wants these values. A `quality_metrics` table would
add a join to every list endpoint in exchange for normalisation purity that buys
nothing here.

**Cost.** A wide table, and `ALTER TABLE ADD COLUMN` in several migrations as
phases land. Both are cheap in Postgres (adding a nullable column is O(1)).

**Where this would change.** If metrics ever needed versioning — "score this
photo with algorithm v2 while keeping v1" — a side table becomes correct. That
is not a requirement, and building for it now would be speculative.

---

## 14. Columns are added by the phase that writes them

**Decision.** Migration `0001` creates only what the scan populates. Metadata,
hash and quality columns arrive with their phases.

**Why.** A schema full of columns nothing writes is indistinguishable from a
schema full of columns something *stopped* writing. Adding each column alongside
the code that fills it keeps the migration history a truthful record of how the
system grew, and means every column in the database at any commit has a writer.

**Cost.** More migration files. That is what migrations are for.
