package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/srikarguntaka/photo-organizer/internal/clustering"
	"github.com/srikarguntaka/photo-organizer/internal/hashing"
	"github.com/srikarguntaka/photo-organizer/internal/ingest"
	"github.com/srikarguntaka/photo-organizer/internal/jobs"
	"github.com/srikarguntaka/photo-organizer/internal/metadata"
	"github.com/srikarguntaka/photo-organizer/internal/store"
)

// Handlers wires job types to the work they perform.
//
// Every handler here is a thin adapter: it resolves the job's target, calls a
// function that already existed and was already idempotent, and translates the
// error. That was the point of building ExtractOne and HashOne as standalone
// methods in Phases 3 and 4 -- moving the work under the queue required no
// changes to the work itself.
type Handlers struct {
	store     *store.Store
	processor *ingest.Processor
	log       *slog.Logger

	// similarityThreshold is the Hamming distance below which two photos are
	// considered near-duplicates. Configurable because it is a judgement call,
	// not a constant of nature -- see hashing.DefaultSimilarityThreshold for
	// the calibration behind the default.
	similarityThreshold int

	// clustering holds the time and distance thresholds that decide where one
	// event ends and the next begins. Judgement calls for the same reason.
	clustering clustering.Options
}

// Tuning collects the thresholds the handlers need.
//
// A struct rather than positional parameters because every field is a
// judgement call that may be revised, and a growing list of bare ints at a
// call site is how the wrong one ends up in the wrong slot.
type Tuning struct {
	SimilarityThreshold int
	MaxGap              time.Duration
	MaxRadiusMeters     float64
}

func NewHandlers(st *store.Store, processor *ingest.Processor, log *slog.Logger, t Tuning) *Handlers {
	threshold := t.SimilarityThreshold
	if threshold <= 0 {
		threshold = hashing.DefaultSimilarityThreshold
	}
	if threshold > hashing.MaxSimilarityThreshold {
		threshold = hashing.MaxSimilarityThreshold
	}

	// Resolved once, here, rather than left as zeroes for something downstream
	// to default. The handler logs these values and the store records them on
	// every cluster row as "the thresholds this was built with", and both of
	// those are lies if the struct still holds an unset zero.
	opts := clustering.Options{
		MaxGap:          t.MaxGap,
		MaxRadiusMeters: t.MaxRadiusMeters,
	}.WithDefaults()

	return &Handlers{
		store:               st,
		processor:           processor,
		log:                 log,
		similarityThreshold: threshold,
		clustering:          opts,
	}
}

// RegisterAll attaches every handler to a worker.
func (h *Handlers) RegisterAll(w *Worker) {
	w.Register(jobs.TypeExtractMetadata, h.ExtractMetadata)
	w.Register(jobs.TypeComputeFileHash, h.ComputeFileHash)
	w.Register(jobs.TypeComputePerceptualHash, h.ComputePerceptualHash)
	w.Register(jobs.TypeAnalyzeQuality, h.AnalyzeQuality)
	w.Register(jobs.TypeGenerateThumbnail, h.GenerateThumbnail)
	w.Register(jobs.TypeBuildDuplicateGroups, h.BuildDuplicateGroups)
	w.Register(jobs.TypeBuildSimilarGroups, h.BuildSimilarGroups)
	w.Register(jobs.TypeBuildClusters, h.BuildClusters)
}

// BuildClusters handles the BUILD_CLUSTERS aggregate stage.
func (h *Handlers) BuildClusters(ctx context.Context, job jobs.Job) error {
	// One prerequisite: clustering reads captured_at and GPS, both of which
	// EXTRACT_METADATA writes. Running first would cluster a library in which
	// every photo still looked undated, produce a single empty-ish result, and
	// never revisit it -- the aggregate runs once.
	//
	// Deferring reschedules WITHOUT consuming an attempt, so waiting for a
	// slow metadata pass cannot exhaust the retry budget and kill the stage.
	outstanding, err := h.store.CountOutstandingJobsOfType(ctx, job.LibraryID, jobs.TypeExtractMetadata)
	if err != nil {
		return fmt.Errorf("checking %s progress: %w", jobs.TypeExtractMetadata, err)
	}
	if outstanding > 0 {
		return jobs.Defer(2*time.Second,
			fmt.Sprintf("%d %s jobs still outstanding", outstanding, jobs.TypeExtractMetadata))
	}

	clusters, undated, err := h.store.RebuildClusters(ctx, job.LibraryID, h.clustering)
	if err != nil {
		return fmt.Errorf("rebuilding clusters: %w", err)
	}
	h.log.Info("rebuilt clusters",
		"library_id", job.LibraryID, "clusters", clusters, "undated_photos", undated,
		"max_gap", h.clustering.MaxGap, "max_radius_meters", h.clustering.MaxRadiusMeters)
	return nil
}

