package jobs

import (
	"errors"
	"math/rand"
	"testing"
	"time"
)

func testRNG() *rand.Rand { return rand.New(rand.NewSource(42)) }

// Backoff must grow, and must stop growing. An uncapped exponential reaches
// hours by attempt 15, which in practice means the job never runs again.
func TestBackoffGrowsThenCaps(t *testing.T) {
	rng := testRNG()

	var prev time.Duration
	for attempt := 1; attempt <= 12; attempt++ {
		// Average several samples: individual values carry +/-25% jitter, so a
		// single sample can dip below its predecessor legitimately.
		var total time.Duration
		const samples = 200
		for i := 0; i < samples; i++ {
			total += Backoff(attempt, rng)
		}
		avg := total / samples

		if avg > backoffCap {
			t.Errorf("attempt %d: average backoff %v exceeds the cap %v", attempt, avg, backoffCap)
		}
		if attempt > 1 && avg < prev && prev < backoffCap/2 {
			t.Errorf("attempt %d: average backoff %v is below attempt %d's %v; it should grow until capped",
				attempt, avg, attempt-1, prev)
		}
		prev = avg
	}

	// Deep into the schedule it must be pinned at the cap, not still doubling.
	for _, attempt := range []int{20, 50, 1000} {
		d := Backoff(attempt, rng)
		if d > backoffCap {
			t.Errorf("attempt %d: backoff %v exceeds cap %v", attempt, d, backoffCap)
		}
	}
}

// A huge attempt count must not overflow into a negative or absurd duration.
func TestBackoffDoesNotOverflow(t *testing.T) {
	rng := testRNG()

	for _, attempt := range []int{0, -5, 63, 64, 1 << 20} {
		d := Backoff(attempt, rng)
		if d <= 0 {
			t.Errorf("attempt %d produced a non-positive backoff %v", attempt, d)
		}
		if d > backoffCap {
			t.Errorf("attempt %d produced %v, above the cap %v", attempt, d, backoffCap)
		}
	}
}

// Jitter is what stops a fleet of jobs that failed together from retrying in
// lockstep. Without it a Postgres restart produces a thundering herd.
func TestBackoffIsJittered(t *testing.T) {
	rng := testRNG()

	seen := map[time.Duration]bool{}
	for i := 0; i < 100; i++ {
		seen[Backoff(5, rng)] = true
	}
	if len(seen) < 50 {
		t.Errorf("only %d distinct backoff values across 100 calls at the same attempt; "+
			"jitter is not spreading retries", len(seen))
	}
}

// The jitter must stay within the stated band, or the schedule is not what the
// comments claim.
func TestBackoffJitterStaysInBand(t *testing.T) {
	rng := testRNG()

	// Attempt 3 = 2s * 2^2 = 8s nominal, so 6s..10s with +/-25%.
	const nominal = 8 * time.Second
	lo, hi := time.Duration(float64(nominal)*0.74), time.Duration(float64(nominal)*1.26)

	for i := 0; i < 1000; i++ {
		d := Backoff(3, rng)
		if d < lo || d > hi {
			t.Fatalf("Backoff(3) = %v, outside the expected jitter band %v..%v", d, lo, hi)
		}
	}
}

// The lease must be renewed many times over before it lapses. If this ratio
// ever narrowed to 2 or 3, ordinary database slowness would start costing jobs.
func TestLeaseRenewalHasComfortableMargin(t *testing.T) {
	ratio := DefaultLeaseDuration / LeaseRenewInterval
	if ratio < 6 {
		t.Errorf("lease %v renewed every %v gives only %d attempts before expiry; "+
			"a transient stall would spuriously lose work",
			DefaultLeaseDuration, LeaseRenewInterval, ratio)
	}
}

