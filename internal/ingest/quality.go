package ingest

import (
	"context"
	"fmt"
	"image"
	"os"
	"path/filepath"
	"time"

	"github.com/srikarguntaka/photo-organizer/internal/photos"
	"github.com/srikarguntaka/photo-organizer/internal/quality"
	"github.com/srikarguntaka/photo-organizer/internal/store"

	// Decoders for image.Decode.
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"

	_ "golang.org/x/image/webp"
)

// AnalyzeOne measures and stores one photo's quality metrics.
//
// The unit the ANALYZE_QUALITY handler calls. Idempotent: a deterministic
// function of the decoded pixels, written as a keyed overwrite.
func (p *Processor) AnalyzeOne(ctx context.Context, libraryRoot string, ph store.PhotoNeedingQuality) error {
	fullPath := filepath.Join(libraryRoot, filepath.FromSlash(ph.RelativePath))

	f, err := os.Open(fullPath)
	if err != nil {
		if os.IsNotExist(err) {
			msg := "file no longer exists on disk"
			p.log.Info("photo missing during quality analysis",
				"photo_id", ph.ID, "path", ph.RelativePath)
			if err := p.store.UpdatePhotoMetadata(ctx, ph.ID, store.PhotoMetadata{
				State:     photos.StateMissing,
				LastError: &msg,
			}); err != nil {
				return err
			}
			return p.store.MarkQualityFailed(ctx, ph.ID, msg)
		}
		return fmt.Errorf("opening %s: %w", ph.RelativePath, err)
	}
	defer f.Close()

	img, _, err := image.Decode(f)
	if err != nil {
		// Undecodable pixels. Permanent, and recorded WITHOUT scores rather
		// than as zeroes -- a fabricated 0.0 would sort to the top of a
		// "worst photos" list, presenting an unmeasured photo as the most
		// defective one in the library.
		msg := "cannot decode image for quality analysis: " + err.Error()
		p.log.Warn("quality analysis failed",
			"photo_id", ph.ID, "path", ph.RelativePath, "error", err)
		return p.store.MarkQualityFailed(ctx, ph.ID, msg)
	}

	return p.store.SetPhotoQuality(ctx, ph.ID, quality.Analyze(img, p.thresholds))
}

// QualityResult reports what an analysis pass did.
type QualityResult struct {
	LibraryID   string `json:"library_id"`
	Analyzed    int    `json:"analyzed"`
	Failed      int    `json:"failed"`
	DurationMS  int64  `json:"duration_ms"`
	Interrupted bool   `json:"interrupted"`
}

// AnalyzeLibrary measures quality for every photo that needs it.
//
// Purely per-photo: unlike hashing, there is no aggregate stage. Quality is a
// property of one image, so nothing needs a global view.
func (p *Processor) AnalyzeLibrary(ctx context.Context, lib *photos.Library) (*QualityResult, error) {
	start := time.Now()
	result := &QualityResult{LibraryID: lib.ID}

	// Pixel work, so the tighter bound applies: quality analysis decodes the
	// full image before downsampling for measurement.
	concurrency := p.concurrency
	if concurrency > maxPixelConcurrency {
		concurrency = maxPixelConcurrency
	}

	res := runPass(ctx, concurrency, metadataBatchSize,
		func(ctx context.Context, limit int) ([]store.PhotoNeedingQuality, error) {
			return p.store.ListPhotosNeedingQuality(ctx, lib.ID, limit)
		},
		func(ctx context.Context, ph store.PhotoNeedingQuality) error {
			return p.AnalyzeOne(ctx, lib.RootPath, ph)
		},
		func(ph store.PhotoNeedingQuality, err error) {
			p.log.Warn("quality analysis failed",
				"photo_id", ph.ID, "path", ph.RelativePath, "error", err)
		})

	result.Analyzed, result.Failed, result.Interrupted = res.Done, res.Failed, res.Interrupted
	result.DurationMS = time.Since(start).Milliseconds()

	if res.Interrupted {
		return result, nil
	}
	if res.FirstErr != nil && res.Done == 0 {
		return result, res.FirstErr
	}
	return result, nil
}
