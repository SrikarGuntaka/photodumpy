package ingest

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/srikarguntaka/photo-organizer/internal/photos"
	"github.com/srikarguntaka/photo-organizer/internal/store"
	"github.com/srikarguntaka/photo-organizer/internal/thumbnail"
)

// ErrNoThumbnailDir is returned when thumbnail generation is requested from a
// Processor that was not given a directory to write to.
var ErrNoThumbnailDir = errors.New("ingest: no thumbnail directory configured")

// ThumbnailOne renders and stores one photo's thumbnail.
//
// The unit the GENERATE_THUMBNAIL handler calls. Idempotent: rendering is
// deterministic and the write is an atomic overwrite keyed by photo id.
//
// ORIENTATION IS READ HERE, from the file already open, rather than taken from
// the photos row. The row's orientation is written by EXTRACT_METADATA, and
// depending on it would mean either deferring until that job finished -- and
// deferring forever if it died -- or rendering sideways thumbnails whenever
// this job happened to run first, then marking them done so nothing revisited
// them. Re-reading a few KB of EXIF costs far less than that coordination, and
// makes this job's output a function of the file alone.
func (p *Processor) ThumbnailOne(ctx context.Context, libraryRoot string, ph store.PhotoNeedingThumbnail) error {
	if p.thumbnailDir == "" {
		return ErrNoThumbnailDir
	}

	fullPath := filepath.Join(libraryRoot, filepath.FromSlash(ph.RelativePath))

	f, err := os.Open(fullPath)
	if err != nil {
		if os.IsNotExist(err) {
			msg := "file no longer exists on disk"
			p.log.Info("photo missing during thumbnail generation",
				"photo_id", ph.ID, "path", ph.RelativePath)
			if err := p.store.UpdatePhotoMetadata(ctx, ph.ID, store.PhotoMetadata{
				State:     photos.StateMissing,
				LastError: &msg,
			}); err != nil {
				return err
			}
			return p.store.MarkThumbnailFailed(ctx, ph.ID, msg)
		}
		// Anything else -- permissions, a flaky network mount -- may be
		// transient, so it goes back to the queue to retry.
		return fmt.Errorf("opening %s: %w", ph.RelativePath, err)
	}
	defer f.Close()

	rendered, err := thumbnail.FromReader(f, thumbnail.MaxEdge)
	if err != nil {
		if !errors.Is(err, thumbnail.ErrDecode) {
			// Seek or read trouble: possibly transient, so retry.
			return fmt.Errorf("reading %s: %w", ph.RelativePath, err)
		}
		// Undecodable pixels. Permanent: the bytes will not improve on retry.
		// Recorded with NULL dimensions rather than a placeholder, so the UI
		// can show "no preview" instead of a thumbnail that looks real.
		p.log.Warn("thumbnail generation failed",
			"photo_id", ph.ID, "path", ph.RelativePath, "error", err)
		return p.store.MarkThumbnailFailed(ctx, ph.ID, err.Error())
	}

	written, err := thumbnail.WriteAtomic(p.thumbnailDir, ph.ID, rendered)
	if err != nil {
		// A write failure is about THIS machine's disk, not the photo, so it
		// is retryable -- a full volume may be freed.
		return fmt.Errorf("writing thumbnail for %s: %w", ph.ID, err)
	}

	return p.store.SetPhotoThumbnail(ctx, ph.ID, written.Width, written.Height, written.Bytes)
}
