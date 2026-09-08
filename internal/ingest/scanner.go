// Package ingest turns a directory on disk into photo rows.
//
// It sits between internal/photos (which knows how to walk a filesystem but
// nothing about storage) and internal/store (which knows SQL but nothing about
// filesystems). Keeping the orchestration separate is what lets the walk be
// tested against a temp directory with no database, and the SQL be tested with
// no filesystem.
package ingest

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/srikarguntaka/photo-organizer/internal/jobs"
	"github.com/srikarguntaka/photo-organizer/internal/photos"
	"github.com/srikarguntaka/photo-organizer/internal/store"
)

// batchSize is how many discovered files accumulate before a database write.
//
// The trade: larger batches mean fewer round trips but more memory held and
// more work lost if the scan is interrupted mid-batch. 500 rows of path
// strings is on the order of tens of kilobytes, and at 500 a 10,000-photo
// library is 20 round trips instead of 10,000.
const batchSize = 500

// ErrScanInProgress is returned when a scan is already running for a library.
var ErrScanInProgress = errors.New("ingest: a scan is already in progress for this library")

// Scanner discovers photos and persists them.
type Scanner struct {
	store *store.Store
	log   *slog.Logger

	// running tracks in-flight scans in THIS process, keyed by library id.
	//
	// The database guard in MarkScanStarted is the real authority (it survives
	// restarts and would cover multiple API replicas). This map exists so a
	// second request in the same process fails fast with a clear error instead
	// of racing to the database, and so shutdown can wait for active scans.
	mu      sync.Mutex
	running map[string]context.CancelFunc
	wg      sync.WaitGroup
}

func NewScanner(st *store.Store, log *slog.Logger) *Scanner {
	return &Scanner{
		store:   st,
		log:     log,
		running: make(map[string]context.CancelFunc),
	}
}

// ScanSync walks a library's root and inserts every supported photo it finds,
// blocking until done.
//
// Idempotent: running it twice over an unchanged folder inserts nothing the
// second time. The returned ScanResult reports Discovered and Inserted
// separately so that guarantee is visible in the output rather than merely
// claimed.
func (s *Scanner) ScanSync(ctx context.Context, lib *photos.Library) (*photos.ScanResult, error) {
	start := time.Now()

	result := &photos.ScanResult{LibraryID: lib.ID}

	// Accumulate into a batch and flush when full. The walk streams, so peak
	// memory is bounded by batchSize regardless of library size -- a 100,000
	// photo library uses the same memory as a 100 photo one.
	batch := make([]photos.Discovered, 0, batchSize)

	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		inserted, err := s.store.InsertPhotoBatch(ctx, lib.ID, batch)
		if err != nil {
			return err
		}
		result.Inserted += inserted
		batch = batch[:0]
		return nil
	}

	walkStats, walkErr := photos.Walk(ctx, lib.RootPath, photos.DefaultWalkOptions(), func(d photos.Discovered) error {
		batch = append(batch, d)
		if len(batch) >= batchSize {
			return flush()
		}
		return nil
	})

	// Flush whatever the walk left in the batch. Done even when the walk
	// failed: partial results are still correct rows, and inserting them means
	// a resumed scan has less to do.
	if flushErr := flush(); flushErr != nil && walkErr == nil {
		walkErr = flushErr
	}

	result.Discovered = walkStats.Supported
	result.AlreadyKnown = result.Discovered - result.Inserted
	result.SkippedUnsupported = walkStats.SkippedFormat
	result.SkippedHidden = walkStats.SkippedHidden
	result.SkippedTooLarge = walkStats.SkippedTooLarge
	result.Unreadable = walkStats.Unreadable
	result.DurationMS = time.Since(start).Milliseconds()

	if walkErr != nil {
		// Cancellation is not a failure. The rows already written are valid,
		// and a later scan resumes because inserts are idempotent.
		if errors.Is(walkErr, context.Canceled) || errors.Is(walkErr, context.DeadlineExceeded) {
			result.Interrupted = true
			return result, nil
		}
		return result, fmt.Errorf("ingest: scanning %s: %w", lib.RootPath, walkErr)
	}

	return result, nil
}

