package ingest

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/srikarguntaka/photo-organizer/internal/hashing"
	"github.com/srikarguntaka/photo-organizer/internal/photos"
	"github.com/srikarguntaka/photo-organizer/internal/store"
)

// HashOne computes and stores the SHA-256 of one photo.
//
// As with ExtractOne, this is the unit Phase 5's COMPUTE_FILE_HASH handler will
// call directly; the queue supplies the scheduling this file provides locally.
//
// Idempotent: the digest is a deterministic function of the file's bytes and
// the write is a keyed overwrite, so a re-run after a lease expiry produces the
// same row.
func (p *Processor) HashOne(ctx context.Context, libraryRoot string, ph store.PhotoNeedingHash) error {
	fullPath := filepath.Join(libraryRoot, filepath.FromSlash(ph.RelativePath))

	f, err := os.Open(fullPath)
	if err != nil {
		if os.IsNotExist(err) {
			// A file that has gone away is not a retryable condition. Mark it
			// and move on rather than burning five attempts on it.
			msg := "file no longer exists on disk"
			p.log.Info("photo missing during hashing", "photo_id", ph.ID, "path", ph.RelativePath)
			if err := p.store.UpdatePhotoMetadata(ctx, ph.ID, store.PhotoMetadata{
				State:     photos.StateMissing,
				LastError: &msg,
			}); err != nil {
				return err
			}
			return p.store.SetPhotoHash(ctx, ph.ID, nil, &msg)
		}
		return fmt.Errorf("opening %s: %w", ph.RelativePath, err)
	}
	defer f.Close()

	// Streamed, never buffered whole: a 200MB panorama costs the same memory as
	// a 2MB snapshot.
	sum, n, err := hashing.SumWithSize(f)
	if err != nil {
		// A read failure partway through may be transient (a flaky drive), so
		// it is returned for retry rather than recorded as final.
		return fmt.Errorf("hashing %s: %w", ph.RelativePath, err)
	}

	// A size mismatch means the file changed between the scan and now. The hash
	// is still correct for the CURRENT bytes, which is what matters, but the
	// stored size is stale and worth flagging.
	var note *string
	if ph.FileSizeBytes > 0 && n != ph.FileSizeBytes {
		msg := fmt.Sprintf("file size changed since scan: %d -> %d bytes", ph.FileSizeBytes, n)
		note = &msg
		p.log.Warn("file changed since scan",
			"photo_id", ph.ID, "path", ph.RelativePath, "was", ph.FileSizeBytes, "now", n)
	}

	return p.store.SetPhotoHash(ctx, ph.ID, sum, note)
}

// HashResult reports what a hashing pass did.
type HashResult struct {
	LibraryID   string `json:"library_id"`
	Hashed      int    `json:"hashed"`
	Failed      int    `json:"failed"`
	DurationMS  int64  `json:"duration_ms"`
	Interrupted bool   `json:"interrupted"`

	// Populated by the group-building stage that follows hashing.
	Groups           int   `json:"duplicate_groups"`
	ReclaimableBytes int64 `json:"reclaimable_bytes"`
}

