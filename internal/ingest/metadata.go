package ingest

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"github.com/srikarguntaka/photo-organizer/internal/metadata"
	"github.com/srikarguntaka/photo-organizer/internal/photos"
	"github.com/srikarguntaka/photo-organizer/internal/quality"
	"github.com/srikarguntaka/photo-organizer/internal/store"
)

// ExtractOne is the unit of metadata work: read one photo, persist what was
// found. Everything else in this file is scheduling around it.
//
// It is deliberately a standalone function on Processor rather than being
// inlined into the loop, because Phase 5's EXTRACT_METADATA job handler calls
// exactly this. The job queue then supplies the scheduling (leases, retries,
// crash recovery) that ProcessLibrary does locally, and no extraction logic
// moves or changes.
//
// Idempotent: a deterministic function of the file's bytes, written as a
// keyed overwrite. Running it twice produces identical rows, which is what
// makes at-least-once job delivery safe.
func (p *Processor) ExtractOne(ctx context.Context, libraryRoot string, photo store.PhotoNeedingMetadata) error {
	fullPath := filepath.Join(libraryRoot, filepath.FromSlash(photo.RelativePath))

	var fallback time.Time
	if photo.FileModifiedAt != nil {
		fallback = *photo.FileModifiedAt
	}

	f, err := os.Open(fullPath)
	if err != nil {
		return p.recordOpenFailure(ctx, photo, err)
	}
	defer f.Close()

	// Re-stat rather than trusting the row: the scan may have run days ago and
	// the file could have been replaced. Cheap, and it catches the case where
	// mtime moved since discovery.
	if info, err := f.Stat(); err == nil && !info.ModTime().IsZero() {
		fallback = info.ModTime()
	}

	md, err := metadata.Extract(f, fallback)
	if err != nil {
		// Not a decodable image. Permanent -- retrying will not change the
		// bytes -- so the photo is marked failed and extraction is recorded as
		// done rather than left pending forever.
		if errors.Is(err, metadata.ErrNotAnImage) {
			msg := err.Error()
			return p.store.UpdatePhotoMetadata(ctx, photo.ID, store.PhotoMetadata{
				State:     photos.StateFailed,
				LastError: &msg,
			})
		}
		// Anything else (a read error mid-file) is potentially transient and
		// is returned so the caller can decide. In Phase 5 that becomes a job
		// retry with backoff.
		return fmt.Errorf("extracting %s: %w", photo.RelativePath, err)
	}

	update := store.PhotoMetadata{State: photos.StateProcessing}

	if md.Width > 0 {
		update.Width, update.Height = &md.Width, &md.Height
	}
	if md.Orientation > 0 {
		update.Orientation = &md.Orientation
	}
	if md.Format != "" {
		update.DetectedFormat = &md.Format
	}
	if md.CapturedAt != nil {
		update.CapturedAt = md.CapturedAt
		src := string(md.CapturedAtSource)
		update.CapturedAtSource = &src
	}
	update.Latitude, update.Longitude = md.Latitude, md.Longitude
	update.CameraMake, update.CameraModel = md.CameraMake, md.CameraModel

	if len(md.Warnings) > 0 {
		// Warnings are kept on the row rather than only logged, so "why does
		// this photo have no date" is answerable from the database later.
		msg := "metadata warnings: " + joinWarnings(md.Warnings)
		update.LastError = &msg
		p.log.Debug("metadata warnings",
			"photo_id", photo.ID, "path", photo.RelativePath, "warnings", md.Warnings)
	}

	return p.store.UpdatePhotoMetadata(ctx, photo.ID, update)
}

// recordOpenFailure distinguishes a file that has gone away from one we simply
// cannot read.
//
// A deleted file is NOT a retryable error. Retrying it five times with
// exponential backoff is pure waste, and the correct end state is a photo
// marked 'missing' rather than 'failed'. A permission error, by contrast, may
// genuinely resolve, so it is returned for retry.
func (p *Processor) recordOpenFailure(ctx context.Context, photo store.PhotoNeedingMetadata, openErr error) error {
	if os.IsNotExist(openErr) {
		msg := "file no longer exists on disk"
		p.log.Info("photo missing", "photo_id", photo.ID, "path", photo.RelativePath)
		return p.store.UpdatePhotoMetadata(ctx, photo.ID, store.PhotoMetadata{
			State:     photos.StateMissing,
			LastError: &msg,
		})
	}
	return fmt.Errorf("opening %s: %w", photo.RelativePath, openErr)
}

func joinWarnings(w []string) string {
	out := ""
	for i, s := range w {
		if i > 0 {
			out += "; "
		}
		out += s
	}
	return out
}

// Processor runs metadata extraction across a library.
//
// In Phase 5 this scheduling is replaced by the job queue -- the queue supplies
// leases, retries and crash recovery, and calls ExtractOne. Until then this
// provides the same bounded concurrency locally so the extraction code is
// exercised and testable now rather than waiting two phases.
type Processor struct {
	store *store.Store
	log   *slog.Logger

	// concurrency bounds how many files are open and being decoded at once.
	concurrency int

	// thresholds are the quality cut-offs. Configuration rather than
	// constants, because every one of them is a judgement call.
	thresholds quality.Thresholds

	// thumbnailDir is where thumbnails are written. Empty in processes that
	// never generate them -- the API, which only reads.
	thumbnailDir string

	mu      sync.Mutex
	running map[string]context.CancelFunc
	wg      sync.WaitGroup
}

