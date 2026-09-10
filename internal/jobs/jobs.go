// Package jobs defines the queue's vocabulary: job types, states, and the
// retry policy. The SQL that implements the queue lives in internal/store; the
// runtime that consumes it lives in internal/worker.
//
// Splitting it this way keeps the backoff arithmetic and the type registry
// unit-testable without a database, which matters because "does backoff grow
// correctly and stay capped" is exactly the kind of thing that is easy to get
// subtly wrong and hard to notice in production.
package jobs

import (
	"fmt"
	"math"
	"math/rand"
	"time"
)

// Type identifies what a job does. Job type is data, not a deployment unit --
// one worker binary handles all of them.
type Type string

const (
	// Per-photo jobs. Embarrassingly parallel; each writes only its own row.
	TypeExtractMetadata       Type = "EXTRACT_METADATA"
	TypeComputeFileHash       Type = "COMPUTE_FILE_HASH"
	TypeComputePerceptualHash Type = "COMPUTE_PERCEPTUAL_HASH"
	TypeAnalyzeQuality        Type = "ANALYZE_QUALITY"

	// Aggregate stages. One per library, needing a global view.
	TypeBuildDuplicateGroups Type = "BUILD_DUPLICATE_GROUPS"
	TypeBuildSimilarGroups   Type = "BUILD_SIMILAR_GROUPS"
	TypeBuildClusters        Type = "BUILD_CLUSTERS"
)

// TargetType distinguishes the two shapes of job.
type TargetType string

const (
	TargetPhoto   TargetType = "photo"
	TargetLibrary TargetType = "library"
)

// Status is a job's position in the state machine.
//
//	                enqueue
//	                   |
//	                   v
//	+------------> pending <---------------+
//	|                 |                    |
//	|   claim (FOR UPDATE SKIP LOCKED)     |
//	|                 v                    |
//	|             running ---------------->+ lease expired, or retryable
//	|            /        \                  failure: attempt++, backoff
//	| success   /          \  attempts exhausted
//	|          v            v
//	+---- succeeded        dead
//
// There is deliberately no 'failed' resting state -- see the migration.
type Status string

const (
	StatusPending   Status = "pending"
	StatusRunning   Status = "running"
	StatusSucceeded Status = "succeeded"
	StatusDead      Status = "dead"
)

// Priority values. Lower runs sooner.
const (
	// PriorityPerPhoto is the default for the fan-out work.
	PriorityPerPhoto = 100
	// PriorityAggregate is higher (later) so the per-photo jobs an aggregate
	// depends on drain first. This is a scheduling hint, not a dependency
	// mechanism -- an aggregate that runs early simply produces a result over
	// whatever has completed, and is re-run when the rest lands.
	PriorityAggregate = 200
)

// DefaultMaxAttempts before a job is declared dead.
//
// Five is a compromise: enough that a transient blip (a brief database
// hiccup, a file locked by a backup process) is ridden out, few enough that a
// genuinely broken job stops burning I/O within a couple of minutes given the
// backoff schedule below.
const DefaultMaxAttempts = 5

// Lease timing.
//
// The ratio is what matters. A 60s lease renewed every 5s gives twelve chances
// to renew before the lease lapses, so a worker has to be genuinely unable to
// reach the database -- not merely slow -- before another worker takes its
// work. Shortening the lease speeds up crash recovery but raises the risk of
// spuriously stealing work from a healthy-but-slow worker.
const (
	DefaultLeaseDuration = 60 * time.Second
	LeaseRenewInterval   = 5 * time.Second
	HeartbeatInterval    = 5 * time.Second

	// A worker is presumed dead after missing this many heartbeats.
	DeadWorkerThreshold = 3 * HeartbeatInterval
)

// Backoff schedule.
const (
	backoffBase = 2 * time.Second
	backoffCap  = 5 * time.Minute
)

// Backoff returns how long to wait before attempt number `attempt` (1-based).
//
// Exponential with a cap, plus jitter. The jitter is not decoration: a
// Postgres restart fails every in-flight job at the same instant, and without
// jitter all of them would retry in the same instant, fail together again, and
// keep marching in lockstep. Spreading retries is what turns a thundering herd
// into a trickle.
//
// The jitter is +/-25% ("full jitter" would be 0..d, which is better for
// contention but makes the schedule hard to reason about and can retry almost
// immediately -- unhelpful when the cause is a database that needs a moment).
func Backoff(attempt int, rng *rand.Rand) time.Duration {
	if attempt < 1 {
		attempt = 1
	}

	// Cap the exponent before shifting: 1<<63 overflows, and an attempt count
	// that high means something is very wrong anyway.
	exp := attempt - 1
	if exp > 30 {
		exp = 30
	}

	d := time.Duration(float64(backoffBase) * math.Pow(2, float64(exp)))
	if d > backoffCap || d <= 0 {
		d = backoffCap
	}

	// +/-25%
	jitter := 1.0 + (rng.Float64()-0.5)/2
	d = time.Duration(float64(d) * jitter)

	if d > backoffCap {
		d = backoffCap
	}
	if d < time.Second {
		d = time.Second
	}
	return d
}

