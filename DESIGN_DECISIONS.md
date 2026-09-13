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
exposed in the API, with a configurable threshold (default 12 — see §16 for how
it was measured).

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

**MEASURED, and the numbers settle it.** `BenchmarkGroupSimilar`:

| photos | time | comparisons |
|--------|------|-------------|
| 100 | 41 µs | 5K |
| 1,000 | 798 µs | 500K |
| 5,000 | 20.4 ms | 12.5M |

Clean quadratic scaling (5× the photos, 25× the time), which extrapolates to
roughly **80 ms at 10,000 photos** and ~8 s at 100,000.

So the optimisation would save 79 milliseconds on a realistic library. It stays
unbuilt — not on a hunch now, but on a measurement. The benchmark is committed
so the crossover can be re-checked if libraries get much larger.

This is also a correction to an earlier estimate in this file's history: I
guessed the win might be "3s to 0.2s". The real figure is far more lopsided,
which is the argument for measuring rather than estimating.

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

**Decision forced by that limitation: EXIF wall-clock is interpreted as UTC.**
This surfaced concretely while building the fixture corpus. `goexif` parses
`DateTimeOriginal` and, finding no zone, attaches `time.Local` — so identical
bytes decode to `09:47 -0500` on a developer machine in US/Central and
`09:47 +0000` in a UTC CI container. Storing the parser's guess would mean the
same photo scanned on two machines gets two different `captured_at` values, and
clustering would silently disagree with itself depending on where it ran.

So the extractor must discard the guessed zone and interpret the wall clock as
UTC, uniformly. This is *wrong* in the sense that it does not recover the real
local time — but it is consistent, machine-independent, and preserves ordering
within a library, which is what clustering actually needs. `captured_at_source`
records that the value came from EXIF so the imprecision stays visible rather
than being laundered into apparent certainty.

The fixture test asserts on wall-clock fields rather than instants for exactly
this reason, with the rationale written at the assertion.

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


---

## 15. Similarity grouping uses connected components, not cliques

**Decision.** Photos within the Hamming threshold are joined with union-find,
and each connected component becomes a group.

**The problem this creates, stated up front.** Similarity within a threshold is
**not transitive**. A and B may be 9 apart, B and C 9 apart, and A and C 18
apart — beyond the threshold — yet all three land in one group.

**Why not cliques.** Requiring every pair in a group to be within the threshold
is exact, and has two problems. Finding maximal cliques is NP-hard. And it
splits genuine burst sequences: in a 20-shot burst each frame resembles its
neighbours while the first and last frame differ substantially, so a clique
approach yields fourteen overlapping groups where the user wanted one.

**How the risk is mitigated rather than hidden:**

- The threshold is deliberately tight — 10 bits of 64. On the fixture corpus
  unrelated photos average 30 bits apart, so a chain needs several improbable
  intermediate hops.
- `max_distance` is stored per group and compared against the threshold. A
  group wider than its own threshold is flagged `chained` in the API and marked
  in the CLI, so a drifted group is visible rather than presented with the same
  confidence as a tight one.
- Every member's distance from the suggested keeper is exposed, so a user can
  see *why* a photo is in a group instead of being asked to trust it.

**Cost.** Some groups will be wider than a strict reading of the threshold
implies. That is the price of not fragmenting bursts, and it is surfaced rather
than swallowed.

---

## 16. The similarity threshold was calibrated, not chosen

**Decision.** Default Hamming distance 12 of 64, configurable via
`SIMILARITY_THRESHOLD`, hard-capped at 24.

**How it was arrived at.** The first fixture corpus made dHash look broken:
near-duplicates measured 9–20 apart while unrelated photos averaged 25 — the
distributions overlapped and no threshold could separate them. Rather than
widen the threshold until tests passed, both image styles were measured:

| image style | near-duplicate | unrelated | separation |
|-------------|----------------|-----------|------------|
| hard edges + fine stripes | mean 18.0, max 30 | mean 20.6, min 8 | **−22 (overlapping)** |
| smooth low-frequency | mean 1.3, max 4 | mean 32.2, min 13 | **+9 (clean)** |

The fixtures were the problem. A perceptual hash samples a 9×8 grid, and the
corpus drew a 6px diagonal stripe every 64px — a periodic pattern that aliases
catastrophically at that scale. Photographs have no such structure.

With photo-realistic fixtures, `TestCalibrateSimilarityGap` measures
near-duplicate pairs at 0–9 bits and unrelated pairs at 17–38, a usable gap of
9–17. The default is 12: three bits above the widest genuine near-duplicate and
five below the closest unrelated pair. An earlier default of 10 also passed, but
with a single bit of headroom — it worked by luck rather than by margin. It sits
below the gap's midpoint on purpose: a missed near-duplicate costs some disk
space, while a false positive groups two unrelated photos and invites deleting
one that was never a duplicate.

**The honest limitation.** This is calibrated against *synthetic* images. Real
photographs — especially flat scenes, night shots and heavily edited frames —
may sit differently, and the threshold should be re-checked against a real
library before anyone relies on the default. That is what
`SIMILARITY_THRESHOLD` is for, and why the API exposes raw distances rather
than only a yes/no.

**Cap at 24.** Random unrelated hashes differ by ~32 bits, so a threshold near
that groups everything with everything. 24 is where the feature stops being
meaningful.

---

## 17. Sharpness is measured at a fixed analysis size, on purpose

Two ranking bugs in Phase 7 came from the same root cause, and both were first
"fixed" in the wrong place before the measurement itself was corrected.

### Thumbnails outranking their own originals

The near-duplicate ranker recommended keeping `scene18-small.jpg` (320×240)
over `scene18.jpg` (640×480), because the thumbnail scored sharpness 1.00
against the original's 0.42.

Laplacian variance measures detail **density** — response per pixel. Downscaling
does not throw detail away so much as concentrate it. Measuring one image at
several sizes, with the original rule that left images under 512px at their
native resolution:

Measured before the pre-smooth described below existed, so the absolute values
are higher than the code produces today; the ratio between the columns is the
point.

| size | measured natively | scaled to 512 |
|------|-------------------|---------------|
| 1600×1200 | 71.5 | 71.5 |
| 640×480 | 72.2 | 72.2 |
| 320×240 | **405.1** | 67.9 |
| 160×120 | **2686.0** | 39.5 |

A 160×120 thumbnail scored 37× its own source.

The first attempt added a `resolutionParity` guard to the ranker: refuse to let
quality decide between images more than 2× apart in pixel count. It broke two
legitimate tests — a blurred 12MP frame should lose to a sharp 3MP one — and
that was the signal. The guard was papering over an unreliable measurement
rather than fixing it. **Every** image is now scaled to a 512px long edge,
including images already smaller, and the sequence is monotonic. The guard and
its tests were deleted.

### Recompressed copies outranking their originals

With that fixed, the ranker still preferred `scene02-recompressed.jpg`
(24.7 KB) over `scene02.jpg` (62.9 KB) — same scene, same dimensions, one a
strictly degraded re-encode of the other.

A Laplacian responds to any sharp intensity change, and lossy JPEG
quantisation manufactures plenty that are not in the photograph: 8×8 block
edges and ringing around contours. The operator cannot distinguish artifact
energy from subject detail, so the damaged file measures as *sharper*.

The fix is a separable 1-2-1 binomial pre-smooth (σ ≈ 0.85) before the
Laplacian runs. Blocking artifacts live at the very top of the frequency range;
real edges span several pixels and mostly survive.

| pair | no pre-smooth | with pre-smooth |
|------|---------------|-----------------|
| scene02 original / recompressed | 57.70 / 75.49 (**+31%**) | 35.90 / 38.22 (+6.5%) |
| scene10 original / recompressed | 104.56 / 124.78 (**+19%**) | 68.68 / 70.67 (+2.9%) |
| scene18 original / recompressed | 88.31 / 107.07 (**+21%**) | 57.70 / 59.42 (+3.0%) |

