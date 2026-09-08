// Package worker is the job-processing runtime: it registers a worker
// identity, claims work, keeps its leases alive, executes handlers with
// bounded concurrency, and shuts down without losing anything.
//
// The scheduling lives here; the actual work lives in the handlers, which are
// the same idempotent functions the in-process passes of Phases 3 and 4
// already used.
package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"os"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/srikarguntaka/photo-organizer/internal/jobs"
	"github.com/srikarguntaka/photo-organizer/internal/store"
)

// Handler executes one job. Handlers must be idempotent: under at-least-once
// delivery a job can run more than once, and a re-run must converge to the
// same state rather than compounding.
//
// Returning jobs.Permanent(err) marks the failure as one that retrying cannot
// fix, sending the job straight to dead.
type Handler func(ctx context.Context, job jobs.Job) error

// Options configures a Worker.
type Options struct {
	// Concurrency caps how many jobs run simultaneously. Zero means NumCPU.
	Concurrency int
	// LeaseDuration is how long a claim is valid without renewal.
	LeaseDuration time.Duration
	// PollInterval is how long to wait after finding an empty queue.
	PollInterval time.Duration
	// ShutdownGrace bounds how long to wait for in-flight jobs on SIGTERM.
	ShutdownGrace time.Duration
	// DisableReaper turns off this worker's reaper loop. Only for tests that
	// need to observe expired leases without them being cleaned up.
	DisableReaper bool
}

func (o *Options) withDefaults() {
	if o.Concurrency <= 0 {
		o.Concurrency = 4
	}
	if o.LeaseDuration <= 0 {
		o.LeaseDuration = jobs.DefaultLeaseDuration
	}
	if o.PollInterval <= 0 {
		o.PollInterval = time.Second
	}
	if o.ShutdownGrace <= 0 {
		o.ShutdownGrace = 30 * time.Second
	}
}

// Worker consumes jobs from the queue.
type Worker struct {
	ID    string
	store *store.Store
	log   *slog.Logger
	opts  Options

	handlers map[jobs.Type]Handler

	// inFlight counts jobs currently executing, so the claim loop only ever
	// asks for as many as it can actually run. A worker must never hold a
	// lease on work it is not running -- that is what makes killing a worker
	// cheap, and what keeps the queue's view of "running" honest.
	mu       sync.Mutex
	inFlight int

	// rng for backoff jitter. Guarded by its own mutex because math/rand's
	// non-global sources are not goroutine-safe and several job goroutines can
	// fail at once.
	rngMu sync.Mutex
	rng   *rand.Rand

	// leaseLost is closed when this worker discovers it has lost its leases,
	// signalling in-flight handlers to stop.
	stopClaiming chan struct{}
	stopOnce     sync.Once
}

// New builds a worker with a fresh identity.
//
// The UUID is generated here rather than by the database because a worker
// needs an identity before it can insert its own registration row, and
// generating it client-side makes a retried registration idempotent.
func New(st *store.Store, log *slog.Logger, opts Options) *Worker {
	opts.withDefaults()

	id := uuid.NewString()
	hostname, _ := os.Hostname()

	return &Worker{
		ID:           id,
		store:        st,
		log:          log.With("worker_id", id, "hostname", hostname),
		opts:         opts,
		handlers:     make(map[jobs.Type]Handler),
		rng:          rand.New(rand.NewSource(time.Now().UnixNano() ^ int64(os.Getpid()))),
		stopClaiming: make(chan struct{}),
	}
}

// Register attaches a handler to a job type. A job whose type has no handler
// is failed permanently rather than retried -- retrying will not make the
// handler appear, and leaving it pending would silently stall the queue.
func (w *Worker) Register(t jobs.Type, h Handler) {
	w.handlers[t] = h
}