// StartAsync begins a scan in the background and returns immediately.
//
// Scanning is async because a large library takes longer than a reasonable
// HTTP timeout, and holding a request open for minutes to report progress that
// the database already knows would be worse. The caller polls the library.
//
// The database guard is claimed before returning, so a caller that gets a nil
// error knows the scan is genuinely theirs.
func (s *Scanner) StartAsync(ctx context.Context, lib *photos.Library, force bool) error {
	s.mu.Lock()
	if _, exists := s.running[lib.ID]; exists && !force {
		s.mu.Unlock()
		return ErrScanInProgress
	}
	s.mu.Unlock()

	claimed, err := s.store.MarkScanStarted(ctx, lib.ID, force)
	if err != nil {
		return err
	}
	if !claimed {
		return ErrScanInProgress
	}

	// Deliberately NOT derived from the request context: the scan must outlive
	// the HTTP request that started it. Cancellation comes from Shutdown.
	scanCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))

	s.mu.Lock()
	s.running[lib.ID] = cancel
	s.mu.Unlock()

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer cancel()
		defer func() {
			s.mu.Lock()
			delete(s.running, lib.ID)
			s.mu.Unlock()
		}()

		log := s.log.With("library_id", lib.ID, "root", lib.RootPath)
		log.Info("scan started")

		result, err := s.ScanSync(scanCtx, lib)

		// Closing out the scan must happen even when scanCtx is cancelled --
		// with a cancelled context the UPDATE would never run and the library
		// would stay marked "scanning" forever, refusing every future scan.
		finishCtx, finishCancel := context.WithTimeout(context.WithoutCancel(scanCtx), 10*time.Second)
		defer finishCancel()
		if ferr := s.store.MarkScanFinished(finishCtx, lib.ID); ferr != nil {
			log.Error("failed to mark scan finished", "error", ferr)
		}

		if err != nil {
			log.Error("scan failed", "error", err, "duration_ms", result.DurationMS)
			return
		}
		log.Info("scan complete",
			"discovered", result.Discovered,
			"inserted", result.Inserted,
			"already_known", result.AlreadyKnown,
			"skipped_unsupported", result.SkippedUnsupported,
			"unreadable", result.Unreadable,
			"interrupted", result.Interrupted,
			"duration_ms", result.DurationMS,
		)
	}()

	return nil
}

// IsScanning reports whether this process is currently scanning a library.
func (s *Scanner) IsScanning(libraryID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.running[libraryID]
	return ok
}

// Shutdown cancels in-flight scans and waits for them to wind down.
//
// Called during API shutdown. Cancelled scans are not lost work: rows already
// inserted stay, and the next scan resumes from where this one stopped because
// inserts are idempotent.
func (s *Scanner) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	for _, cancel := range s.running {
		cancel()
	}
	s.mu.Unlock()

	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("ingest: scans did not stop within the shutdown grace period: %w", ctx.Err())
	}
}

// EnqueuePhotoJobs queues the per-photo pipeline for every photo in a library
// that still needs it, plus the aggregate stage that follows.
//
// This is the Phase 5 replacement for the in-process passes: instead of doing
// the work here, the API fans out jobs and the worker pool consumes them. The
// work itself did not move -- the handlers call the same ExtractOne and
// HashOne this package already exposed.
//
// Enqueueing is idempotent (a partial unique index on dedupe_key), so calling
// this repeatedly while jobs are outstanding is a no-op rather than a way to
// multiply the queue.
//
// Paging is by KEYSET (id > cursor), not OFFSET. The listing predicate is
// "still needs work", which enqueueing does not change -- the worker completing
// the job does, later. An OFFSET pager would therefore either loop forever on
// page one or skip rows as the set shifted underneath it. Carrying the last id
// forward is correct regardless of what the workers are doing concurrently.
func (s *Scanner) EnqueuePhotoJobs(ctx context.Context, libraryID string) (int, error) {
	const pageSize = 1000
	total := 0

	for _, spec := range []struct {
		jobType jobs.Type
		list    func(context.Context, string, string, int) ([]string, error)
	}{
		{jobs.TypeExtractMetadata, s.store.ListPhotoIDsNeedingMetadata},
		{jobs.TypeComputeFileHash, s.store.ListPhotoIDsNeedingHash},
	} {
		cursor := ""
		for {
			if ctx.Err() != nil {
				return total, ctx.Err()
			}

			ids, err := spec.list(ctx, libraryID, cursor, pageSize)
			if err != nil {
				return total, err
			}
			if len(ids) == 0 {
				break
			}

			batch := make([]jobs.Enqueue, len(ids))
			for i, id := range ids {
				batch[i] = jobs.NewPhotoJob(spec.jobType, id, libraryID)
			}
			n, err := s.store.EnqueueJobs(ctx, batch)
			if err != nil {
				return total, err
			}
			total += n

			if len(ids) < pageSize {
				break
			}
			cursor = ids[len(ids)-1]
		}
	}

	// The aggregate stage. One per library, at a lower priority so the
	// per-photo work it depends on drains first.
	n, err := s.store.EnqueueJobs(ctx, []jobs.Enqueue{
		jobs.NewLibraryJob(jobs.TypeBuildDuplicateGroups, libraryID),
	})
	if err != nil {
		return total, err
	}
	return total + n, nil
}
