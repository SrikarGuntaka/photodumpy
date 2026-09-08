//go:build integration

// Integration tests for the job queue and worker runtime.
//
// These need a real Postgres because the behaviour under test IS the database
// behaviour: SKIP LOCKED under genuine concurrency, lease expiry against the
// server clock, and atomic claim semantics. A mock would test the mock.
//
//	go test -tags integration ./internal/worker/
package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/srikarguntaka/photo-organizer/internal/database"
	"github.com/srikarguntaka/photo-organizer/internal/jobs"
	"github.com/srikarguntaka/photo-organizer/internal/photos"
	"github.com/srikarguntaka/photo-organizer/internal/store"
	"github.com/srikarguntaka/photo-organizer/migrations"
)

func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()

	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping integration test")
	}

	ctx := context.Background()
	log := testLogger(t)

	pool, err := database.Connect(ctx, dsn, 24, 30*time.Second, log)
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}
	if err := database.Migrate(ctx, pool, migrations.FS(), log); err != nil {
		t.Fatalf("migrating: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func testLogger(t *testing.T) *slog.Logger {
	t.Helper()
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

// testLibrary creates an isolated library so parallel runs do not collide.
func testLibrary(t *testing.T, st *store.Store) string {
	t.Helper()
	root := t.TempDir()

	lib, _, err := st.CreateLibrary(context.Background(),
		fmt.Sprintf("wtest-%d", time.Now().UnixNano()), root, photos.SourceLocalFS)
	if err != nil {
		t.Fatalf("creating library: %v", err)
	}
	t.Cleanup(func() {
		_, _ = st.Pool().Exec(context.Background(), `DELETE FROM libraries WHERE id = $1`, lib.ID)
	})
	return lib.ID
}

// enqueueN queues n no-op jobs against a library.
func enqueueN(t *testing.T, st *store.Store, libraryID string, n int) {
	t.Helper()

	batch := make([]jobs.Enqueue, n)
	for i := range batch {
		// A distinct synthetic target per job so each gets its own dedupe key.
		batch[i] = jobs.NewPhotoJob(jobs.TypeExtractMetadata, uuid.NewString(), libraryID)
	}
	inserted, err := st.EnqueueJobs(context.Background(), batch)
	if err != nil {
		t.Fatalf("enqueuing: %v", err)
	}
	if inserted != n {
		t.Fatalf("enqueued %d jobs, want %d", inserted, n)
	}
}

// ---------------------------------------------------------------------------
// Claiming
// ---------------------------------------------------------------------------

// SKIP LOCKED's entire purpose: N workers claiming simultaneously must partition
// the work, never hand the same job to two workers.
func TestConcurrentClaimsNeverOverlap(t *testing.T) {
	st := store.New(testPool(t))
	libID := testLibrary(t, st)

	const totalJobs = 300
	const workers = 8
	enqueueN(t, st, libID, totalJobs)

	var mu sync.Mutex
	claimedBy := map[int64]string{}
	var wg sync.WaitGroup

	for i := 0; i < workers; i++ {
		workerID := uuid.NewString()
		if err := st.RegisterWorker(context.Background(), workerID, "test", os.Getpid(), 4); err != nil {
			t.Fatal(err)
		}

		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			for {
				claimed, err := st.ClaimJobs(context.Background(), id, 10, time.Minute)
				if err != nil {
					t.Errorf("claim: %v", err)
					return
				}
				if len(claimed) == 0 {
					return
				}
				mu.Lock()
				for _, j := range claimed {
					if prev, dup := claimedBy[j.ID]; dup {
						t.Errorf("job %d claimed by both %s and %s -- SKIP LOCKED failed",
							j.ID, prev, id)
					}
					claimedBy[j.ID] = id
				}
				mu.Unlock()
			}
		}(workerID)
	}
	wg.Wait()

	if len(claimedBy) != totalJobs {
		t.Errorf("claimed %d distinct jobs, enqueued %d", len(claimedBy), totalJobs)
	}

	// Work should actually be spread, not all taken by whoever got there first.
	holders := map[string]int{}
	for _, id := range claimedBy {
		holders[id]++
	}
	if len(holders) < 2 {
		t.Errorf("all %d jobs went to %d worker(s); SKIP LOCKED should let workers "+
			"proceed in parallel rather than queueing behind one another",
			totalJobs, len(holders))
	}
	t.Logf("%d jobs partitioned across %d workers with zero overlap", totalJobs, len(holders))
}

// A claim marks the job running, sets a lease, and consumes an attempt.
func TestClaimSetsLeaseAndConsumesAttempt(t *testing.T) {
	st := store.New(testPool(t))
	libID := testLibrary(t, st)
	enqueueN(t, st, libID, 1)

	workerID := uuid.NewString()
	if err := st.RegisterWorker(context.Background(), workerID, "test", 1, 1); err != nil {
		t.Fatal(err)
	}

	claimed, err := st.ClaimJobs(context.Background(), workerID, 5, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(claimed) != 1 {
		t.Fatalf("claimed %d jobs, want 1", len(claimed))
	}
	j := claimed[0]

	if j.Status != jobs.StatusRunning {
		t.Errorf("status = %s, want running", j.Status)
	}
	if j.LeasedBy == nil || *j.LeasedBy != workerID {
		t.Errorf("leased_by = %v, want %s", j.LeasedBy, workerID)
	}
	if j.LeaseExpiresAt == nil || !j.LeaseExpiresAt.After(time.Now()) {
		t.Errorf("lease_expires_at = %v, want a future time", j.LeaseExpiresAt)
	}
	// Counting at claim time -- not on failure -- is what makes a worker that
	// crashes without reporting still consume an attempt.
	if j.AttemptCount != 1 {
		t.Errorf("attempt_count = %d after first claim, want 1", j.AttemptCount)
	}
	if j.StartedAt == nil {
		t.Error("started_at was not set")
	}
}

// A job scheduled into the future must not be claimable yet.
func TestBackoffDelaysClaimability(t *testing.T) {
	st := store.New(testPool(t))
	libID := testLibrary(t, st)
	enqueueN(t, st, libID, 1)

	workerID := uuid.NewString()
	if err := st.RegisterWorker(context.Background(), workerID, "test", 1, 1); err != nil {
		t.Fatal(err)
	}

	claimed, _ := st.ClaimJobs(context.Background(), workerID, 1, time.Minute)
	if len(claimed) != 1 {
		t.Fatal("expected to claim one job")
	}

	// Fail it with a long backoff.
	_, applied, err := st.FailJob(context.Background(), claimed[0].ID, workerID,
		"simulated transient failure", time.Hour, false)
	if err != nil {
		t.Fatal(err)
	}
	if !applied {
		t.Fatal("FailJob did not apply")
	}

	again, err := st.ClaimJobs(context.Background(), workerID, 5, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != 0 {
		t.Errorf("claimed %d jobs that are backed off until later; next_attempt_at is not "+
			"being honoured", len(again))
	}
}

// ---------------------------------------------------------------------------
// Lease expiry and crash recovery
// ---------------------------------------------------------------------------

// THE crash-recovery test. A worker takes work and dies without reporting.
// The lease expires, the reaper returns the job to the queue, and another
// worker completes it. Nothing is lost.
func TestCrashedWorkerJobsAreReclaimed(t *testing.T) {
	st := store.New(testPool(t))
	libID := testLibrary(t, st)

	const total = 20
	enqueueN(t, st, libID, total)

	// A worker claims everything with a lease short enough to expire during
	// the test, then "crashes" -- we simply stop touching it. No release, no
	// unregister, no lease renewal: exactly what a SIGKILL looks like.
	crashed := uuid.NewString()
	if err := st.RegisterWorker(context.Background(), crashed, "crashed-host", 999, 20); err != nil {
		t.Fatal(err)
	}

	const shortLease = 2 * time.Second
	claimed, err := st.ClaimJobs(context.Background(), crashed, total, shortLease)
	if err != nil {
		t.Fatal(err)
	}
	if len(claimed) != total {
		t.Fatalf("claimed %d, want %d", len(claimed), total)
	}
	for _, j := range claimed {
		if _, err := st.RecordExecutionStart(context.Background(), j.ID, crashed, j.AttemptCount); err != nil {
			t.Fatal(err)
		}
	}
	t.Logf("worker %s claimed %d jobs, then died", crashed[:8], total)

	// Before expiry, the work must NOT be claimable -- otherwise the lease is
	// doing nothing and two workers would run the same job.
	survivor := uuid.NewString()
	if err := st.RegisterWorker(context.Background(), survivor, "survivor", 1000, 20); err != nil {
		t.Fatal(err)
	}
	early, err := st.ClaimJobs(context.Background(), survivor, total, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(early) != 0 {
		t.Fatalf("claimed %d jobs that are still under a valid lease; the lease is not "+
			"protecting in-flight work", len(early))
	}

	// Wait out the lease.
	time.Sleep(shortLease + time.Second)

	requeued, died, err := st.ReapExpiredLeases(context.Background(), time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("reaper requeued %d, killed %d", requeued, died)
	if requeued != total {
		t.Errorf("reaper requeued %d jobs, want %d -- work was lost", requeued, total)
	}

	// The survivor can now claim and finish everything.
	recovered, err := st.ClaimJobs(context.Background(), survivor, total, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(recovered) != total {
		t.Errorf("survivor claimed %d of %d reclaimed jobs", len(recovered), total)
	}
	for _, j := range recovered {
		ok, err := st.CompleteJob(context.Background(), j.ID, survivor)
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			t.Errorf("could not complete reclaimed job %d", j.ID)
		}
		// The attempt from the crashed worker must have been counted.
		if j.AttemptCount != 2 {
			t.Errorf("job %d attempt_count = %d, want 2 (the crashed attempt must count, "+
				"or a job that reliably kills workers would retry forever)", j.ID, j.AttemptCount)
		}
	}

	counts, err := st.CountAllJobs(context.Background(), libID)
	if err != nil {
		t.Fatal(err)
	}
	if counts.Succeeded != total {
		t.Errorf("succeeded = %d, want %d (%+v)", counts.Succeeded, total, counts)
	}
	if counts.Pending != 0 || counts.Running != 0 || counts.Dead != 0 {
		t.Errorf("queue not drained: %+v", counts)
	}

	// The audit trail must show the crashed attempts as abandoned rather than
	// silently missing.
	var abandoned int
	if err := st.Pool().QueryRow(context.Background(),
		`SELECT count(*) FROM job_executions WHERE worker_id = $1 AND outcome = 'abandoned'`,
		crashed).Scan(&abandoned); err != nil {
		t.Fatal(err)
	}
	if abandoned != total {
		t.Errorf("%d abandoned executions recorded, want %d", abandoned, total)
	}
}

// A lease that is being renewed must never be reaped.
func TestRenewedLeasesSurviveTheReaper(t *testing.T) {
	st := store.New(testPool(t))
	libID := testLibrary(t, st)
	enqueueN(t, st, libID, 5)

	workerID := uuid.NewString()
	if err := st.RegisterWorker(context.Background(), workerID, "test", 1, 5); err != nil {
		t.Fatal(err)
	}

	const shortLease = 2 * time.Second
	claimed, err := st.ClaimJobs(context.Background(), workerID, 5, shortLease)
	if err != nil {
		t.Fatal(err)
	}
	if len(claimed) != 5 {
		t.Fatalf("claimed %d, want 5", len(claimed))
	}

	// Renew across a span longer than the lease.
	deadline := time.Now().Add(shortLease + 2*time.Second)
	for time.Now().Before(deadline) {
		n, err := st.RenewLeases(context.Background(), workerID, shortLease)
		if err != nil {
			t.Fatal(err)
		}
		if n != 5 {
			t.Fatalf("renewed %d leases, want 5", n)
		}
		time.Sleep(400 * time.Millisecond)
	}

	requeued, _, err := st.ReapExpiredLeases(context.Background(), time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if requeued != 0 {
		t.Errorf("reaper took %d actively-renewed jobs; renewal is not protecting them", requeued)
	}
}

// Clean shutdown releases leases immediately rather than waiting them out.
func TestGracefulReleaseReturnsWorkImmediately(t *testing.T) {
	st := store.New(testPool(t))
	libID := testLibrary(t, st)
	enqueueN(t, st, libID, 10)

	leaving := uuid.NewString()
	if err := st.RegisterWorker(context.Background(), leaving, "test", 1, 10); err != nil {
		t.Fatal(err)
	}

	// A long lease: without an explicit release this work would be stuck for
	// an hour.
	claimed, err := st.ClaimJobs(context.Background(), leaving, 10, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if len(claimed) != 10 {
		t.Fatalf("claimed %d, want 10", len(claimed))
	}

	released, err := st.ReleaseWorkerLeases(context.Background(), leaving)
	if err != nil {
		t.Fatal(err)
	}
	if released != 10 {
		t.Errorf("released %d leases, want 10", released)
	}

	other := uuid.NewString()
	if err := st.RegisterWorker(context.Background(), other, "test", 2, 10); err != nil {
		t.Fatal(err)
	}
	again, err := st.ClaimJobs(context.Background(), other, 10, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != 10 {
		t.Errorf("another worker claimed %d of the released jobs, want 10 -- clean shutdown "+
			"should reassign work in milliseconds, not after the lease elapses", len(again))
	}
}

// A worker whose lease was reaped must not be able to mark the job done: the
// job now belongs to someone else, who will report its own outcome.
func TestReapedWorkerCannotCompleteOrFail(t *testing.T) {
	st := store.New(testPool(t))
	libID := testLibrary(t, st)
	enqueueN(t, st, libID, 1)

	slow := uuid.NewString()
	if err := st.RegisterWorker(context.Background(), slow, "slow", 1, 1); err != nil {
		t.Fatal(err)
	}

	claimed, err := st.ClaimJobs(context.Background(), slow, 1, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	jobID := claimed[0].ID

	time.Sleep(1500 * time.Millisecond)
	if _, _, err := st.ReapExpiredLeases(context.Background(), time.Millisecond); err != nil {
		t.Fatal(err)
	}

	ok, err := st.CompleteJob(context.Background(), jobID, slow)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Error("a reaped worker was able to mark its old job succeeded; the lease " +
			"ownership check is not working")
	}

	_, applied, err := st.FailJob(context.Background(), jobID, slow, "late failure", time.Second, false)
	if err != nil {
		t.Fatal(err)
	}
	if applied {
		t.Error("a reaped worker was able to record a failure on its old job")
	}
}

// ---------------------------------------------------------------------------
// Retries
// ---------------------------------------------------------------------------

// A job must retry up to max_attempts, then die -- not loop forever.
func TestJobDiesAfterMaxAttempts(t *testing.T) {
	st := store.New(testPool(t))
	libID := testLibrary(t, st)

	const maxAttempts = 3
	e := jobs.NewPhotoJob(jobs.TypeExtractMetadata, uuid.NewString(), libID)
	e.MaxAttempts = maxAttempts
	if _, err := st.EnqueueJobs(context.Background(), []jobs.Enqueue{e}); err != nil {
		t.Fatal(err)
	}

	workerID := uuid.NewString()
	if err := st.RegisterWorker(context.Background(), workerID, "test", 1, 1); err != nil {
		t.Fatal(err)
	}

	var lastJobID int64
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		claimed, err := st.ClaimJobs(context.Background(), workerID, 1, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		if len(claimed) != 1 {
			t.Fatalf("attempt %d: claimed %d jobs, want 1", attempt, len(claimed))
		}
		lastJobID = claimed[0].ID

		if claimed[0].AttemptCount != attempt {
			t.Errorf("attempt %d: attempt_count = %d", attempt, claimed[0].AttemptCount)
		}

		// Zero backoff so the next claim can happen immediately.
		dead, applied, err := st.FailJob(context.Background(), lastJobID, workerID,
			fmt.Sprintf("failure %d", attempt), 0, false)
		if err != nil {
			t.Fatal(err)
		}
		if !applied {
			t.Fatalf("attempt %d: FailJob did not apply", attempt)
		}

		wantDead := attempt == maxAttempts
		if dead != wantDead {
			t.Errorf("attempt %d of %d: dead = %v, want %v", attempt, maxAttempts, dead, wantDead)
		}
	}

	final, err := st.GetJob(context.Background(), lastJobID)
	if err != nil {
		t.Fatal(err)
	}
	if final.Status != jobs.StatusDead {
		t.Errorf("status = %s after exhausting attempts, want dead", final.Status)
	}
	if final.LastError == nil {
		t.Error("a dead job must retain its last error for diagnosis")
	}

	// And it must stay dead -- not become claimable again.
	if again, _ := st.ClaimJobs(context.Background(), workerID, 5, time.Minute); len(again) != 0 {
		t.Errorf("claimed %d dead jobs; dead must be terminal", len(again))
	}
}

// A permanent failure must skip the remaining attempts entirely.
func TestPermanentFailureSkipsRetries(t *testing.T) {
	st := store.New(testPool(t))
	libID := testLibrary(t, st)
	enqueueN(t, st, libID, 1)

	workerID := uuid.NewString()
	if err := st.RegisterWorker(context.Background(), workerID, "test", 1, 1); err != nil {
		t.Fatal(err)
	}

	claimed, _ := st.ClaimJobs(context.Background(), workerID, 1, time.Minute)
	if len(claimed) != 1 {
		t.Fatal("expected one job")
	}

	dead, applied, err := st.FailJob(context.Background(), claimed[0].ID, workerID,
		"not a decodable image", time.Second, true)
	if err != nil {
		t.Fatal(err)
	}
	if !applied {
		t.Fatal("FailJob did not apply")
	}
	if !dead {
		t.Error("a permanent failure did not kill the job on attempt 1 of 5; retrying a file " +
			"that is not an image wastes four more attempts")
	}
}

// ---------------------------------------------------------------------------
// Enqueue idempotency
// ---------------------------------------------------------------------------

func TestEnqueueIsIdempotentWhileOutstanding(t *testing.T) {
	st := store.New(testPool(t))
	libID := testLibrary(t, st)

	photoID := uuid.NewString()
	e := jobs.NewPhotoJob(jobs.TypeExtractMetadata, photoID, libID)

	first, err := st.EnqueueJobs(context.Background(), []jobs.Enqueue{e})
	if err != nil {
		t.Fatal(err)
	}
	if first != 1 {
		t.Fatalf("first enqueue inserted %d, want 1", first)
	}

	second, err := st.EnqueueJobs(context.Background(), []jobs.Enqueue{e})
	if err != nil {
		t.Fatal(err)
	}
	if second != 0 {
		t.Errorf("second enqueue inserted %d, want 0 -- scanning twice must not double the queue",
			second)
	}

	// A different job type for the same photo is legitimate and must queue.
	other := jobs.NewPhotoJob(jobs.TypeComputeFileHash, photoID, libID)
	n, err := st.EnqueueJobs(context.Background(), []jobs.Enqueue{other})
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("a different job type for the same photo inserted %d, want 1", n)
	}
}

// Once a job has succeeded, the same work may legitimately be queued again --
// that is a rescan, not a duplicate.
func TestEnqueueAllowedAfterSuccess(t *testing.T) {
	st := store.New(testPool(t))
	libID := testLibrary(t, st)

	photoID := uuid.NewString()
	e := jobs.NewPhotoJob(jobs.TypeExtractMetadata, photoID, libID)
	if _, err := st.EnqueueJobs(context.Background(), []jobs.Enqueue{e}); err != nil {
		t.Fatal(err)
	}

	workerID := uuid.NewString()
	if err := st.RegisterWorker(context.Background(), workerID, "test", 1, 1); err != nil {
		t.Fatal(err)
	}
	claimed, _ := st.ClaimJobs(context.Background(), workerID, 1, time.Minute)
	if len(claimed) != 1 {
		t.Fatal("expected one job")
	}
	if ok, err := st.CompleteJob(context.Background(), claimed[0].ID, workerID); err != nil || !ok {
		t.Fatalf("completing: ok=%v err=%v", ok, err)
	}

	n, err := st.EnqueueJobs(context.Background(), []jobs.Enqueue{e})
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("re-enqueue after success inserted %d, want 1 -- a rescan must be able to "+
			"redo work that previously completed", n)
	}
}

// ---------------------------------------------------------------------------
// Full worker runtime
// ---------------------------------------------------------------------------

// Multiple real workers must drain a queue, each job running at least once and
// no job running on two workers at the same instant.
func TestWorkerPoolDrainsQueueWithoutOverlap(t *testing.T) {
	st := store.New(testPool(t))
	libID := testLibrary(t, st)

	const totalJobs = 120
	const workerCount = 4
	enqueueN(t, st, libID, totalJobs)

	var executed sync.Map // jobID -> execution count
	var concurrentPerJob sync.Map
	var overlaps atomic.Int32

	handler := func(ctx context.Context, job jobs.Job) error {
		// Detect two workers inside the same job at once.
		if _, loaded := concurrentPerJob.LoadOrStore(job.ID, true); loaded {
			overlaps.Add(1)
		}
		defer concurrentPerJob.Delete(job.ID)

		n, _ := executed.LoadOrStore(job.ID, new(atomic.Int32))
		n.(*atomic.Int32).Add(1)

		time.Sleep(5 * time.Millisecond)
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	var wg sync.WaitGroup
	for i := 0; i < workerCount; i++ {
		w := New(st, testLogger(t), Options{
			Concurrency:   4,
			LeaseDuration: 30 * time.Second,
			PollInterval:  100 * time.Millisecond,
			ShutdownGrace: 10 * time.Second,
		})
		w.Register(jobs.TypeExtractMetadata, handler)

		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := w.Run(ctx); err != nil {
				t.Errorf("worker run: %v", err)
			}
		}()
	}

	// Wait for the queue to drain.
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		counts, err := st.CountAllJobs(context.Background(), libID)
		if err != nil {
			t.Fatal(err)
		}
		if counts.Succeeded == totalJobs {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}

	cancel()
	wg.Wait()

	counts, err := st.CountAllJobs(context.Background(), libID)
	if err != nil {
		t.Fatal(err)
	}
	if counts.Succeeded != totalJobs {
		t.Errorf("succeeded = %d of %d (%+v)", counts.Succeeded, totalJobs, counts)
	}
	if overlaps.Load() > 0 {
		t.Errorf("%d jobs were executing on two workers simultaneously", overlaps.Load())
	}

	distinct := 0
	executed.Range(func(_, _ any) bool { distinct++; return true })
	if distinct != totalJobs {
		t.Errorf("%d distinct jobs executed, want %d -- work was lost", distinct, totalJobs)
	}
	t.Logf("%d workers drained %d jobs with zero overlap", workerCount, totalJobs)
}

// The end-to-end resilience claim: kill a worker mid-flight and every job still
// completes.
func TestKilledWorkerMidFlightLosesNoWork(t *testing.T) {
	st := store.New(testPool(t))
	libID := testLibrary(t, st)

	const totalJobs = 60
	enqueueN(t, st, libID, totalJobs)

	var completions sync.Map

	slowHandler := func(ctx context.Context, job jobs.Job) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(80 * time.Millisecond):
		}
		completions.Store(job.ID, true)
		return nil
	}

	// A short lease so the test does not wait a full minute for recovery.
	const lease = 3 * time.Second

	victimCtx, killVictim := context.WithCancel(context.Background())
	victim := New(st, testLogger(t), Options{
		Concurrency:   4,
		LeaseDuration: lease,
		PollInterval:  50 * time.Millisecond,
		// Zero grace and no lease release: this must look like SIGKILL, not a
		// clean shutdown, or the test proves the wrong thing.
		ShutdownGrace: time.Nanosecond,
		DisableReaper: true,
	})
	victim.Register(jobs.TypeExtractMetadata, slowHandler)

	victimDone := make(chan struct{})
	go func() {
		defer close(victimDone)
		_ = victim.Run(victimCtx)
	}()

	// Let it get its teeth into some work.
	time.Sleep(600 * time.Millisecond)

	var running int
	if err := st.Pool().QueryRow(context.Background(),
		`SELECT count(*) FROM jobs WHERE leased_by = $1 AND status = 'running'`,
		victim.ID).Scan(&running); err != nil {
		t.Fatal(err)
	}
	t.Logf("victim holds %d jobs; killing it", running)
	if running == 0 {
		t.Fatal("victim had claimed nothing; the test would prove nothing")
	}

	killVictim()
	<-victimDone

	// Leave the orphaned leases to expire naturally, as a SIGKILL would.
	// (ShutdownGrace of a nanosecond means the release path may still have
	// run; the survivors' reaper handles whatever remains either way.)

	survivorCtx, stopSurvivors := context.WithTimeout(context.Background(), 90*time.Second)
	defer stopSurvivors()

	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		w := New(st, testLogger(t), Options{
			Concurrency:   4,
			LeaseDuration: lease,
			PollInterval:  50 * time.Millisecond,
			ShutdownGrace: 5 * time.Second,
		})
		w.Register(jobs.TypeExtractMetadata, slowHandler)

		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = w.Run(survivorCtx)
		}()
	}

	deadline := time.Now().Add(75 * time.Second)
	var counts store.JobCounts
	for time.Now().Before(deadline) {
		var err error
		counts, err = st.CountAllJobs(context.Background(), libID)
		if err != nil {
			t.Fatal(err)
		}
		if counts.Succeeded == totalJobs {
			break
		}
		time.Sleep(250 * time.Millisecond)
	}

	stopSurvivors()
	wg.Wait()

	if counts.Succeeded != totalJobs {
		t.Errorf("succeeded = %d of %d after a worker was killed mid-flight (%+v) -- "+
			"work was permanently lost", counts.Succeeded, totalJobs, counts)
	}
	if counts.Dead != 0 {
		t.Errorf("%d jobs died; a crash should cause reassignment, not death", counts.Dead)
	}
	t.Logf("all %d jobs completed despite a worker being killed mid-flight", totalJobs)
}

// A worker that shuts down cleanly must hand its work back rather than
// stranding it.
func TestCleanShutdownStrandsNothing(t *testing.T) {
	st := store.New(testPool(t))
	libID := testLibrary(t, st)
	enqueueN(t, st, libID, 20)

	handler := func(ctx context.Context, job jobs.Job) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
		return nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	w := New(st, testLogger(t), Options{
		Concurrency:   4,
		LeaseDuration: time.Hour, // long: only an explicit release can free these
		PollInterval:  50 * time.Millisecond,
		ShutdownGrace: 5 * time.Second,
	})
	w.Register(jobs.TypeExtractMetadata, handler)

	done := make(chan struct{})
	go func() { defer close(done); _ = w.Run(ctx) }()

	time.Sleep(500 * time.Millisecond)
	cancel()
	<-done

	// Nothing may remain leased to the departed worker.
	var stranded int
	if err := st.Pool().QueryRow(context.Background(),
		`SELECT count(*) FROM jobs WHERE leased_by = $1 AND status = 'running'`,
		w.ID).Scan(&stranded); err != nil {
		t.Fatal(err)
	}
	if stranded != 0 {
		t.Errorf("%d jobs remain leased to a cleanly-stopped worker; with an hour-long lease "+
			"they would be stuck until it expired", stranded)
	}

	workers, err := st.ListWorkers(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, wk := range workers {
		if wk.ID == w.ID && wk.Status != "stopped" {
			t.Errorf("worker status = %s after clean shutdown, want stopped", wk.Status)
		}
	}
}

// A job whose type has no handler must fail permanently rather than retry or
// silently stall the queue.
func TestUnknownJobTypeFailsPermanently(t *testing.T) {
	st := store.New(testPool(t))
	libID := testLibrary(t, st)

	e := jobs.NewPhotoJob(jobs.Type("NO_SUCH_TYPE"), uuid.NewString(), libID)
	if _, err := st.EnqueueJobs(context.Background(), []jobs.Enqueue{e}); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	w := New(st, testLogger(t), Options{
		Concurrency:  2,
		PollInterval: 50 * time.Millisecond,
	})
	// Deliberately no handler registered for that type.
	w.Register(jobs.TypeExtractMetadata, func(context.Context, jobs.Job) error { return nil })

	done := make(chan struct{})
	go func() { defer close(done); _ = w.Run(ctx) }()

	deadline := time.Now().Add(15 * time.Second)
	var counts store.JobCounts
	for time.Now().Before(deadline) {
		var err error
		counts, err = st.CountAllJobs(context.Background(), libID)
		if err != nil {
			t.Fatal(err)
		}
		if counts.Dead == 1 {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	cancel()
	<-done

	if counts.Dead != 1 {
		t.Errorf("unhandled job did not die (%+v); it would sit pending forever, "+
			"silently stalling the queue", counts)
	}
}

// A handler returning jobs.Permanent must not consume the remaining attempts.
func TestHandlerPermanentErrorKillsJobOnFirstAttempt(t *testing.T) {
	st := store.New(testPool(t))
	libID := testLibrary(t, st)
	enqueueN(t, st, libID, 1)

	handler := func(context.Context, jobs.Job) error {
		return jobs.Permanent(errors.New("file is not a decodable image"))
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	w := New(st, testLogger(t), Options{Concurrency: 1, PollInterval: 50 * time.Millisecond})
	w.Register(jobs.TypeExtractMetadata, handler)

	done := make(chan struct{})
	go func() { defer close(done); _ = w.Run(ctx) }()

	deadline := time.Now().Add(15 * time.Second)
	var counts store.JobCounts
	for time.Now().Before(deadline) {
		var err error
		counts, err = st.CountAllJobs(context.Background(), libID)
		if err != nil {
			t.Fatal(err)
		}
		if counts.Dead == 1 {
			break
		}
		time.Sleep(150 * time.Millisecond)
	}
	cancel()
	<-done

	if counts.Dead != 1 {
		t.Fatalf("permanent failure did not kill the job (%+v)", counts)
	}

	var attempts int
	if err := st.Pool().QueryRow(context.Background(),
		`SELECT max(attempt_count) FROM jobs WHERE library_id = $1`, libID).Scan(&attempts); err != nil {
		t.Fatal(err)
	}
	if attempts != 1 {
		t.Errorf("attempt_count = %d, want 1 -- a permanent error must not burn retries", attempts)
	}
}

// ---------------------------------------------------------------------------
// Worker registry
// ---------------------------------------------------------------------------

func TestWorkerRegistrationAndHeartbeat(t *testing.T) {
	st := store.New(testPool(t))

	id := uuid.NewString()
	if err := st.RegisterWorker(context.Background(), id, "host-a", 123, 8); err != nil {
		t.Fatal(err)
	}

	// Re-registering the same id must update, not duplicate.
	if err := st.RegisterWorker(context.Background(), id, "host-a", 123, 8); err != nil {
		t.Fatal(err)
	}

	active, err := st.Heartbeat(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if !active {
		t.Error("heartbeat reported an active worker as inactive")
	}

	workers, err := st.ListWorkers(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	found := 0
	for _, w := range workers {
		if w.ID == id {
			found++
			if w.Status != "active" || w.Concurrency != 8 || w.Hostname != "host-a" {
				t.Errorf("worker row = %+v", w)
			}
		}
	}
	if found != 1 {
		t.Errorf("found %d rows for one worker id, want exactly 1", found)
	}

	if err := st.UnregisterWorker(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	stillActive, err := st.Heartbeat(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if stillActive {
		t.Error("a stopped worker still heartbeats as active; it would keep claiming work")
	}
}

// A worker that stops heartbeating must be marked dead.
func TestDeadWorkerDetection(t *testing.T) {
	st := store.New(testPool(t))

	id := uuid.NewString()
	if err := st.RegisterWorker(context.Background(), id, "zombie", 1, 1); err != nil {
		t.Fatal(err)
	}

	// Backdate the heartbeat rather than sleeping for the real threshold.
	if _, err := st.Pool().Exec(context.Background(),
		`UPDATE workers SET last_heartbeat_at = now() - interval '10 minutes' WHERE id = $1`,
		id); err != nil {
		t.Fatal(err)
	}

	n, err := st.ReapDeadWorkers(context.Background(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if n < 1 {
		t.Error("a worker with a stale heartbeat was not marked dead")
	}

	stillActive, err := st.Heartbeat(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if stillActive {
		t.Error("a worker declared dead still reports as active")
	}
}

// ---------------------------------------------------------------------------
// Deferral / prerequisite ordering
// ---------------------------------------------------------------------------

// Regression test for a real Phase 5 failure: the aggregate ran 33ms before
// the hashing it depends on had finished, grouped over a partially-hashed
// library, and reported 6 duplicate files where the true answer was 9.
//
// Priority orders the CLAIM, not the execution -- a worker claiming a batch
// gets both kinds of job at once and runs them concurrently.
func TestDeferredJobDoesNotConsumeAnAttempt(t *testing.T) {
	st := store.New(testPool(t))
	libID := testLibrary(t, st)
	enqueueN(t, st, libID, 1)

	workerID := uuid.NewString()
	if err := st.RegisterWorker(context.Background(), workerID, "test", 1, 1); err != nil {
		t.Fatal(err)
	}

	claimed, err := st.ClaimJobs(context.Background(), workerID, 1, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(claimed) != 1 {
		t.Fatal("expected one job")
	}
	if claimed[0].AttemptCount != 1 {
		t.Fatalf("attempt_count = %d after claim, want 1", claimed[0].AttemptCount)
	}

	applied, err := st.DeferJob(context.Background(), claimed[0].ID, workerID,
		10*time.Millisecond, "prerequisites not ready")
	if err != nil {
		t.Fatal(err)
	}
	if !applied {
		t.Fatal("DeferJob did not apply")
	}

	after, err := st.GetJob(context.Background(), claimed[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Status != jobs.StatusPending {
		t.Errorf("status = %s after deferral, want pending", after.Status)
	}
	if after.AttemptCount != 0 {
		t.Errorf("attempt_count = %d after deferral, want 0 -- a deferral is not an attempt, "+
			"or a slow prerequisite would exhaust the job's retries while it waits",
			after.AttemptCount)
	}
	if after.LeasedBy != nil {
		t.Error("a deferred job is still leased")
	}
}

// A job that defers many times must never die from waiting.
func TestRepeatedDeferralsNeverExhaustAttempts(t *testing.T) {
	st := store.New(testPool(t))
	libID := testLibrary(t, st)
	enqueueN(t, st, libID, 1)

	workerID := uuid.NewString()
	if err := st.RegisterWorker(context.Background(), workerID, "test", 1, 1); err != nil {
		t.Fatal(err)
	}

	// Far more deferrals than DefaultMaxAttempts.
	for i := 0; i < 20; i++ {
		claimed, err := st.ClaimJobs(context.Background(), workerID, 1, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		if len(claimed) != 1 {
			t.Fatalf("iteration %d: job became unclaimable after %d deferrals -- "+
				"waiting killed it", i, i)
		}
		if _, err := st.DeferJob(context.Background(), claimed[0].ID, workerID, 0, "still waiting"); err != nil {
			t.Fatal(err)
		}
	}

	counts, err := st.CountAllJobs(context.Background(), libID)
	if err != nil {
		t.Fatal(err)
	}
	if counts.Dead != 0 {
		t.Errorf("%d jobs died from repeated deferral; waiting must not be fatal", counts.Dead)
	}
	if counts.Pending != 1 {
		t.Errorf("pending = %d, want 1 (%+v)", counts.Pending, counts)
	}
}

// The end-to-end ordering guarantee: an aggregate must not run until the
// per-photo work it depends on has drained, even when both are claimed
// together.
func TestAggregateWaitsForPrerequisites(t *testing.T) {
	st := store.New(testPool(t))
	libID := testLibrary(t, st)

	// Enough per-photo jobs that they cannot all finish instantly.
	const photoJobs = 40
	batch := make([]jobs.Enqueue, 0, photoJobs+1)
	for i := 0; i < photoJobs; i++ {
		batch = append(batch, jobs.NewPhotoJob(jobs.TypeComputeFileHash, uuid.NewString(), libID))
	}
	batch = append(batch, jobs.NewLibraryJob(jobs.TypeBuildDuplicateGroups, libID))
	if _, err := st.EnqueueJobs(context.Background(), batch); err != nil {
		t.Fatal(err)
	}

	var hashesDone atomic.Int32
	var aggregateRanEarly atomic.Bool
	var aggregateRuns atomic.Int32

	hashHandler := func(ctx context.Context, job jobs.Job) error {
		time.Sleep(30 * time.Millisecond)
		hashesDone.Add(1)
		return nil
	}

	aggregateHandler := func(ctx context.Context, job jobs.Job) error {
		outstanding, err := st.CountOutstandingJobsOfType(ctx, job.LibraryID, jobs.TypeComputeFileHash)
		if err != nil {
			return err
		}
		if outstanding > 0 {
			return jobs.Defer(50*time.Millisecond,
				fmt.Sprintf("%d hash jobs outstanding", outstanding))
		}
		// If this runs while hashes are incomplete, the guarantee is broken.
		if hashesDone.Load() < photoJobs {
			aggregateRanEarly.Store(true)
		}
		aggregateRuns.Add(1)
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Concurrency deliberately EXCEEDS the total job count, so every job --
	// including the aggregate -- is claimed in a single batch and starts
	// executing at once. That is what reproduces the production failure:
	// priority ordered the claim, but once claimed the aggregate ran
	// concurrently with the hashing it depends on.
	//
	// With a smaller pool the aggregate lands in a later batch and the race
	// often does not occur, which would make this test pass for the wrong
	// reason.
	w := New(st, testLogger(t), Options{
		Concurrency:   photoJobs + 10,
		LeaseDuration: 30 * time.Second,
		PollInterval:  50 * time.Millisecond,
		ShutdownGrace: 5 * time.Second,
	})
	w.Register(jobs.TypeComputeFileHash, hashHandler)
	w.Register(jobs.TypeBuildDuplicateGroups, aggregateHandler)

	done := make(chan struct{})
	go func() { defer close(done); _ = w.Run(ctx) }()

	deadline := time.Now().Add(45 * time.Second)
	var counts store.JobCounts
	for time.Now().Before(deadline) {
		var err error
		counts, err = st.CountAllJobs(context.Background(), libID)
		if err != nil {
			t.Fatal(err)
		}
		if counts.Succeeded == photoJobs+1 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	cancel()
	<-done

	if aggregateRanEarly.Load() {
		t.Error("the aggregate ran before its prerequisites finished -- it would produce " +
			"a confident answer over incomplete data")
	}
	if counts.Succeeded != photoJobs+1 {
		t.Errorf("succeeded = %d of %d (%+v)", counts.Succeeded, photoJobs+1, counts)
	}
	if aggregateRuns.Load() != 1 {
		t.Errorf("aggregate did real work %d times, want exactly 1", aggregateRuns.Load())
	}

	// The deferrals must be visible in the audit trail rather than looking like
	// failures or abandonment.
	var deferrals int
	if err := st.Pool().QueryRow(context.Background(), `
		SELECT count(*) FROM job_executions e
		JOIN jobs j ON j.id = e.job_id
		WHERE j.library_id = $1 AND e.outcome = 'deferred'`, libID).Scan(&deferrals); err != nil {
		t.Fatal(err)
	}
	t.Logf("aggregate deferred %d times, then ran once over the complete set", deferrals)
	if deferrals == 0 {
		t.Error("no deferrals recorded; the aggregate never actually had to wait, " +
			"so this test proved nothing")
	}
}