// Run starts the worker and blocks until ctx is cancelled.
//
// Lifecycle:
//
//	register
//	  |
//	  +-- heartbeat loop    (every 5s)
//	  +-- lease renewal     (every 5s, extends held leases)
//	  +-- reaper loop       (every 15s, reclaims other workers' expired leases)
//	  |
//	  +-- claim loop:
//	        wait for a free slot     <- bounded by Concurrency
//	        claim only as many as there are free slots
//	        dispatch each into the pool
//
//	SIGTERM (ctx cancelled)
//	  +-- stop claiming
//	  +-- wait for in-flight jobs, up to ShutdownGrace
//	  +-- release any leases still held  -> instant reassignment
//	  +-- mark self stopped
func (w *Worker) Run(ctx context.Context) error {
	hostname, _ := os.Hostname()

	if err := w.store.RegisterWorker(ctx, w.ID, hostname, os.Getpid(), w.opts.Concurrency); err != nil {
		return fmt.Errorf("worker: registering: %w", err)
	}
	w.log.Info("worker registered",
		"concurrency", w.opts.Concurrency,
		"lease", w.opts.LeaseDuration,
		"handlers", len(w.handlers))

	var background sync.WaitGroup

	background.Add(1)
	go func() { defer background.Done(); w.heartbeatLoop(ctx) }()

	background.Add(1)
	go func() { defer background.Done(); w.renewLoop(ctx) }()

	if !w.opts.DisableReaper {
		background.Add(1)
		go func() { defer background.Done(); w.reaperLoop(ctx) }()
	}

	// The claim loop runs on this goroutine and returns when ctx is cancelled.
	jobsWG := w.claimLoop(ctx)

	// --- shutdown ---------------------------------------------------------
	w.log.Info("shutting down, waiting for in-flight jobs", "grace", w.opts.ShutdownGrace)

	drained := make(chan struct{})
	go func() { jobsWG.Wait(); close(drained) }()

	select {
	case <-drained:
		w.log.Info("in-flight jobs finished")
	case <-time.After(w.opts.ShutdownGrace):
		w.log.Warn("shutdown grace elapsed with jobs still running; releasing their leases")
	}

	background.Wait()

	// A fresh context: ctx is already cancelled, and these two writes are the
	// difference between work being reassigned in milliseconds and it sitting
	// idle for the full lease duration.
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()

	released, err := w.store.ReleaseWorkerLeases(cleanupCtx, w.ID)
	if err != nil {
		w.log.Error("releasing leases on shutdown", "error", err)
	} else if released > 0 {
		w.log.Info("released leases for immediate reassignment", "jobs", released)
	}

	if err := w.store.UnregisterWorker(cleanupCtx, w.ID); err != nil {
		w.log.Error("unregistering", "error", err)
	}

	w.log.Info("worker stopped cleanly")
	return nil
}

// claimLoop pulls work until the context is cancelled. Returns a WaitGroup
// tracking the jobs it dispatched.
func (w *Worker) claimLoop(ctx context.Context) *sync.WaitGroup {
	var wg sync.WaitGroup

	for {
		select {
		case <-ctx.Done():
			return &wg
		case <-w.stopClaiming:
			w.log.Warn("stopped claiming: this worker was declared dead")
			return &wg
		default:
		}

		free := w.freeSlots()
		if free == 0 {
			// All slots busy. Sleep briefly rather than spinning.
			if !w.sleep(ctx, 50*time.Millisecond) {
				return &wg
			}
			continue
		}

		// Claim only as many as can be run right now. Claiming more would mean
		// holding leases on work sitting in a local queue -- if this worker
		// then died, that work would be stuck until the lease expired despite
		// never having been started.
		claimed, err := w.store.ClaimJobs(ctx, w.ID, free, w.opts.LeaseDuration)
		if err != nil {
			if ctx.Err() != nil {
				return &wg
			}
			// A database blip. Back off rather than hammering it.
			w.log.Warn("claiming jobs failed", "error", err)
			if !w.sleep(ctx, w.opts.PollInterval) {
				return &wg
			}
			continue
		}

		if len(claimed) == 0 {
			// Empty queue. Jittered sleep so N workers polling an idle queue
			// do not synchronise into a thundering herd.
			if !w.sleep(ctx, w.jitteredPoll()) {
				return &wg
			}
			continue
		}

		for _, job := range claimed {
			w.addInFlight(1)
			wg.Add(1)
			go func(j jobs.Job) {
				defer wg.Done()
				defer w.addInFlight(-1)
				w.execute(ctx, j)
			}(job)
		}
	}
}

