package api

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/srikarguntaka/photo-organizer/internal/photos"
	"github.com/srikarguntaka/photo-organizer/internal/store"
	"github.com/srikarguntaka/photo-organizer/internal/thumbnail"
)

// Media endpoints serve image BYTES, which makes them the part of the API that
// touches the filesystem on a client's behalf. Every request names a photo by
// id and nothing else. No endpoint accepts a path, so there is no user-supplied
// string that can be steered at a file outside the library -- the path is
// always reconstructed server-side from database rows, and then still checked.

// servableTypes maps a detected format to the Content-Type it is served as.
//
// An allow-list, not a lookup of whatever the file claims to be. The format
// came from decoding the file's header, but a file served with a type the
// browser renders as a document -- text/html, image/svg+xml -- can execute
// script in this origin. Only raster formats with no scripting model are
// listed; anything else is served as a download, never rendered.
var servableTypes = map[photos.Format]string{
	photos.FormatJPEG: "image/jpeg",
	photos.FormatPNG:  "image/png",
	photos.FormatWebP: "image/webp",
}

// setImageSafetyHeaders is applied to every byte-serving response.
//
//   - nosniff stops a browser from second-guessing the Content-Type and
//     rendering a mislabelled file as HTML.
//   - The CSP forbids the response from loading or running anything, and
//     sandbox strips it of its origin if it is ever opened as a document. An
//     image needs none of that, so granting none of it costs nothing.
//   - private caching: this is one user's photo library. A shared proxy cache
//     has no business storing it.
func setImageSafetyHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Security-Policy", "default-src 'none'; sandbox")
	h.Set("Cache-Control", "private, max-age=86400")
}

// handleGetThumbnail serves a photo's generated thumbnail.
func (s *Server) handleGetThumbnail(w http.ResponseWriter, r *http.Request) {
	log := loggerFrom(r.Context(), s.log)

	// Lowercased because UUIDs are case-insensitive on input, and Postgres
	// treats them so: without this, an uppercase id served the original but
	// 404'd the thumbnail for the same photo. Safe to do before validation --
	// changing letter case cannot introduce a separator or a dot, and PathFor
	// still validates the result.
	id := strings.ToLower(r.PathValue("id"))

	// PathFor rejects anything that is not a canonical UUID, which is also the
	// complete traversal defence: see its doc comment. A malformed id cannot
	// name a photo, so it is a 404 rather than a 400.
	path, err := thumbnail.PathFor(s.cfg.ThumbnailDir, id)
	if err != nil {
		writeError(w, log, http.StatusNotFound, "not_found", "thumbnail not found")
		return
	}

	// The database is consulted even though the path is already known, for two
	// reasons. A thumbnail file can outlive its photo row -- a deleted library
	// leaves its thumbnails on disk -- and must not keep being served. And the
	// generation time is what makes the ETag change when a thumbnail is
	// regenerated.
	generatedAt, err := s.store.GetThumbnailGeneratedAt(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, log, http.StatusNotFound, "not_found", "thumbnail not found")
			return
		}
		log.Error("looking up thumbnail", "error", err, "photo_id", id)
		writeError(w, log, http.StatusInternalServerError, "internal", "failed to load thumbnail")
		return
	}

	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			// The row says a thumbnail exists but the file does not: the volume
			// was wiped, or this API is pointed at a different directory from
			// the workers. Worth a warning -- it will not fix itself -- but to
			// the client it is simply missing.
			log.Warn("thumbnail recorded but missing on disk", "photo_id", id)
			writeError(w, log, http.StatusNotFound, "not_found", "thumbnail not found")
			return
		}
		log.Error("opening thumbnail", "error", err, "photo_id", id)
		writeError(w, log, http.StatusInternalServerError, "internal", "failed to load thumbnail")
		return
	}
	defer f.Close()

	setImageSafetyHeaders(w)
	w.Header().Set("Content-Type", "image/jpeg")
	// A strong validator: the photo id and the instant the file was written
	// uniquely identify its bytes. ServeContent honours If-None-Match against
	// it, so a revisited grid costs a 304 per tile rather than a re-download.
	w.Header().Set("ETag", fmt.Sprintf(`"%s-%d"`, id, generatedAt.UnixNano()))

	http.ServeContent(w, r, "", generatedAt, f)
}

// handleGetOriginal serves a photo's original file, read-only.
//
// For the detail view's full-size image. It re-derives the path from the
// database and re-checks containment on every request, rather than trusting
// that the path was safe when the photo was scanned: the library root is
// configuration, and configuration changes.
func (s *Server) handleGetOriginal(w http.ResponseWriter, r *http.Request) {
	log := loggerFrom(r.Context(), s.log)
	id := r.PathValue("id")

	view, err := s.store.GetPhotoView(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) || isInvalidUUID(err) {
			writeError(w, log, http.StatusNotFound, "not_found", "photo not found")
			return
		}
		log.Error("loading photo", "error", err, "photo_id", id)
		writeError(w, log, http.StatusInternalServerError, "internal", "failed to load photo")
		return
	}

	lib, err := s.store.GetLibrary(r.Context(), view.LibraryID)
	if err != nil {
		log.Error("loading library for original", "error", err, "photo_id", id)
		writeError(w, log, http.StatusInternalServerError, "internal", "failed to load photo")
		return
	}

	// Two containment checks, each guarding a different layer.
	//
	// 1. The library root must still sit inside PHOTO_ROOT. It did when the
	//    library was created, but PHOTO_ROOT is an environment variable that
	//    may since have been narrowed.
	root, err := photos.ResolveWithin(s.cfg.PhotoRoot, lib.RootPath)
	if err != nil {
		log.Warn("library root is outside PHOTO_ROOT; refusing to serve",
			"library_id", lib.ID, "photo_id", id)
		writeError(w, log, http.StatusNotFound, "not_found", "photo not found")
		return
	}

	// 2. The photo must resolve inside that root. relative_path came from a
	//    directory walk, not a request, but a symlink created inside the
	//    library after the scan could now point anywhere. ResolveWithin
	//    evaluates symlinks before comparing, which is what catches that.
	full, err := photos.ResolveWithin(root, filepath.FromSlash(view.RelativePath))
	if err != nil {
		if errors.Is(err, photos.ErrOutsideRoot) {
			log.Warn("photo resolves outside its library; refusing to serve", "photo_id", id)
		}
		// Not echoed: on a traversal the error would confirm what exists.
		writeError(w, log, http.StatusNotFound, "not_found", "photo not found")
		return
	}

	// Opened read-only. The library is mounted :ro in Docker, so this is
	// defence in depth rather than the only guard, but os.Open never requests
	// write access regardless of the mount.
	f, err := os.Open(full)
	if err != nil {
		writeError(w, log, http.StatusNotFound, "not_found", "photo not found")
		return
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		// A directory, device or FIFO where a photo used to be. Serving a
		// FIFO would block the handler until the write timeout.
		writeError(w, log, http.StatusNotFound, "not_found", "photo not found")
		return
	}

	setImageSafetyHeaders(w)
	if ct, ok := servableTypes[photos.Format(view.DetectedFormat)]; ok {
		w.Header().Set("Content-Type", ct)
	} else {
		// Not a format the browser should render. Force a download so it is
		// never interpreted as a document in this origin.
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", "attachment")
	}

	// No filename argument. Content-Type is always set above, so ServeContent
	// will not replace it -- but passing the real name would make extension
	// inference the fallback if a future edit ever dropped that header, and a
	// photo named "x.html" must never be served as HTML.
	http.ServeContent(w, r, "", info.ModTime().Truncate(time.Second), f)
}