// A worker must be declared dead well before jobs are reaped from it, or the
// two mechanisms fight.
func TestDeadWorkerThresholdIsSeveralHeartbeats(t *testing.T) {
	if DeadWorkerThreshold < 2*HeartbeatInterval {
		t.Errorf("DeadWorkerThreshold %v is under two heartbeat intervals (%v); "+
			"a single missed beat would mark a healthy worker dead",
			DeadWorkerThreshold, HeartbeatInterval)
	}
	if DeadWorkerThreshold >= DefaultLeaseDuration {
		t.Errorf("DeadWorkerThreshold %v >= lease %v; a worker would keep its leases "+
			"past the point it is considered dead", DeadWorkerThreshold, DefaultLeaseDuration)
	}
}

// The dedupe key is what makes enqueueing idempotent; it must be stable and
// must distinguish both dimensions.
func TestDedupeKey(t *testing.T) {
	a := DedupeKey(TypeExtractMetadata, "photo-1")
	if a != DedupeKey(TypeExtractMetadata, "photo-1") {
		t.Error("DedupeKey is not stable for identical input")
	}
	if a == DedupeKey(TypeComputeFileHash, "photo-1") {
		t.Error("different job types share a dedupe key; both would not be able to queue")
	}
	if a == DedupeKey(TypeExtractMetadata, "photo-2") {
		t.Error("different targets share a dedupe key; only one photo would be processed")
	}
}

// Aggregates must sort after per-photo work, so the fan-out they depend on
// drains first.
func TestAggregatesRunAfterPerPhotoWork(t *testing.T) {
	if PriorityAggregate <= PriorityPerPhoto {
		t.Errorf("PriorityAggregate (%d) must be greater than PriorityPerPhoto (%d); "+
			"lower runs sooner, and an aggregate over incomplete data is wasted work",
			PriorityAggregate, PriorityPerPhoto)
	}
}

func TestJobConstructors(t *testing.T) {
	p := NewPhotoJob(TypeComputeFileHash, "photo-1", "lib-1")
	if p.TargetType != TargetPhoto || p.TargetID != "photo-1" || p.LibraryID != "lib-1" {
		t.Errorf("NewPhotoJob produced %+v", p)
	}
	if p.Priority != PriorityPerPhoto {
		t.Errorf("photo job priority = %d, want %d", p.Priority, PriorityPerPhoto)
	}

	l := NewLibraryJob(TypeBuildDuplicateGroups, "lib-1")
	if l.TargetType != TargetLibrary {
		t.Errorf("NewLibraryJob target type = %s, want library", l.TargetType)
	}
	// An aggregate targets the library itself, which is what lets one table
	// hold both shapes of job.
	if l.TargetID != l.LibraryID {
		t.Errorf("library job TargetID %q should equal LibraryID %q", l.TargetID, l.LibraryID)
	}
	if l.Priority != PriorityAggregate {
		t.Errorf("library job priority = %d, want %d", l.Priority, PriorityAggregate)
	}
}

func TestAttemptsRemaining(t *testing.T) {
	cases := []struct{ attempts, max, want int }{
		{0, 5, 5},
		{1, 5, 4},
		{5, 5, 0},
		{7, 5, 0}, // must never go negative
	}
	for _, c := range cases {
		j := &Job{AttemptCount: c.attempts, MaxAttempts: c.max}
		if got := j.AttemptsRemaining(); got != c.want {
			t.Errorf("attempt %d of %d: remaining = %d, want %d", c.attempts, c.max, got, c.want)
		}
	}
}

// A permanent error must survive wrapping, or the queue will retry something
// that can never succeed.
func TestPermanentErrorIsDetectableThroughWrapping(t *testing.T) {
	base := errors.New("not a decodable image")
	err := Permanent(base)

	var perm *PermanentError
	if !errors.As(err, &perm) {
		t.Fatal("Permanent error is not detectable with errors.As")
	}
	if !errors.Is(err, base) {
		t.Error("Permanent must not hide the underlying error from errors.Is")
	}
	if err.Error() != base.Error() {
		t.Errorf("Error() = %q, want the wrapped message %q", err.Error(), base.Error())
	}

	// An ordinary error must NOT look permanent, or every transient failure
	// would go straight to dead without a retry.
	if errors.As(errors.New("timeout"), &perm) {
		t.Error("an ordinary error was classified as permanent")
	}
}