// execute runs one job and records the outcome.
func (w *Worker) execute(ctx context.Context, job jobs.Job) {
	log := w.log.With("job_id", job.ID, "job_type", job.Type, "attempt", job.AttemptCount)

	execID, err := w.store.RecordExecutionStart(ctx, job.ID, w.ID, job.AttemptCount)
	if err != nil {
		// Losing the audit row is not worth abandoning the work over; log and
		// continue with execID 0, which the end-recorder skips.
		log.Warn("recording execution start", "error", err)
	}

	handler, ok := w.handlers[job.Type]
	if !ok {
		msg := fmt.Sprintf("no handler registered for job type %q", job.Type)
		log.Error("unhandled job type", "job_type", job.Type)
		w.finishFailed(ctx, job, execID, msg, true)
		return
	}

	start := time.Now()
	err = handler(ctx, job)
	elapsed := time.Since(start)

	if err != nil {
		// A deferral is not a failure: the handler declined to run because its
		// prerequisites are not ready. Reschedule without consuming an attempt.
		var def *jobs.DeferError
		if errors.As(err, &def) {
			deferCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
			applied, derr := w.store.DeferJob(deferCtx, job.ID, w.ID, def.After, def.Error())
			cancel()
			if derr != nil {
				log.Error("deferring job", "error", derr)
			} else if !applied {
				log.Warn("lost lease before deferring")
			}
			if execID > 0 {
				msg := def.Error()
				_ = w.store.RecordExecutionEnd(context.WithoutCancel(ctx), execID, jobs.OutcomeDeferred, &msg)
			}
			log.Debug("job deferred", "after", def.After, "reason", def.Reason)
			return
		}

		// A cancelled context during shutdown is not a job failure. Leave the
		// job leased; the shutdown path releases it, or the lease expires.
		if ctx.Err() != nil && errors.Is(err, context.Canceled) {
			log.Info("job interrupted by shutdown; leaving it for reassignment")
			if execID > 0 {
				msg := "interrupted by shutdown"
				_ = w.store.RecordExecutionEnd(context.WithoutCancel(ctx), execID, jobs.OutcomeAbandoned, &msg)
			}
			return
		}

		var perm *jobs.PermanentError
		permanent := errors.As(err, &perm)
		log.Warn("job failed", "error", err, "permanent", permanent, "duration_ms", elapsed.Milliseconds())
		w.finishFailed(ctx, job, execID, err.Error(), permanent)
		return
	}

	applied, err := w.store.CompleteJob(ctx, job.ID, w.ID)
	if err != nil {
		log.Error("marking job complete", "error", err)
		return
	}
	if !applied {
		// The lease was reaped while the handler ran, and another worker now
		// owns this job. The work was still done and its writes are idempotent,
		// so nothing is corrupt -- but this worker must not claim the outcome.
		log.Warn("lost lease before completion; another worker owns this job now")
		if execID > 0 {
			msg := "lease lost before completion"
			_ = w.store.RecordExecutionEnd(ctx, execID, jobs.OutcomeAbandoned, &msg)
		}
		return
	}

	if execID > 0 {
		_ = w.store.RecordExecutionEnd(ctx, execID, jobs.OutcomeSucceeded, nil)
	}
	log.Debug("job succeeded", "duration_ms", elapsed.Milliseconds())
}

func (w *Worker) finishFailed(ctx context.Context, job jobs.Job, execID int64, msg string, permanent bool) {
	// Use a live context: ctx may be cancelled, and failing to record the
	// failure would leave the job leased until its lease expired.
	recordCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()

	w.rngMu.Lock()
	backoff := jobs.Backoff(job.AttemptCount, w.rng)
	w.rngMu.Unlock()

	dead, applied, err := w.store.FailJob(recordCtx, job.ID, w.ID, msg, backoff, permanent)
	if err != nil {
		w.log.Error("recording job failure", "job_id", job.ID, "error", err)
		return
	}
	if !applied {
		w.log.Warn("lost lease before recording failure", "job_id", job.ID)
	}
	if dead {
		w.log.Error("job is dead: no attempts remain",
			"job_id", job.ID, "job_type", job.Type, "last_error", msg)
	}

	if execID > 0 {
		_ = w.store.RecordExecutionEnd(recordCtx, execID, jobs.OutcomeFailed, &msg)
	}
}