That residual is worth about 0.005 on the overall score — inside the ranker's
0.02 quality tolerance — so file size correctly breaks the tie and the original
is kept in all three groups.

Note what was *not* done: the tolerance was not widened to 0.05. Before the
pre-smooth the three artifact-induced gaps were 0.019, 0.020 and 0.022, so two
groups ranked correctly and one did not, purely by where they fell relative to
an arbitrary constant. Tuning the constant would have hidden the defect in
exactly the way the `resolutionParity` guard did.

The pre-smooth also improves the thing the metric is named for. On the five
fixtures measured both ways, the worst-case ratio between the least-blurred
blurred frame and the least-sharp sharp one went from 24× (66.79 / 2.79) to
57× (38.69 / 0.68): the smoothing removes sensor-level noise that was propping
up the blurred frames' floor.
The blur threshold moved from 0.15 to 0.12 to sit near the centre of the
widened gap (blurred 0.020–0.047, everything else 0.200–0.591 across 39
analysable fixtures, zero misclassifications in either direction).

`sharpnessSaturation` dropped from 500 to 310 at the same time: the pre-smooth
costs in-focus images roughly 38% of their variance, and leaving the constant
alone would have shifted every score down without changing any ordering, while
making values stored before and after the change incomparable.

**The honest limitation**, as with the similarity threshold: this is calibrated
against synthetic fixtures. Real photographs, especially soft-focus portraits
and night shots, will sit differently. That is why the thresholds are
configuration rather than constants, and why `raw_laplacian_variance` is stored
alongside the normalised score — a future recalibration is then a SQL update
rather than a re-decode of the whole library.

---

## 18. Events are segmented in one ordered pass, not clustered in a metric space

The obvious move for "group photos by time and place" is to reach for a
general-purpose clusterer — k-means, or DBSCAN over a combined time/space
metric. Both were rejected for the same reason: they need a single distance
between two photos, and computing one requires an exchange rate between *an
hour* and *a kilometre*.

There is no honest such number. Whatever constant is chosen is a fabricated
parameter with no units and no way to calibrate it, and it silently decides
every boundary in the library. So the two dimensions stay separate, each with
its own threshold in its own units, and the algorithm is a single ordered pass
that cuts between consecutive photos when either test fails. It is also
O(n log n) dominated by the sort, against DBSCAN's O(n²) without a spatial
index.

k-means fails on a second count anyway: it needs *k* up front. Nobody knows how
many events are in a folder — that is the question being asked.

### The two tests are asymmetric, deliberately

**Time is measured between consecutive photos.** A four-hour gap is what a
break in shooting looks like. Measuring from the cluster's start instead would
guillotine a wedding photographed steadily for nine hours at the four-hour
mark, which is why `TestSteadyShootingIsOneEvent` exists.

**Distance is measured from the cluster's anchor** — its first GPS fix — not
from the previous photo. Consecutive-pair comparison permits unbounded drift: a
photo every 200m along a coast road never exceeds a 1500m threshold, so fifty
kilometres of coastline becomes one "place". Anchoring bounds a cluster's
radius by construction.

The cost is real and worth naming: membership depends on which photo came
first, and a genuine walking tour is cut into segments. That is the better
failure. Several clusters a person can merge by eye beats one that silently
spans a county.

### Missing GPS is the common case, not an error path

Most photos have no GPS. A photo without coordinates joins the cluster its
timestamp puts it in and simply does not participate in the distance test — it
must never split an event and must never be excluded from one. A cluster that
starts with unlocated photos adopts the first fix it sees as its anchor, and
keeps the members that joined before it; they joined on time, which is still
true.

Per-member distance is stored NULL when the photo has no fix, never 0. "No fix"
and "at the anchor" are different facts, and conflating them would put every
unlocated photo at the centre of the map.