// ProcessorOptions configures a Processor.
type ProcessorOptions struct {
	// Concurrency is the number of photos processed simultaneously. Zero means
	// runtime.NumCPU().
	Concurrency int

	// QualityThresholds override the calibrated defaults. The zero value uses
	// quality.DefaultThresholds().
	QualityThresholds *quality.Thresholds

	// ThumbnailDir is the directory thumbnails are written to. It must not be
	// inside any photo library.
	ThumbnailDir string
}

func NewProcessor(st *store.Store, log *slog.Logger, opts ProcessorOptions) *Processor {
	c := opts.Concurrency
	if c <= 0 {
		// Metadata extraction reads a few KB per file and does no pixel
		// decoding, so it is closer to I/O-bound than CPU-bound. NumCPU is a
		// reasonable default that does not oversubscribe either resource.
		c = runtime.NumCPU()
	}
	th := quality.DefaultThresholds()
	if opts.QualityThresholds != nil {
		th = *opts.QualityThresholds
	}

	return &Processor{
		store:        st,
		log:          log,
		concurrency:  c,
		thresholds:   th,
		thumbnailDir: opts.ThumbnailDir,
		running:      make(map[string]context.CancelFunc),
	}
}

// MetadataResult reports what a processing pass did.
type MetadataResult struct {
	LibraryID   string `json:"library_id"`
	Processed   int    `json:"processed"`
	Failed      int    `json:"failed"`
	Missing     int    `json:"missing"`
	DurationMS  int64  `json:"duration_ms"`
	Interrupted bool   `json:"interrupted"`
}

// batchSize is how many photo rows are claimed from the database per round
// trip. Larger than concurrency so workers are never idle waiting for the next
// query, small enough that a cancelled run loses little.
const metadataBatchSize = 200

// ProcessLibrary extracts metadata for every photo in a library that still
// needs it, with bounded concurrency, and blocks until done.
//
// Concurrency is bounded in two places, deliberately:
//
//  1. A buffered channel of size p.concurrency caps how many files are open and
//     being decoded simultaneously. Without it, a 100,000-photo library would
//     spawn 100,000 goroutines, exhaust file descriptors, and thrash.
//  2. Rows are claimed in batches rather than all at once, so memory is
//     independent of library size.
//
// There is never one goroutine per photo.
func (p *Processor) ProcessLibrary(ctx context.Context, lib *photos.Library) (*MetadataResult, error) {
	start := time.Now()
	result := &MetadataResult{LibraryID: lib.ID}

	// Full NumCPU concurrency: metadata extraction reads image headers, a few
	// hundred bytes per file, so it is I/O-bound rather than memory-bound.
	// Pixel work uses a tighter bound -- see maxPixelConcurrency.
	res := runPass(ctx, p.concurrency, metadataBatchSize,
		func(ctx context.Context, limit int) ([]store.PhotoNeedingMetadata, error) {
			return p.store.ListPhotosNeedingMetadata(ctx, lib.ID, limit)
		},
		func(ctx context.Context, ph store.PhotoNeedingMetadata) error {
			return p.ExtractOne(ctx, lib.RootPath, ph)
		},
		func(ph store.PhotoNeedingMetadata, err error) {
			p.log.Warn("metadata extraction failed",
				"photo_id", ph.ID, "path", ph.RelativePath, "error", err)
		})

	result.Processed, result.Failed, result.Interrupted = res.Done, res.Failed, res.Interrupted
	result.DurationMS = time.Since(start).Milliseconds()

	if res.Interrupted {
		return result, nil
	}
	if res.FirstErr != nil && res.Done == 0 {
		// Everything failed -- likely the library root is gone rather than
		// every individual file being broken.
		return result, res.FirstErr
	}
	return result, nil
}

// StartAsync runs ProcessLibrary in the background, mirroring Scanner.StartAsync
// so the API can return 202 and let the caller poll.
func (p *Processor) StartAsync(ctx context.Context, lib *photos.Library) error {
	p.mu.Lock()
	if _, exists := p.running[lib.ID]; exists {
		p.mu.Unlock()
		return ErrProcessingInProgress
	}
	procCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	p.running[lib.ID] = cancel
	p.mu.Unlock()

	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		defer cancel()
		defer func() {
			p.mu.Lock()
			delete(p.running, lib.ID)
			p.mu.Unlock()
		}()

		log := p.log.With("library_id", lib.ID)
		log.Info("metadata extraction started", "concurrency", p.concurrency)

		result, err := p.ProcessLibrary(procCtx, lib)
		if err != nil {
			log.Error("metadata extraction failed", "error", err, "duration_ms", result.DurationMS)
			return
		}
		log.Info("metadata extraction complete",
			"processed", result.Processed,
			"failed", result.Failed,
			"interrupted", result.Interrupted,
			"duration_ms", result.DurationMS)
	}()

	return nil
}

// ErrProcessingInProgress is returned when extraction is already running.
var ErrProcessingInProgress = errors.New("ingest: metadata extraction is already running for this library")

// IsProcessing reports whether this process is extracting metadata for a library.
func (p *Processor) IsProcessing(libraryID string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	_, ok := p.running[libraryID]
	return ok
}

// Shutdown cancels in-flight processing and waits for it to stop.
func (p *Processor) Shutdown(ctx context.Context) error {
	p.mu.Lock()
	for _, cancel := range p.running {
		cancel()
	}
	p.mu.Unlock()

	done := make(chan struct{})
	go func() {
		p.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("ingest: metadata extraction did not stop in time: %w", ctx.Err())
	}
}