// heartbeatLoop tells the database this worker is alive.
func (w *Worker) heartbeatLoop(ctx context.Context) {
	t := time.NewTicker(jobs.HeartbeatInterval)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			active, err := w.store.Heartbeat(ctx, w.ID)
			if err != nil {
				if ctx.Err() == nil {
					w.log.Warn("heartbeat failed", "error", err)
				}
				continue
			}
			if !active {
				// The reaper declared this worker dead while it was still
				// running -- typically a long database partition. Its leases
				// have been taken. Stop claiming rather than competing with
				// whichever worker picked the work up.
				w.log.Error("this worker was declared dead; halting claims")
				w.stopOnce.Do(func() { close(w.stopClaiming) })
			}
		}
	}
}

// renewLoop extends the leases on jobs this worker holds.
//
// Renewing every 5s against a 60s lease gives twelve chances to succeed before
// the lease lapses, so brief slowness never costs a job.
func (w *Worker) renewLoop(ctx context.Context) {
	t := time.NewTicker(jobs.LeaseRenewInterval)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			held := w.currentInFlight()
			if held == 0 {
				continue
			}

			renewed, err := w.store.RenewLeases(ctx, w.ID, w.opts.LeaseDuration)
			if err != nil {
				if ctx.Err() == nil {
					w.log.Warn("renewing leases failed", "error", err)
				}
				continue
			}
			if renewed < held {
				// Fewer rows updated than jobs running: the reaper took some.
				// Worth surfacing loudly -- it means this worker was
				// unreachable long enough to be considered dead.
				w.log.Warn("some leases were lost to the reaper",
					"in_flight", held, "renewed", renewed)
			}
		}
	}
}

// reaperLoop reclaims work abandoned by dead workers.
//
// Every worker runs this. It is not a singleton role, because the reaping
// UPDATE is atomic and SKIP LOCKED means concurrent reapers do not collide --
// re-reaping an already-reaped job simply matches no rows. Making it a
// singleton would mean electing a leader, which is a whole distributed-systems
// problem to solve for an operation that is naturally idempotent.
func (w *Worker) reaperLoop(ctx context.Context) {
	const interval = 15 * time.Second

	t := time.NewTicker(interval)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			w.rngMu.Lock()
			backoff := jobs.Backoff(1, w.rng)
			w.rngMu.Unlock()

			requeued, died, err := w.store.ReapExpiredLeases(ctx, backoff)
			if err != nil {
				if ctx.Err() == nil {
					w.log.Warn("reaping expired leases failed", "error", err)
				}
			} else if requeued > 0 || died > 0 {
				w.log.Warn("reclaimed jobs from expired leases",
					"requeued", requeued, "died", died)
			}

			n, err := w.store.ReapDeadWorkers(ctx, jobs.DeadWorkerThreshold)
			if err != nil {
				if ctx.Err() == nil {
					w.log.Warn("reaping dead workers failed", "error", err)
				}
			} else if n > 0 {
				w.log.Warn("marked workers dead", "count", n)
			}
		}
	}
}

// --- small helpers --------------------------------------------------------

func (w *Worker) freeSlots() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	n := w.opts.Concurrency - w.inFlight
	if n < 0 {
		return 0
	}
	return n
}

func (w *Worker) currentInFlight() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.inFlight
}

func (w *Worker) addInFlight(delta int) {
	w.mu.Lock()
	w.inFlight += delta
	w.mu.Unlock()
}

// jitteredPoll spreads idle polling so N workers do not synchronise.
func (w *Worker) jitteredPoll() time.Duration {
	w.rngMu.Lock()
	f := 0.5 + w.rng.Float64()
	w.rngMu.Unlock()
	return time.Duration(float64(w.opts.PollInterval) * f)
}

// sleep waits for d or until ctx is cancelled. Returns false if cancelled.
func (w *Worker) sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-w.stopClaiming:
		return false
	case <-t.C:
		return true
	}
}