### Haversine, and why not the alternatives

| formula | rejected because |
|---------|------------------|
| equirectangular | faster, degrades badly near the poles |
| spherical law of cosines | algebraically equivalent, loses precision catastrophically for *nearby* points — the case that dominates here |
| Vincenty | more accurate on the ellipsoid, but iterative and famously fails to converge for near-antipodal points |

Haversine is exact for the spherical model at every separation and stable at
short range. It is written as `atan2(sqrt(a), sqrt(1-a))` rather than
`asin(sqrt(a))`: the two agree in exact arithmetic, but rounding can push `a` a
hair above 1 for antipodal points, and `asin(>1)` is NaN while `atan2` stays
well defined. Longitude wraparound needs no special handling, because
`sin((l2-l1)/2)` is periodic — a pair straddling the antimeridian at +179.9 and
−179.9 gives the correct 22km rather than 40,000km.

A single Earth radius is an approximation — the spheroid means up to ~0.5%
error against WGS-84. At the scale that decides whether two photos are within a
kilometre, that is metres, far inside consumer GPS error.

There is exactly ONE implementation of it. An early draft computed member
distances a second time in SQL, with a comment claiming this avoided drift; it
does the opposite. The SQL version would also have had to be the `asin` form,
losing precision at exactly the short ranges this feature works at.

### Clusters built from mtimes are labelled, not hidden

A photo with no EXIF date falls back to filesystem mtime. A cluster built
entirely from mtimes is a statement about when *files were written*, not when
photographs were taken — and copying a folder stamps every file within a second
or two, which collapses a whole library into one enormous "event".

Rather than suppress those, each cluster counts how many members were dated by
a camera and reports `confidence` as high, mixed or low. On the fixture corpus
this correctly labels the seven no-EXIF files, all stamped within one second of
each other at generation time, as a single low-confidence event.

### Rebuild whole, not incrementally

A single new photo can legitimately MERGE two existing events — one taken in
the gap between them joins both into one. An incremental path would have to
handle merges, splits and re-anchoring. Full rebuild is a few milliseconds at
this scale and is idempotent by construction: re-running produces a
byte-identical fingerprint, which is asserted end to end.

### A bug worth recording

`RebuildClusters` stores the thresholds each cluster was built with, so the
grouping stays interpretable later. The first version stored the *requested*
options rather than the *effective* ones — defaults were applied inside
`Segment`, so a caller passing a zero value had "built with a maximum gap of
zero seconds" written to every row.

It was caught by the `max_gap_seconds > 0` CHECK constraint, which is the right
place for it to be caught and the wrong place to be relying on. The fix
exports `Options.WithDefaults()` and resolves once, at construction, so the
values logged and the values stored are the values applied.

---

## 19. The read API's sort key is an allow-list, not a parameter

Every user-supplied value in this codebase is a bind parameter. Sorting is the
one place that is impossible: `ORDER BY` cannot take a placeholder, so a sort
key has to reach the query as *text*. That makes it the single most likely
injection point in a read API, and it is worth being explicit about how it is
closed.

`sortColumns` is a map from the API's own sort names to fixed SQL expressions.
A key that is not in the map never reaches the query — the caller's string is
used only as a map lookup, and an unrecognised one is a 400. Even
`sort=relative_path`, the real column name, is rejected: the accepted vocabulary
is the API's, not the schema's, so the column names are not an input surface and
renaming one is not a breaking change.

Validation happens in two places on purpose. The store's map is the *boundary*
— it is what makes injection impossible. The HTTP layer's `ValidSort` check is
about *honesty*: without it a typo would fall through to the default ordering
and return a plausible page with no indication the sort was ignored.

### Silent no-ops are worse than errors

The same reasoning drives every other filter. A misspelled flag, an
unparseable date, `to` before `from`, `order=sideways` — all 400. The
alternative is a caller who sees a believable result set and cannot tell it was
unfiltered. An error is recoverable; a wrong answer that looks right is not.