// ExtractMetadata handles one EXTRACT_METADATA job.
func (h *Handlers) ExtractMetadata(ctx context.Context, job jobs.Job) error {
	lib, err := h.store.GetLibrary(ctx, job.LibraryID)
	if err != nil {
		return h.libraryError(err)
	}

	photo, err := h.store.GetPhoto(ctx, job.TargetID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// The photo row is gone -- the library was deleted, or a rescan
			// removed it. Nothing to do, and retrying will not bring it back.
			return jobs.Permanent(fmt.Errorf("photo %s no longer exists", job.TargetID))
		}
		return fmt.Errorf("loading photo: %w", err)
	}

	return h.processor.ExtractOne(ctx, lib.RootPath, store.PhotoNeedingMetadata{
		ID:             photo.ID,
		RelativePath:   photo.RelativePath,
		FileModifiedAt: photo.FileModifiedAt,
	})
}

// ComputeFileHash handles one COMPUTE_FILE_HASH job.
func (h *Handlers) ComputeFileHash(ctx context.Context, job jobs.Job) error {
	lib, err := h.store.GetLibrary(ctx, job.LibraryID)
	if err != nil {
		return h.libraryError(err)
	}

	photo, err := h.store.GetPhoto(ctx, job.TargetID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return jobs.Permanent(fmt.Errorf("photo %s no longer exists", job.TargetID))
		}
		return fmt.Errorf("loading photo: %w", err)
	}

	return h.processor.HashOne(ctx, lib.RootPath, store.PhotoNeedingHash{
		ID:            photo.ID,
		RelativePath:  photo.RelativePath,
		FileSizeBytes: photo.FileSizeBytes,
	})
}

// ComputePerceptualHash handles one COMPUTE_PERCEPTUAL_HASH job.
func (h *Handlers) ComputePerceptualHash(ctx context.Context, job jobs.Job) error {
	lib, err := h.store.GetLibrary(ctx, job.LibraryID)
	if err != nil {
		return h.libraryError(err)
	}

	photo, err := h.store.GetPhoto(ctx, job.TargetID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return jobs.Permanent(fmt.Errorf("photo %s no longer exists", job.TargetID))
		}
		return fmt.Errorf("loading photo: %w", err)
	}

	return h.processor.PHashOne(ctx, lib.RootPath, store.PhotoNeedingPHash{
		ID:           photo.ID,
		RelativePath: photo.RelativePath,
	})
}

// AnalyzeQuality handles one ANALYZE_QUALITY job.
func (h *Handlers) AnalyzeQuality(ctx context.Context, job jobs.Job) error {
	lib, err := h.store.GetLibrary(ctx, job.LibraryID)
	if err != nil {
		return h.libraryError(err)
	}

	photo, err := h.store.GetPhoto(ctx, job.TargetID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return jobs.Permanent(fmt.Errorf("photo %s no longer exists", job.TargetID))
		}
		return fmt.Errorf("loading photo: %w", err)
	}

	return h.processor.AnalyzeOne(ctx, lib.RootPath, store.PhotoNeedingQuality{
		ID:           photo.ID,
		RelativePath: photo.RelativePath,
	})
}

// GenerateThumbnail handles one GENERATE_THUMBNAIL job.
//
// No prerequisite, unlike the aggregate stages: the thumbnail pass reads
// orientation from the file itself, so it can run in any order relative to
// metadata extraction. See Processor.ThumbnailOne.
func (h *Handlers) GenerateThumbnail(ctx context.Context, job jobs.Job) error {
	lib, err := h.store.GetLibrary(ctx, job.LibraryID)
	if err != nil {
		return h.libraryError(err)
	}

	photo, err := h.store.GetPhoto(ctx, job.TargetID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return jobs.Permanent(fmt.Errorf("photo %s no longer exists", job.TargetID))
		}
		return fmt.Errorf("loading photo: %w", err)
	}

	err = h.processor.ThumbnailOne(ctx, lib.RootPath, store.PhotoNeedingThumbnail{
		ID:           photo.ID,
		RelativePath: photo.RelativePath,
	})
	// A worker started without a thumbnail directory is misconfigured, and no
	// number of retries will fix that. Burning five attempts per photo would
	// bury the one real message under thousands of identical failures.
	if errors.Is(err, ingest.ErrNoThumbnailDir) {
		return jobs.Permanent(err)
	}
	return err
}

