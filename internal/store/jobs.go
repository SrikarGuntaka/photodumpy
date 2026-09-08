package store

import (
	"context"
	"fmt"
	"time"

	"github.com/srikarguntaka/photo-organizer/internal/jobs"
)

const jobColumns = `
	id, job_type, target_type, target_id, library_id,
	status, priority, attempt_count, max_attempts,
	leased_by, lease_expires_at, next_attempt_at,
	last_error, created_at, started_at, completed_at`

func scanJob(row interface{ Scan(...any) error }) (jobs.Job, error) {
	var j jobs.Job
	err := row.Scan(
		&j.ID, &j.Type, &j.TargetType, &j.TargetID, &j.LibraryID,
		&j.Status, &j.Priority, &j.AttemptCount, &j.MaxAttempts,
		&j.LeasedBy, &j.LeaseExpiresAt, &j.NextAttemptAt,
		&j.LastError, &j.CreatedAt, &j.StartedAt, &j.CompletedAt,
	)
	return j, err
}

// EnqueueJobs inserts jobs, skipping any that already have an outstanding
// duplicate. Returns how many were actually new.
//
// Idempotent via the partial unique index on dedupe_key WHERE status IN
// ('pending','running'). Scanning a folder twice, or retrying an API call,
// therefore cannot double the queue. A job that has already SUCCEEDED can be
// enqueued again -- that is a deliberate rescan, not a duplicate.
//
// Batched through unnest for the same reason photo inserts are: fanning out
// 10,000 per-photo jobs one INSERT at a time is 10,000 round trips.
func (s *Store) EnqueueJobs(ctx context.Context, batch []jobs.Enqueue) (int, error) {
	if len(batch) == 0 {
		return 0, nil
	}

	types := make([]string, len(batch))
	targetTypes := make([]string, len(batch))
	targetIDs := make([]string, len(batch))
	libraryIDs := make([]string, len(batch))
	priorities := make([]int32, len(batch))
	maxAttempts := make([]int32, len(batch))
	dedupeKeys := make([]string, len(batch))

	for i, e := range batch {
		types[i] = string(e.Type)
		targetTypes[i] = string(e.TargetType)
		targetIDs[i] = e.TargetID
		libraryIDs[i] = e.LibraryID
		priorities[i] = int32(e.Priority)
		if e.MaxAttempts <= 0 {
			e.MaxAttempts = jobs.DefaultMaxAttempts
		}
		maxAttempts[i] = int32(e.MaxAttempts)
		dedupeKeys[i] = jobs.DedupeKey(e.Type, e.TargetID)
	}

	const q = `
		INSERT INTO jobs (
			job_type, target_type, target_id, library_id,
			priority, max_attempts, dedupe_key
		)
		SELECT t, tt, tid::uuid, lid::uuid, pri, maxa, dk
		FROM unnest($1::text[], $2::text[], $3::text[], $4::text[],
		            $5::int[], $6::int[], $7::text[])
			AS x(t, tt, tid, lid, pri, maxa, dk)
		ON CONFLICT DO NOTHING`

	tag, err := s.pool.Exec(ctx, q,
		types, targetTypes, targetIDs, libraryIDs, priorities, maxAttempts, dedupeKeys)
	if err != nil {
		return 0, fmt.Errorf("store: enqueuing jobs: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// ClaimJobs atomically leases up to `limit` runnable jobs to a worker.
//
// THE core query of the whole system. Three things make it correct under
// concurrency:
//
//  1. FOR UPDATE SKIP LOCKED. Concurrent workers running this simultaneously
//     skip over rows their peers have locked rather than blocking on them.
//     Without SKIP LOCKED every worker queues behind the same first row and
//     throughput is flat no matter how many workers you add -- that is the
//     difference between a queue that scales and one that does not.
//
//  2. The subquery selects, the outer statement updates. Doing it in one
//     statement means claiming is a single atomic operation: there is no
//     window between "I found a job" and "I marked it mine" in which another
//     worker could take it.
//
//  3. attempt_count is incremented AT CLAIM TIME, not on failure. If it were
//     incremented on failure, a worker that crashes without reporting anything
//     would never consume an attempt, and a job that reliably kills its worker
//     would be retried forever. Counting at claim time means every attempt is
//     paid for, including the ones that end in a crash.
//
// Returns the claimed jobs, already marked running with a lease.
func (s *Store) ClaimJobs(ctx context.Context, workerID string, limit int, lease time.Duration) ([]jobs.Job, error) {
	if limit <= 0 {
		return nil, nil
	}

	const q = `
		UPDATE jobs SET
			status           = 'running',
			leased_by        = $1,
			lease_expires_at = now() + $2::interval,
			attempt_count    = attempt_count + 1,
			started_at       = COALESCE(started_at, now())
		WHERE id IN (
			SELECT id FROM jobs
			WHERE status = 'pending'
			  AND next_attempt_at <= now()
			ORDER BY priority, id
			LIMIT $3
			FOR UPDATE SKIP LOCKED
		)
		RETURNING ` + jobColumns

	rows, err := s.pool.Query(ctx, q, workerID, lease, limit)
	if err != nil {
		return nil, fmt.Errorf("store: claiming jobs: %w", err)
	}
	defer rows.Close()

	out := []jobs.Job{}
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scanning claimed job: %w", err)
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// RecordExecutionStart appends an audit row for an attempt and returns its id.
//
// The audit table is what makes "no job ran on two workers at once" a
// checkable claim rather than an assertion -- the jobs table only holds the
// latest attempt.
func (s *Store) RecordExecutionStart(ctx context.Context, jobID int64, workerID string, attempt int) (int64, error) {
	const q = `
		INSERT INTO job_executions (job_id, worker_id, attempt)
		VALUES ($1, $2, $3)
		RETURNING id`

	var id int64
	if err := s.pool.QueryRow(ctx, q, jobID, workerID, attempt).Scan(&id); err != nil {
		return 0, fmt.Errorf("store: recording execution start: %w", err)
	}
	return id, nil
}

// RecordExecutionEnd closes out an audit row.
func (s *Store) RecordExecutionEnd(ctx context.Context, execID int64, outcome jobs.Outcome, errMsg *string) error {
	const q = `
		UPDATE job_executions
		SET finished_at = now(), outcome = $2, error = $3
		WHERE id = $1`

	if _, err := s.pool.Exec(ctx, q, execID, string(outcome), errMsg); err != nil {
		return fmt.Errorf("store: recording execution end: %w", err)
	}
	return nil
}

// CompleteJob marks a job succeeded and releases its lease.
//
// The WHERE clause includes leased_by: a worker whose lease has already been
// reaped and reassigned must NOT be able to mark the job done, because another
// worker is now running it and will report its own result. Returns whether the
// update actually applied, so the caller can notice it lost the race.
func (s *Store) CompleteJob(ctx context.Context, jobID int64, workerID string) (bool, error) {
	const q = `
		UPDATE jobs SET
			status           = 'succeeded',
			leased_by        = NULL,
			lease_expires_at = NULL,
			completed_at     = now(),
			last_error       = NULL
		WHERE id = $1 AND leased_by = $2 AND status = 'running'`

	tag, err := s.pool.Exec(ctx, q, jobID, workerID)
	if err != nil {
		return false, fmt.Errorf("store: completing job: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// FailJob records a failure and either schedules a retry or kills the job.
//
// permanent forces death regardless of attempts remaining -- for failures that
// cannot succeed on a retry, like a file that is not a decodable image.
//
// As with CompleteJob, the lease ownership check prevents a worker that has
// already been reaped from stomping on the row.
func (s *Store) FailJob(ctx context.Context, jobID int64, workerID string, errMsg string, backoff time.Duration, permanent bool) (dead bool, applied bool, err error) {
	// A single statement decides retry-vs-death, so there is no read-then-write
	// window in which the attempt count could change underneath us.
	const q = `
		UPDATE jobs SET
			status = CASE
				WHEN $5 OR attempt_count >= max_attempts THEN 'dead'::job_status
				ELSE 'pending'::job_status
			END,
			leased_by        = NULL,
			lease_expires_at = NULL,
			next_attempt_at  = CASE
				WHEN $5 OR attempt_count >= max_attempts THEN next_attempt_at
				ELSE now() + $4::interval
			END,
			completed_at = CASE
				WHEN $5 OR attempt_count >= max_attempts THEN now()
				ELSE NULL
			END,
			last_error = $3
		WHERE id = $1 AND leased_by = $2 AND status = 'running'
		RETURNING status = 'dead'`

	var isDead bool
	row := s.pool.QueryRow(ctx, q, jobID, workerID, errMsg, backoff, permanent)
	if err := row.Scan(&isDead); err != nil {
		if err.Error() == "no rows in result set" {
			// Lost the lease to the reaper; another worker owns this now.
			return false, false, nil
		}
		return false, false, fmt.Errorf("store: failing job: %w", err)
	}
	return isDead, true, nil
}

// RenewLeases extends the lease on every job a worker currently holds.
//
// Returns how many were extended. A count lower than expected means the reaper
// took some jobs away, which the worker uses as its signal to abandon that
// work rather than keep computing a result nobody will accept.
func (s *Store) RenewLeases(ctx context.Context, workerID string, lease time.Duration) (int, error) {
	const q = `
		UPDATE jobs
		SET lease_expires_at = now() + $2::interval
		WHERE leased_by = $1 AND status = 'running'`

	tag, err := s.pool.Exec(ctx, q, workerID, lease)
	if err != nil {
		return 0, fmt.Errorf("store: renewing leases: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// ReleaseWorkerLeases returns a worker's in-flight jobs to the queue
// immediately, without waiting for their leases to expire.
//
// Called on graceful shutdown. This is the difference between
// `docker compose stop worker` reassigning work in milliseconds and it sitting
// idle for the full 60-second lease. attempt_count is NOT decremented: the
// attempt was genuinely spent, and pretending otherwise would let a job that
// repeatedly kills its worker cycle forever.
func (s *Store) ReleaseWorkerLeases(ctx context.Context, workerID string) (int, error) {
	const q = `
		UPDATE jobs SET
			status           = 'pending',
			leased_by        = NULL,
			lease_expires_at = NULL,
			next_attempt_at  = now()
		WHERE leased_by = $1 AND status = 'running'`

	tag, err := s.pool.Exec(ctx, q, workerID)
	if err != nil {
		return 0, fmt.Errorf("store: releasing worker leases: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// ReapExpiredLeases returns jobs whose lease has lapsed to the queue.
//
// This is the crash-recovery mechanism. A worker that segfaults, is SIGKILLed,
// or loses its database connection stops renewing; once the lease passes, this
// makes the job claimable again. Any worker can run the reaper -- it is not a
// singleton role -- because the UPDATE is atomic and reaping an already-reaped
// job is a no-op.
//
// Jobs that have exhausted their attempts go straight to dead rather than
// looping forever.
func (s *Store) ReapExpiredLeases(ctx context.Context, backoff time.Duration) (requeued int, died int, err error) {
	const q = `
		WITH expired AS (
			SELECT id FROM jobs
			WHERE status = 'running'
			  AND lease_expires_at < now()
			FOR UPDATE SKIP LOCKED
		),
		reaped AS (
			UPDATE jobs j SET
				status = CASE
					WHEN j.attempt_count >= j.max_attempts THEN 'dead'::job_status
					ELSE 'pending'::job_status
				END,
				leased_by        = NULL,
				lease_expires_at = NULL,
				next_attempt_at  = CASE
					WHEN j.attempt_count >= j.max_attempts THEN j.next_attempt_at
					ELSE now() + $1::interval
				END,
				completed_at = CASE
					WHEN j.attempt_count >= j.max_attempts THEN now()
					ELSE NULL
				END,
				last_error = COALESCE(
					'lease expired: worker stopped responding (attempt '
						|| j.attempt_count || ' of ' || j.max_attempts || ')',
					j.last_error)
			FROM expired e
			WHERE j.id = e.id
			RETURNING j.id, j.status
		),
		audited AS (
			-- Close out the abandoned execution rows so the audit trail shows
			-- why an attempt has no outcome.
			UPDATE job_executions x
			SET finished_at = now(),
			    outcome     = 'abandoned',
			    error       = 'lease expired'
			FROM reaped r
			WHERE x.job_id = r.id AND x.finished_at IS NULL
			RETURNING 1
		)
		SELECT
			count(*) FILTER (WHERE status = 'pending'),
			count(*) FILTER (WHERE status = 'dead')
		FROM reaped`

	if err := s.pool.QueryRow(ctx, q, backoff).Scan(&requeued, &died); err != nil {
		return 0, 0, fmt.Errorf("store: reaping expired leases: %w", err)
	}
	return requeued, died, nil
}

// GetJob fetches one job. Mostly for tests and debugging.
func (s *Store) GetJob(ctx context.Context, id int64) (*jobs.Job, error) {
	const q = `SELECT ` + jobColumns + ` FROM jobs WHERE id = $1`

	j, err := scanJob(s.pool.QueryRow(ctx, q, id))
	if err != nil {
		return nil, normaliseErr(err)
	}
	return &j, nil
}

// JobCounts is a breakdown of queue state.
type JobCounts struct {
	Pending   int `json:"pending"`
	Running   int `json:"running"`
	Succeeded int `json:"succeeded"`
	Dead      int `json:"dead"`
	Total     int `json:"total"`
}

// CountJobsByType returns per-type queue state for a library, which is what the
// progress display is built from.
func (s *Store) CountJobsByType(ctx context.Context, libraryID string) (map[string]JobCounts, error) {
	const q = `
		SELECT job_type,
		       count(*) FILTER (WHERE status = 'pending')   AS pending,
		       count(*) FILTER (WHERE status = 'running')   AS running,
		       count(*) FILTER (WHERE status = 'succeeded') AS succeeded,
		       count(*) FILTER (WHERE status = 'dead')      AS dead,
		       count(*)                                     AS total
		FROM jobs
		WHERE library_id = $1
		GROUP BY job_type`

	rows, err := s.pool.Query(ctx, q, libraryID)
	if err != nil {
		return nil, fmt.Errorf("store: counting jobs: %w", err)
	}
	defer rows.Close()

	out := map[string]JobCounts{}
	for rows.Next() {
		var t string
		var c JobCounts
		if err := rows.Scan(&t, &c.Pending, &c.Running, &c.Succeeded, &c.Dead, &c.Total); err != nil {
			return nil, fmt.Errorf("store: scanning job counts: %w", err)
		}
		out[t] = c
	}
	return out, rows.Err()
}

// CountAllJobs returns overall queue state for a library.
func (s *Store) CountAllJobs(ctx context.Context, libraryID string) (JobCounts, error) {
	const q = `
		SELECT count(*) FILTER (WHERE status = 'pending'),
		       count(*) FILTER (WHERE status = 'running'),
		       count(*) FILTER (WHERE status = 'succeeded'),
		       count(*) FILTER (WHERE status = 'dead'),
		       count(*)
		FROM jobs WHERE library_id = $1`

	var c JobCounts
	err := s.pool.QueryRow(ctx, q, libraryID).Scan(
		&c.Pending, &c.Running, &c.Succeeded, &c.Dead, &c.Total)
	if err != nil {
		return c, fmt.Errorf("store: counting jobs: %w", err)
	}
	return c, nil
}

// ListPhotoIDsNeedingMetadata returns ids of photos with no metadata yet,
// after the given cursor. Pass "" for the first page.
func (s *Store) ListPhotoIDsNeedingMetadata(ctx context.Context, libraryID, afterID string, limit int) ([]string, error) {
	return s.listPhotoIDs(ctx, `
		SELECT id FROM photos
		WHERE library_id = $1
		  AND metadata_extracted_at IS NULL
		  AND ($3 = '' OR id > $3::uuid)
		ORDER BY id LIMIT $2`, libraryID, afterID, limit)
}

// ListPhotoIDsNeedingHash returns ids of photos with no hash yet, after the
// given cursor.
func (s *Store) ListPhotoIDsNeedingHash(ctx context.Context, libraryID, afterID string, limit int) ([]string, error) {
	return s.listPhotoIDs(ctx, `
		SELECT id FROM photos
		WHERE library_id = $1
		  AND hashed_at IS NULL
		  AND state <> 'missing'
		  AND ($3 = '' OR id > $3::uuid)
		ORDER BY id LIMIT $2`, libraryID, afterID, limit)
}

func (s *Store) listPhotoIDs(ctx context.Context, q, libraryID, afterID string, limit int) ([]string, error) {
	if limit <= 0 || limit > 10000 {
		limit = 1000
	}
	rows, err := s.pool.Query(ctx, q, libraryID, limit, afterID)
	if err != nil {
		return nil, fmt.Errorf("store: listing photo ids: %w", err)
	}
	defer rows.Close()

	out := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("store: scanning photo id: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// DeferJob reschedules a job without consuming an attempt.
//
// The claim incremented attempt_count optimistically, on the assumption the
// job would be tried. A deferral means it was not tried, so that increment is
// undone -- otherwise an aggregate waiting for a long hashing pass would burn
// through its five attempts and die without ever having done anything.
//
// GREATEST(0, ...) guards the floor: an attempt count must never go negative
// even if this somehow ran twice for one claim.
//
// As with CompleteJob and FailJob, the lease-ownership check stops a worker
// that has already been reaped from rescheduling a job someone else now owns.
func (s *Store) DeferJob(ctx context.Context, jobID int64, workerID string, after time.Duration, reason string) (bool, error) {
	const q = `
		UPDATE jobs SET
			status           = 'pending',
			leased_by        = NULL,
			lease_expires_at = NULL,
			next_attempt_at  = now() + $3::interval,
			attempt_count    = GREATEST(0, attempt_count - 1),
			last_error       = $4
		WHERE id = $1 AND leased_by = $2 AND status = 'running'`

	tag, err := s.pool.Exec(ctx, q, jobID, workerID, after, reason)
	if err != nil {
		return false, fmt.Errorf("store: deferring job: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// CountOutstandingJobsOfType reports how many jobs of a type are still pending
// or running for a library.
//
// Used by aggregate handlers to check their prerequisites before doing work
// that would be wrong if the per-photo pipeline has not finished.
func (s *Store) CountOutstandingJobsOfType(ctx context.Context, libraryID string, jobType jobs.Type) (int, error) {
	const q = `
		SELECT count(*) FROM jobs
		WHERE library_id = $1
		  AND job_type = $2
		  AND status IN ('pending', 'running')`

	var n int
	if err := s.pool.QueryRow(ctx, q, libraryID, string(jobType)).Scan(&n); err != nil {
		return 0, fmt.Errorf("store: counting outstanding jobs: %w", err)
	}
	return n, nil
}