The flag vocabulary itself is derived from `quality.AllFlags` rather than
retyped in the API. The first version *was* retyped, and was wrong on two of
seven entries within minutes — `shadow_clipping` against the real
`shadows_clipped` — so the API would have rejected a flag name it prints in its
own output. A test now drives `flagsFor` through extreme metrics and asserts the
list covers everything emitted, and that nothing in the list is unreachable.

### LIKE metacharacters are escaped

Path search is `ILIKE '%' || $n || '%'`. The value is bound, so this is not an
injection risk — but without escaping, `_` is a single-character wildcard and
`%` matches everything. Searching for `scene0_` would silently return
`scene01` through `scene09`, and searching for `%` would return the entire
library while claiming to have filtered. The backslash is escaped first,
otherwise it would escape the escapes added after it.

### Every ordering ends with the primary key

Three byte-identical copies of a file compare equal on size, on capture time and
on quality. Rows that tie have no defined order, and Postgres may return them
differently on each execution — so paging through such a result silently
repeats some rows and skips others. Appending `photos.id` to every `ORDER BY`
makes the sort total, which is what makes offset pagination coherent at all.

### NULLS LAST on every nullable sort

Postgres sorts NULLs first under `DESC`. "Worst quality first" would therefore
open with a page of photos that have no quality score — presenting *not
measured* as *measured badly*, the same confusion `MarkQualityFailed` exists to
prevent. Every nullable sort column pins its NULLs to the end.

### One predicate builder feeds the page and the count

`total` is the number of rows matching the filter, and it comes from the same
`predicate()` call that builds the page query. A count that quietly ignores a
filter is what makes a UI offer page 7 of a 5-page result. They cannot drift,
because there is only one of them.

### Memberships are scalar subqueries, not joins

A photo can be in a duplicate group AND a similar group AND a cluster. Joining
three membership tables would multiply that photo into eight rows, and `LIMIT`
would then paginate over the multiplied rows rather than over photos — a page
size of 100 returning 43 distinct photos. Each membership is a single indexed
lookup on `photo_id` in the select list instead.

### PhotoView is a projection, not a widened domain type

`photos.Photo` is what the ingestion passes read and write, and it stays narrow.
`store.PhotoView` joins in quality scores, hashes and group memberships that no
pass needs. Widening the domain type instead would hand every handler fields it
has no business touching, and the hash columns in particular would then need
rendering logic — `phash` is stored as a signed int64, because Postgres has no
unsigned type, and reads as a meaningless negative number until it is hexed.

---

## 20. Thumbnails: the only image data this application writes

### Orientation is read from the file, not from the metadata row

Neither `image/jpeg` nor `x/image` applies EXIF orientation on decode, so a
phone photo held upright decodes lying on its side. Getting this wrong puts
every portrait photo in the grid at 90 degrees — the most visible bug a photo
tool can have.

The photos row already has an `orientation` column, written by
`EXTRACT_METADATA`. Using it would make thumbnail generation depend on that job
having finished, and both ways of expressing that dependency are bad:

- **Defer until metadata is done.** If the metadata job dies, `metadata_extracted_at`
  stays NULL forever and the thumbnail job defers forever. `Defer` does not
  consume attempts, so nothing would ever kill it.
- **Don't wait.** Whenever the thumbnail job wins the race, it renders sideways
  and marks itself done, and nothing revisits it.

Instead `thumbnail.FromReader` re-reads orientation from the file handle it has
already opened. A few KB of EXIF parsing is far cheaper than cross-job
coordination, and the thumbnail becomes a function of the file alone — it can
run in any order, on any worker, with no prerequisite.

The test for this does not trust the pixel arithmetic alone. It builds a JPEG,
splices in a hand-written EXIF APP1 segment carrying orientation 6, and checks
that the real extractor's reading of it turns a 60×20 image into a 20×60 one
with its marker in the right corner. The EXIF bytes are hand-built rather than
produced by a library, so a bug shared between writer and reader cannot cancel
itself out.

