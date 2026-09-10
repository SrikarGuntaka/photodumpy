package ingest

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/srikarguntaka/photo-organizer/internal/hashing"
	"github.com/srikarguntaka/photo-organizer/internal/photos"
	"github.com/srikarguntaka/photo-organizer/internal/store"
)

// PHashOne computes and stores the perceptual hash of one photo.
//
// As with ExtractOne and HashOne, this is the unit the COMPUTE_PERCEPTUAL_HASH
// job handler calls; the queue supplies the scheduling.
//
// Idempotent: dHash is a deterministic function of the decoded pixels, written
// as a keyed overwrite.
//
// MEMORY: unlike SHA-256, this cannot stream -- a perceptual hash needs pixels,
// and Go's image/jpeg offers no scaled decode, so a 12-megapixel JPEG briefly
// occupies ~48MB as RGBA. That is why pixel work belongs under a tighter
// concurrency bound than metadata extraction, which only reads headers.
func (p *Processor) PHashOne(ctx context.Context, libraryRoot string, ph store.PhotoNeedingPHash) error {
	fullPath := filepath.Join(libraryRoot, filepath.FromSlash(ph.RelativePath))

	f, err := os.Open(fullPath)
	if err != nil {
		if os.IsNotExist(err) {
			msg := "file no longer exists on disk"
			p.log.Info("photo missing during perceptual hashing",
				"photo_id", ph.ID, "path", ph.RelativePath)
			if err := p.store.UpdatePhotoMetadata(ctx, ph.ID, store.PhotoMetadata{
				State:     photos.StateMissing,
				LastError: &msg,
			}); err != nil {
				return err
			}
			return p.store.MarkPHashFailed(ctx, ph.ID, msg)
		}
		return fmt.Errorf("opening %s: %w", ph.RelativePath, err)
	}
	defer f.Close()

	hash, err := hashing.DHashReader(f)
	if err != nil {
		// Undecodable pixels. Permanent -- the bytes will not improve.
		//
		// Recorded as phashed_at set with phash left NULL, NOT as a zero hash.
		// A zero hash would put every undecodable photo at distance 0 from
		// every other, grouping all of them together as near-duplicates.
		msg := "cannot decode image for perceptual hash: " + err.Error()
		p.log.Warn("perceptual hash failed",
			"photo_id", ph.ID, "path", ph.RelativePath, "error", err)
		return p.store.MarkPHashFailed(ctx, ph.ID, msg)
	}

	return p.store.SetPhotoPHash(ctx, ph.ID, hash, nil)
}

// PHashLibrary computes perceptual hashes for a library, then rebuilds its
// near-duplicate groups.
//
// Same shape as HashLibrary: a per-photo parallel pass followed by a single
// aggregate stage that needs a global view.
func (p *Processor) PHashLibrary(ctx context.Context, lib *photos.Library, threshold int) (*PHashResult, error) {
	start := time.Now()
	result := &PHashResult{LibraryID: lib.ID, Threshold: threshold}

	// Pixel work is bounded more tightly than header work: decoding a
	// 12-megapixel JPEG costs ~48MB of RGBA, so running NumCPU of them at once
	// on a 22-core machine would reserve a gigabyte. Metadata extraction reads
	// a few hundred bytes per file and has no such constraint.
	concurrency := p.concurrency
	if concurrency > maxPixelConcurrency {
		concurrency = maxPixelConcurrency
	}

	res := runPass(ctx, concurrency, metadataBatchSize,
		func(ctx context.Context, limit int) ([]store.PhotoNeedingPHash, error) {
			return p.store.ListPhotosNeedingPHash(ctx, lib.ID, limit)
		},
		func(ctx context.Context, ph store.PhotoNeedingPHash) error {
			return p.PHashOne(ctx, lib.RootPath, ph)
		},
		func(ph store.PhotoNeedingPHash, err error) {
			p.log.Warn("perceptual hashing failed",
				"photo_id", ph.ID, "path", ph.RelativePath, "error", err)
		})

	result.Hashed, result.Failed, result.Interrupted = res.Done, res.Failed, res.Interrupted
	result.DurationMS = time.Since(start).Milliseconds()

	if res.Interrupted {
		// Groups are deliberately NOT rebuilt from a partially-hashed library:
		// a photo without a hash cannot be known to be unique, so the groups
		// would look authoritative while being wrong.
		return result, nil
	}
	if res.FirstErr != nil && res.Done == 0 {
		return result, res.FirstErr
	}

	groups, reclaimable, err := p.store.RebuildSimilarGroups(ctx, lib.ID, threshold)
	if err != nil {
		return result, err
	}
	result.Groups = groups
	result.ReclaimableBytes = reclaimable
	result.DurationMS = time.Since(start).Milliseconds()
	return result, nil
}

// PHashResult reports what a perceptual hashing pass did.
type PHashResult struct {
	LibraryID        string `json:"library_id"`
	Threshold        int    `json:"threshold"`
	Hashed           int    `json:"hashed"`
	Failed           int    `json:"failed"`
	Groups           int    `json:"similar_groups"`
	ReclaimableBytes int64  `json:"reclaimable_bytes"`
	DurationMS       int64  `json:"duration_ms"`
	Interrupted      bool   `json:"interrupted"`
}

// maxPixelConcurrency caps how many images are decoded simultaneously.
//
// A perceptual hash needs pixels, and a 12-megapixel JPEG is ~48MB as RGBA.
// On a 22-core machine NumCPU concurrency would hold a gigabyte of decoded
// images. Header-only work (metadata extraction) has no such ceiling and keeps
// the full NumCPU bound.
const maxPixelConcurrency = 6