// BuildSimilarGroups handles the near-duplicate aggregate stage.
//
// Same prerequisite discipline as BuildDuplicateGroups: it refuses to run
// while the perceptual hashing it depends on is still outstanding, because
// grouping over a partially-hashed library produces a confident wrong answer.
func (h *Handlers) BuildSimilarGroups(ctx context.Context, job jobs.Job) error {
	// Two prerequisites, not one. Perceptual hashes decide MEMBERSHIP; quality
	// scores decide which member to suggest keeping. Running before quality
	// lands would produce correct groups with a worse recommendation, and
	// nothing would revisit it.
	for _, prereq := range []jobs.Type{jobs.TypeComputePerceptualHash, jobs.TypeAnalyzeQuality} {
		outstanding, err := h.store.CountOutstandingJobsOfType(ctx, job.LibraryID, prereq)
		if err != nil {
			return fmt.Errorf("checking %s progress: %w", prereq, err)
		}
		if outstanding > 0 {
			return jobs.Defer(2*time.Second,
				fmt.Sprintf("%d %s jobs still outstanding", outstanding, prereq))
		}
	}

	groups, reclaimable, err := h.store.RebuildSimilarGroups(ctx, job.LibraryID, h.similarityThreshold)
	if err != nil {
		return fmt.Errorf("rebuilding similar groups: %w", err)
	}
	h.log.Info("rebuilt similar groups",
		"library_id", job.LibraryID, "groups", groups,
		"threshold", h.similarityThreshold, "reclaimable_bytes", reclaimable)
	return nil
}

// BuildDuplicateGroups handles the aggregate stage.
//
// This is the library-scoped job: one per library rather than one per photo,
// because it needs a global view of every hash. It runs under exactly the same
// lease, retry and crash-recovery machinery as the per-photo jobs -- the only
// difference is target_type and a lower priority.
//
// PREREQUISITE CHECK. Priority orders the claim, not the execution: a worker
// claiming a batch of 22 gets per-photo jobs and this aggregate together and
// runs them concurrently. Observed in testing -- the aggregate started 33ms
// before the last hash finished and reported 6 duplicate files where the true
// answer was 9. A wrong answer delivered confidently is worse than a late one,
// so this refuses to run until the hashing it depends on has drained.
//
// Deferring rather than failing is what makes that safe: a deferral does not
// consume an attempt, so a library that takes ten minutes to hash cannot
// exhaust this job's retries while waiting.
//
// Re-checking on every run, rather than encoding a static dependency graph,
// keeps it correct when jobs are added, retried, or reclaimed from a dead
// worker while the aggregate waits.
func (h *Handlers) BuildDuplicateGroups(ctx context.Context, job jobs.Job) error {
	outstanding, err := h.store.CountOutstandingJobsOfType(ctx, job.LibraryID, jobs.TypeComputeFileHash)
	if err != nil {
		return fmt.Errorf("checking hashing progress: %w", err)
	}
	if outstanding > 0 {
		return jobs.Defer(2*time.Second,
			fmt.Sprintf("%d COMPUTE_FILE_HASH jobs still outstanding", outstanding))
	}

	groups, reclaimable, err := h.store.RebuildDuplicateGroups(ctx, job.LibraryID)
	if err != nil {
		return fmt.Errorf("rebuilding duplicate groups: %w", err)
	}
	h.log.Info("rebuilt duplicate groups",
		"library_id", job.LibraryID, "groups", groups, "reclaimable_bytes", reclaimable)
	return nil
}

// libraryError translates a library lookup failure. A deleted library means
// every job targeting it is moot; retrying would just burn attempts.
func (h *Handlers) libraryError(err error) error {
	if errors.Is(err, store.ErrNotFound) {
		return jobs.Permanent(errors.New("library no longer exists"))
	}
	return fmt.Errorf("loading library: %w", err)
}

// IsPermanentMediaError reports whether an error from the media pipeline should
// skip retries. Exposed so the ingest package's error taxonomy stays the single
// source of truth rather than being duplicated here.
func IsPermanentMediaError(err error) bool {
	return errors.Is(err, metadata.ErrNotAnImage)
}