### Filenames come from the photo id and nothing else

`PathFor` accepts only a canonical lowercase UUID. A string matching
`^[0-9a-f]{8}-…-[0-9a-f]{12}$` contains no separator, no dot and no drive
letter, so no value that passes can name anything outside the thumbnail
directory. There is no traversal check to get subtly wrong, because there is
nothing to traverse with.

The photo's own filename is never involved. It comes from the user's disk, can
contain anything the filesystem permits, and two photos in different folders
routinely share one.

The API lowercases the id before calling `PathFor`. UUIDs are case-insensitive
on input and Postgres treats them that way, so without it an uppercase id served
the original but 404'd the thumbnail for the same photo. Changing letter case
cannot introduce a separator, and the result is still validated.

### Writes are atomic

Encode to a temp file in the same directory, `fsync`, rename into place. A
worker killed mid-write — the exact crash the queue is built to survive — would
otherwise leave a truncated JPEG that the API serves indefinitely. The temp file
sits beside the target rather than in `/tmp` because rename is only atomic
within one filesystem, and in Docker those are different mounts. The sync
happens before the rename because the directory entry can reach disk before
the data does, and a power loss would leave a correctly-named empty file.

### Rendering order: flatten, scale, orient

- **Flatten before scaling.** JPEG has no alpha; a transparent pixel encodes as
  black. Flattening after scaling is too late — resampling has already blended
  the black premultiplied colour into opaque neighbours, leaving a dark fringe.
  Background is light grey rather than white so it doesn't glare in a dark UI.
- **Orient after scaling.** Rotation is a pixel permutation, so it costs the same
  at any size. Doing it on the 400px result instead of a 12MP decode is hundreds
  of times less work, and long-edge scaling is symmetric so the order does not
  change the output.
- **Never upscale.** A 240px image stays 240px; enlarging it invents nothing.

### The API cannot write thumbnails

Workers generate thumbnails; the API only serves them. The thumbnail volume is
mounted `:ro` on the API container, so the process facing HTTP has no write
access to image data anywhere — verified by attempting a write inside the
container, which the kernel refuses.

### Serving bytes safely

Every media endpoint takes a photo id and never a path. For originals the path
is rebuilt from database rows and containment is re-checked **on every
request**, twice: the library root must still be inside `PHOTO_ROOT` (which is
configuration and may have narrowed), and the file must resolve inside the root
after symlinks are evaluated (a symlink created after the scan could point
anywhere). Non-regular files are refused, since serving a FIFO would block the
handler until the write timeout.

Content-Type comes from an allow-list of raster formats, never from the file's
extension — `ServeContent` is called with an empty name so a photo named
`x.html` cannot be served as HTML. Every response carries `nosniff`, a
`default-src 'none'; sandbox` CSP, and `Cache-Control: private`.

Thumbnails carry a strong ETag built from the photo id and the generation
timestamp, so revisiting a grid costs a 304 per tile and a regenerated thumbnail
is picked up immediately.

---

## 21. Two bugs that terminal output hid and a UI made obvious

Both shipped in earlier phases with passing tests. Both were found within
minutes of rendering real data in the web UI, because a screen puts related
numbers side by side where a CLI prints them one command apart.

### Exact copies were reported as near-duplicates (Phase 6)

The overview showed **Exact duplicates: 324 KB** next to **Near-duplicates:
614 KB across 9 groups**. The fixture generator builds 6 near-duplicate
groups. The other 3 were the exact-duplicate groups again: byte-identical files
have identical perceptual hashes, sit at Hamming distance 0, and so formed their
own "near-duplicate" groups. The review screen then described three identical
files as *"visually similar but not identical"* — false — and a user adding the
two cards would have counted the same 324 KB twice.

No test caught it because no test compared similar groups to the manifest.
`TestNearDuplicatesAreNotReportedAsExact` existed; its mirror did not.

