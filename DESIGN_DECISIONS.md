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

**Decision.** Default Hamming distance 10 of 64, configurable via
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

With photo-realistic fixtures the corpus gives near-duplicates at worst 8 and
unrelated at mean 30.2 with zero false positives, so 10 sits comfortably in the
gap.

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