// HashLibrary computes hashes for every photo that needs one, then rebuilds the
// library's duplicate groups.
//
// The two halves are deliberately different shapes, and this is the split
// described in ARCHITECTURE.md:
//
//   - Hashing is PER-PHOTO: embarrassingly parallel, each write touches only
//     its own row, so it runs under the same bounded-concurrency pool as
//     metadata extraction.
//   - Grouping is an AGGREGATE: it needs a global view of every hash in the
//     library, so it runs once, in one transaction, after the hashing settles.
//
// Sharding the aggregate would mean merging partial groups across workers --
// more coordination than a couple of indexed scans deserve.
func (p *Processor) HashLibrary(ctx context.Context, lib *photos.Library) (*HashResult, error) {
	start := time.Now()
	result := &HashResult{LibraryID: lib.ID}

	sem := make(chan struct{}, p.concurrency)
	var mu sync.Mutex
	var wg sync.WaitGroup
	var firstErr error

	for {
		if ctx.Err() != nil {
			result.Interrupted = true
			break
		}

		batch, err := p.store.ListPhotosNeedingHash(ctx, lib.ID, metadataBatchSize)
		if err != nil {
			if ctx.Err() != nil {
				result.Interrupted = true
				break
			}
			return result, err
		}
		if len(batch) == 0 {
			break
		}

		for _, ph := range batch {
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				result.Interrupted = true
			}
			if ctx.Err() != nil {
				break
			}

			wg.Add(1)
			go func(ph store.PhotoNeedingHash) {
				defer wg.Done()
				defer func() { <-sem }()

				err := p.HashOne(ctx, lib.RootPath, ph)

				mu.Lock()
				defer mu.Unlock()
				if err != nil {
					if ctx.Err() == nil {
						result.Failed++
						if firstErr == nil {
							firstErr = err
						}
						p.log.Warn("hashing failed",
							"photo_id", ph.ID, "path", ph.RelativePath, "error", err)
					}
					return
				}
				result.Hashed++
			}(ph)
		}

		// Wait for this batch before claiming the next.
		//
		// Without this the loop re-queries while the batch is still in flight.
		// Those goroutines have not written hashed_at yet, so the SAME rows
		// come back and are processed a second time -- unbounded goroutine
		// growth and duplicated I/O, invisible in the data because the writes
		// are idempotent. Caught by asserting an exact count: 3 files hashed 6
		// times.
		//
		// The cost is that workers idle briefly at each batch boundary. With
		// batches of 200 and concurrency in the low tens that is a rounding
		// error, and correctness is not negotiable here.
		wg.Wait()

		if ctx.Err() != nil {
			result.Interrupted = true
			break
		}
	}

	wg.Wait()
	result.DurationMS = time.Since(start).Milliseconds()

	if result.Interrupted {
		// Groups are deliberately NOT rebuilt after an interrupted pass. Doing
		// so would publish groups derived from a partially-hashed library,
		// which looks authoritative but is wrong -- a file whose hash has not
		// been computed yet cannot be known to be unique. Resuming completes
		// the hashing and then builds groups over the full picture.
		return result, nil
	}

	groups, reclaimable, err := p.store.RebuildDuplicateGroups(ctx, lib.ID)
	if err != nil {
		return result, err
	}
	result.Groups = groups
	result.ReclaimableBytes = reclaimable
	result.DurationMS = time.Since(start).Milliseconds()

	if firstErr != nil && result.Hashed == 0 {
		return result, firstErr
	}
	return result, nil
}

// StartHashAsync runs HashLibrary in the background.
func (p *Processor) StartHashAsync(ctx context.Context, lib *photos.Library) error {
	key := "hash:" + lib.ID

	p.mu.Lock()
	if _, exists := p.running[key]; exists {
		p.mu.Unlock()
		return ErrProcessingInProgress
	}
	hashCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	p.running[key] = cancel
	p.mu.Unlock()

	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		defer cancel()
		defer func() {
			p.mu.Lock()
			delete(p.running, key)
			p.mu.Unlock()
		}()

		log := p.log.With("library_id", lib.ID)
		log.Info("hashing started", "concurrency", p.concurrency)

		result, err := p.HashLibrary(hashCtx, lib)
		if err != nil {
			log.Error("hashing failed", "error", err, "duration_ms", result.DurationMS)
			return
		}
		log.Info("hashing complete",
			"hashed", result.Hashed,
			"failed", result.Failed,
			"duplicate_groups", result.Groups,
			"reclaimable_bytes", result.ReclaimableBytes,
			"interrupted", result.Interrupted,
			"duration_ms", result.DurationMS)
	}()

	return nil
}

// IsHashing reports whether this process is hashing a library.
func (p *Processor) IsHashing(libraryID string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	_, ok := p.running["hash:"+libraryID]
	return ok
}
