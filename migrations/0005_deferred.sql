-- 0005_deferred: let a job reschedule itself without consuming an attempt.
--
-- Motivation, from a real failure observed in Phase 5 testing:
--
--   COMPUTE_FILE_HASH       first_start 39.935  last_finish 40.082
--   BUILD_DUPLICATE_GROUPS  first_start 40.049  last_finish 40.082
--
-- The aggregate started 33ms BEFORE the hashing it depends on had finished. It
-- grouped over a partially-hashed library, found 6 duplicate files instead of
-- 9, and recorded that as the answer.
--
-- Priority (100 for per-photo, 200 for aggregates) orders the CLAIM, but a
-- worker claiming a batch of 22 gets both kinds in one batch and runs them
-- concurrently. Ordering the claim does not order the execution.
--
-- Rather than build dependency tracking -- a dependency table, cycle
-- detection, cascade-on-death -- the aggregate now checks its own
-- prerequisites and defers when they are unmet. Re-checking on every run is
-- more robust than a static graph, because it stays correct when work is
-- added, retried or reclaimed while the aggregate is waiting.
--
-- A deferral is not an attempt: nothing was tried and nothing failed. So the
-- claim-time increment is undone, and the audit trail gets its own outcome
-- rather than being mislabelled as abandoned (which means "the worker died").

ALTER TABLE job_executions DROP CONSTRAINT job_executions_outcome_check;

ALTER TABLE job_executions
    ADD CONSTRAINT job_executions_outcome_check
    CHECK (outcome IS NULL OR outcome IN ('succeeded', 'failed', 'abandoned', 'deferred'));