**Fix:** similarity candidates are collapsed to one per distinct file before
grouping. Exact copies are the exact-duplicate pipeline's job; the similarity
pipeline compares distinct images. Near-duplicate reclaimable space fell from
614 KB to 290 KB — the difference is precisely the exact-duplicate total that
had been counted twice.

The representative is not arbitrary. It must be the same copy the
exact-duplicate group suggests keeping, or the Review screen's two tabs would
recommend keeping *different* copies of the same bytes. That ranking lives in
SQL, and the final tiebreak compares paths under the database's collation — so
it was extracted to one shared `exactKeeperOrder` fragment used by both
queries, rather than reimplemented in Go where a string comparison can order
two paths the other way.

`BUILD_SIMILAR_GROUPS` also gained `COMPUTE_FILE_HASH` as a prerequisite.
Collapsing depends on sha256, so grouping before every file was hashed would
leave some copies uncollapsed and make the result depend on job timing.

Two integration tests now pin it: the mirror test asserts the group count
matches the manifest and no group holds two identical copies; a second plants a
file that is both exactly duplicated *and* has a near-duplicate variant — a case
the corpus lacked — and asserts the near-duplicate group contains exactly one
copy, and that it is the exact-duplicate keeper. Disabling the collapse fails
both, the first with *"found 9 near-duplicate groups, corpus was built with 6"*.

### Every photo without GPS carried a warning (Phase 3)

The detail panel showed, in red, *"Last error: gps unreadable: exif: tag
GPSLongitude is not present"* — on a photo that simply has no GPS, which this
project's own documentation calls normal rather than an error.

The extractor intended to suppress exactly that case. It detected "no GPS block"
by matching the error text for `"not found"`. goexif's message says
`"is not present"`. The match never succeeded, so every EXIF-without-GPS photo —
9 of 42 in the corpus — was stored with a spurious warning.

**Fix:** use the library's typed error, `exif.IsTagNotPresentError`, instead of
its wording. One subtlety kept: a GPS block with latitude but no longitude also
yields that error, and that block genuinely is malformed, so latitude is probed
and an incomplete block still warns. (The corpus has no such file, so that
branch is not exercised by a test.)

`TestMissingGPSIsNotAnError` already checked that such a photo did not *fail*.
It never checked that it produced no *warning*, which is where the bug lived. The
assertion was added first and failed on all 9 files before the fix.

The lesson in both cases is the same: a test that checks the thing did not
break is not a test that the thing is right.

---

## 22. The web UI

### Served by the API, from the same origin

The built UI is a directory of static files that the Go API serves on its own
origin. A separate static server would make the browser treat UI and API as
different origins: CORS headers, a preflight on every non-simple request, and a
credentials policy to get right. On one origin there is no cross-origin request
to permit, so none of that exists to misconfigure. In development, Vite proxies
`/api` so the browser still sees one origin — the app cannot tell which setup it
is under.

It is a directory rather than `go:embed` so that `go build` and `go test` never
depend on a Node toolchain having run first. Without a build present, the UI is
simply absent and every API route still works.

### Three kinds of request, and the SPA-fallback bug

A path naming a real file is that file. A path with no extension is a client
route and gets `index.html`. A path **with** an extension that does not exist is
a 404 — never `index.html`. Serving HTML in place of a missing
`/assets/index-OLD.js` is the classic single-page-app bug: the browser reports a
MIME-type error that points nowhere near the cause, a stale cached `index.html`
naming an asset from a previous build. Unmatched `/api/` paths are JSON 404s,
registered on their own prefix so the UI's catch-all can never swallow them.

Hashed assets are cached for a year as `immutable`; `index.html` is `no-cache`,
because it is the one file that names the current build's assets.

### A strict CSP that is not decorative