// Job is one unit of queued work.
type Job struct {
	ID         int64      `json:"id"`
	Type       Type       `json:"job_type"`
	TargetType TargetType `json:"target_type"`
	TargetID   string     `json:"target_id"`
	LibraryID  string     `json:"library_id"`

	Status   Status `json:"status"`
	Priority int    `json:"priority"`

	AttemptCount int `json:"attempt_count"`
	MaxAttempts  int `json:"max_attempts"`

	LeasedBy       *string    `json:"leased_by,omitempty"`
	LeaseExpiresAt *time.Time `json:"lease_expires_at,omitempty"`
	NextAttemptAt  time.Time  `json:"next_attempt_at"`

	LastError *string `json:"last_error,omitempty"`

	CreatedAt   time.Time  `json:"created_at"`
	StartedAt   *time.Time `json:"started_at,omitempty"`
	CompletedAt *time.Time `json:"completed_at,omitempty"`
}

// AttemptsRemaining reports how many tries are left before the job dies.
func (j *Job) AttemptsRemaining() int {
	n := j.MaxAttempts - j.AttemptCount
	if n < 0 {
		return 0
	}
	return n
}

// DedupeKey builds the idempotent enqueue key for a job.
//
// One outstanding job per (type, target). Enqueuing the same work twice while
// the first is still pending or running is a no-op, which is what makes
// "scan the folder again" safe.
func DedupeKey(t Type, targetID string) string {
	return fmt.Sprintf("%s:%s", t, targetID)
}

// Enqueue describes a job to be created.
type Enqueue struct {
	Type        Type
	TargetType  TargetType
	TargetID    string
	LibraryID   string
	Priority    int
	MaxAttempts int
}

// NewPhotoJob builds a per-photo enqueue request with the usual defaults.
func NewPhotoJob(t Type, photoID, libraryID string) Enqueue {
	return Enqueue{
		Type:        t,
		TargetType:  TargetPhoto,
		TargetID:    photoID,
		LibraryID:   libraryID,
		Priority:    PriorityPerPhoto,
		MaxAttempts: DefaultMaxAttempts,
	}
}

// NewLibraryJob builds an aggregate enqueue request.
func NewLibraryJob(t Type, libraryID string) Enqueue {
	return Enqueue{
		Type:        t,
		TargetType:  TargetLibrary,
		TargetID:    libraryID,
		LibraryID:   libraryID,
		Priority:    PriorityAggregate,
		MaxAttempts: DefaultMaxAttempts,
	}
}

// Outcome is how an execution ended, recorded in job_executions.
type Outcome string

const (
	OutcomeSucceeded Outcome = "succeeded"
	OutcomeFailed    Outcome = "failed"
	// OutcomeAbandoned is written by the reaper: the execution never reported
	// a result because its worker stopped renewing the lease.
	OutcomeAbandoned Outcome = "abandoned"
)

// PermanentError marks a failure that must not be retried.
//
// The distinction matters more than it looks. A file that is not a decodable
// image will still not be one on the fifth attempt, and retrying it five times
// with exponential backoff wastes minutes of I/O per broken file. Handlers
// return this to say "record the failure and stop", which sends the job
// straight to dead without consuming the remaining attempts.
type PermanentError struct {
	Err error
}

func (e *PermanentError) Error() string { return e.Err.Error() }
func (e *PermanentError) Unwrap() error { return e.Err }

// Permanent wraps err so the queue will not retry it.
func Permanent(err error) error { return &PermanentError{Err: err} }

// OutcomeDeferred records a run that did nothing because its prerequisites
// were not met yet. Distinct from failure (nothing went wrong) and from
// abandonment (the worker did not die).
const OutcomeDeferred Outcome = "deferred"

// DeferError asks the queue to run this job again later without counting the
// run as an attempt.
//
// The motivating case is an aggregate stage whose per-photo prerequisites are
// still in flight. Priority orders the claim, not the execution, so an
// aggregate can be claimed in the same batch as the work it depends on and run
// concurrently with it -- producing a confident answer over incomplete data.
//
// Deferring is deliberately NOT a failure. Nothing was attempted, so consuming
// one of five attempts would mean a large library could exhaust the aggregate's
// retries purely by taking a while to hash, and the groups would never be
// built at all.
type DeferError struct {
	After  time.Duration
	Reason string
}

func (e *DeferError) Error() string {
	return fmt.Sprintf("deferred for %s: %s", e.After, e.Reason)
}

// Defer returns an error asking the queue to retry after the given delay
// without consuming an attempt.
func Defer(after time.Duration, reason string) error {
	if after <= 0 {
		after = 2 * time.Second
	}
	return &DeferError{After: after, Reason: reason}
}