`default-src 'self'` with no `unsafe-inline` or `unsafe-eval`, plus
`frame-ancestors 'none'`, `base-uri 'none'` and `object-src 'none'`. Vite emits
external scripts and stylesheets, and React applies inline `style` props through
the CSSOM rather than as HTML attributes, which the policy permits — so the app
needs no exemptions. Verified in the running container with zero console
violations. A test fails if `unsafe-inline` or `unsafe-eval` ever appears.

The page loads nothing from anywhere else. No CDN, no web fonts, no analytics:
for a tool that reads someone's entire photo library, no third party learning
that the page was opened is part of "local-first".

### State lives in the URL

Library, view, every filter, and which photo's panel is open are all in the URL.
Every screen is bookmarkable, a refresh restores exactly what was showing, and
the back button does what a user expects. Filter changes *replace* the history
entry — otherwise typing "trip" adds four entries and back steps through "tri",
"tr", "t". Opening a photo *pushes*, so back closes it.

Two bugs were caught here before shipping. Closing the panel called `back()`
whenever history state was null — but a pasted link *also* has null state, and in
a tab with earlier browsing history that `back()` left the app entirely. The fix
marks the entry the panel pushes, and closes by `back()` only on that marker.
The follow-on: a `replace` wrote null state and would wipe the marker, or — when
moving between photos inside a panel opened from a pasted link — stamp a marker
onto an entry the panel never pushed. A replace now preserves existing state
unless told otherwise. Both have tests, and both behaviours were exercised in
the real browser.

### The stale-response race

Type "tri", then "trip". If the "tri" request is slower, its response lands
last and overwrites the correct results — no error, just the wrong photos.
`useApi` aborts the previous request when inputs change, **and** tags each
request with a generation number, committing a result only if nothing newer has
started. Abort alone is not enough: a response that has already arrived can
still resolve. The test for this was mutation-checked — removing the generation
guard makes it fail with the stale result on screen.

### Pagination by offset, not by growing the limit

The first "Load more" grew `limit` by 60 per click. The API clamps any limit
above 500 back to its default of 100, so the ninth click would have silently
*shrunk* a 480-photo grid to 100. Pages are now fetched by offset and appended,
each tagged with the filter it was fetched for, so a page arriving after the
filters changed can never be appended to a different query's results.

### Filters the API does not support are not faked

The overview's "worth a second look" card first linked to `?flagged=1`, a
parameter the search API did not have. The server would have ignored it, and
the page would have claimed to be filtered while showing every photo. Rather
than fake it client-side, the API gained a real `has_flags` filter, tri-state
like the others and matching the GIN index's predicate.

### Honesty in the interface

The same rules as the API, carried through to pixels:

- **Unmeasured is a dash, never zero.** A photo with no quality score shows
  `—`, not `0.00`.
- **Hedged flags keep their hedge.** "Possibly blurry", with a note that flags
  describe pixels, not merit.
- **No delete button**, because there is no delete path. The review screen's
  only action copies the non-keeper paths to the clipboard for the user to act
  on in their own tools.
- **The keeper's reasons are visible.** Near-duplicate members show resolution,
  size and measured quality — the ranking's inputs — rather than a bare "keep".
- **Timestamps render in UTC**, matching how EXIF wall-clock times were read. A
  local-zone conversion would move a 23:30 photo onto the next day, and the date
  is what the timeline groups by.

### Deliberately small dependency surface

React and React DOM at runtime; nothing else. Routing, data fetching and polling
are each a few dozen lines written for this app's actual needs, rather than a
router and a query library whose behaviour would have to be learned, configured
and trusted. That is a trade-off rather than a rule: at a larger scale, a
maintained query cache would earn its place.

### Known gaps

- **Orphaned thumbnails.** Deleting a library leaves its thumbnail files on
  disk. The API refuses to serve them, since it checks the photo row first, but
  nothing removes them.
- **No component tests for the views.** Logic with real failure modes — the
  race, history, encoding, formatting — is unit tested; the views were verified
  by driving the running app, not by automated rendering tests.
