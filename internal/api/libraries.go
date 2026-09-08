package api

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/srikarguntaka/photo-organizer/internal/ingest"
	"github.com/srikarguntaka/photo-organizer/internal/photos"
	"github.com/srikarguntaka/photo-organizer/internal/store"
)

type createLibraryRequest struct {
	Name string `json:"name"`
	// Path may be absolute or relative to PHOTO_ROOT. It is validated to sit
	// inside PHOTO_ROOT before anything touches the filesystem.
	Path string `json:"path"`
}

type libraryResponse struct {
	photos.Library
	// ScanState is derived, not stored, so it cannot drift from the timestamps.
	ScanState string `json:"scan_state"`
	// PhotoCount is omitted from list responses (it would be a query per row).
	PhotoCount *int `json:"photo_count,omitempty"`
}

func newLibraryResponse(l *photos.Library, count *int) libraryResponse {
	return libraryResponse{Library: *l, ScanState: l.ScanState(), PhotoCount: count}
}

// handleCreateLibrary registers a folder as a library.
//
// Idempotent: posting the same path twice returns the existing library with
// 200 rather than creating a second one or erroring. 201 is returned only when
// a row was genuinely created, so a client can tell the difference.
func (s *Server) handleCreateLibrary(w http.ResponseWriter, r *http.Request) {
	log := loggerFrom(r.Context(), s.log)

	var req createLibraryRequest
	if err := decodeJSON(w, r, &req, log); err != nil {
		return
	}

	req.Name = strings.TrimSpace(req.Name)
	req.Path = strings.TrimSpace(req.Path)

	if req.Path == "" {
		writeError(w, log, http.StatusBadRequest, "invalid_request", "path is required")
		return
	}

	// THE security check. Everything downstream trusts that root_path is
	// inside PHOTO_ROOT; this is the only place that is established.
	resolved, err := photos.ResolveWithin(s.cfg.PhotoRoot, req.Path)
	if err != nil {
		if errors.Is(err, photos.ErrOutsideRoot) {
			// Deliberately does not echo the resolved path: on a traversal
			// attempt that would confirm what exists outside the root.
			log.Warn("rejected library path outside photo root", "requested", req.Path)
			writeError(w, log, http.StatusBadRequest, "path_outside_root",
				"path must be inside the configured photo root")
			return
		}
		writeError(w, log, http.StatusBadRequest, "invalid_path", err.Error())
		return
	}

	if err := requireDirectory(resolved); err != nil {
		writeError(w, log, http.StatusBadRequest, "invalid_path", err.Error())
		return
	}

	if req.Name == "" {
		req.Name = defaultLibraryName(resolved)
	}

	lib, created, err := s.store.CreateLibrary(r.Context(), req.Name, resolved, photos.SourceLocalFS)
	if err != nil {
		log.Error("creating library", "error", err)
		writeError(w, log, http.StatusInternalServerError, "internal", "failed to create library")
		return
	}

	status := http.StatusOK
	if created {
		status = http.StatusCreated
		log.Info("library created", "library_id", lib.ID, "root", lib.RootPath)
	}
	writeJSON(w, log, status, newLibraryResponse(lib, nil))
}

func (s *Server) handleListLibraries(w http.ResponseWriter, r *http.Request) {
	log := loggerFrom(r.Context(), s.log)

	libs, err := s.store.ListLibraries(r.Context())
	if err != nil {
		log.Error("listing libraries", "error", err)
		writeError(w, log, http.StatusInternalServerError, "internal", "failed to list libraries")
		return
	}

	out := make([]libraryResponse, 0, len(libs))
	for i := range libs {
		out = append(out, newLibraryResponse(&libs[i], nil))
	}
	writeJSON(w, log, http.StatusOK, map[string]any{"libraries": out})
}

func (s *Server) handleGetLibrary(w http.ResponseWriter, r *http.Request) {
	log := loggerFrom(r.Context(), s.log)

	lib, ok := s.lookupLibrary(w, r)
	if !ok {
		return
	}

	count, err := s.store.CountPhotos(r.Context(), lib.ID)
	if err != nil {
		log.Error("counting photos", "error", err, "library_id", lib.ID)
		writeError(w, log, http.StatusInternalServerError, "internal", "failed to count photos")
		return
	}

	byState, err := s.store.CountPhotosByState(r.Context(), lib.ID)
	if err != nil {
		log.Error("counting photos by state", "error", err, "library_id", lib.ID)
		writeError(w, log, http.StatusInternalServerError, "internal", "failed to count photos")
		return
	}

	dupes, err := s.store.SummariseDuplicates(r.Context(), lib.ID)
	if err != nil {
		log.Error("summarising duplicates", "error", err, "library_id", lib.ID)
		writeError(w, log, http.StatusInternalServerError, "internal", "failed to summarise duplicates")
		return
	}

	meta, err := s.store.SummariseMetadata(r.Context(), lib.ID)
	if err != nil {
		log.Error("summarising metadata", "error", err, "library_id", lib.ID)
		writeError(w, log, http.StatusInternalServerError, "internal", "failed to summarise metadata")
		return
	}

	resp := newLibraryResponse(lib, &count)
	writeJSON(w, log, http.StatusOK, map[string]any{
		"library":         resp,
		"photos_by_state": byState,
		"metadata":        meta,
		"duplicates":      dupes,
		// Whether THIS process is scanning/extracting. Distinct from
		// scan_state, which is what the database believes -- if they disagree
		// after a crash, that is worth being able to see.
		"scanning_here":   s.scanner.IsScanning(lib.ID),
		"extracting_here": s.processor.IsProcessing(lib.ID),
		"hashing_here":    s.processor.IsHashing(lib.ID),
	})
}

// handleScanLibrary kicks off a scan and returns immediately with 202.
//
// Async because a large library takes longer than a sensible HTTP timeout.
// Progress is polled from GET /api/libraries/{id}, which reads the counts the
// scan is already writing -- no separate progress channel to keep in sync.
func (s *Server) handleScanLibrary(w http.ResponseWriter, r *http.Request) {
	log := loggerFrom(r.Context(), s.log)

	lib, ok := s.lookupLibrary(w, r)
	if !ok {
		return
	}

	// force=true recovers a library stuck in "scanning" after a crash.
	force := r.URL.Query().Get("force") == "true"

	// Re-validate the stored path on every scan rather than trusting it because
	// it passed once. PHOTO_ROOT may have been reconfigured, or the directory
	// replaced with a symlink, since the library was created.
	if _, err := photos.ResolveWithin(s.cfg.PhotoRoot, lib.RootPath); err != nil {
		log.Warn("library root no longer valid", "library_id", lib.ID, "error", err)
		writeError(w, log, http.StatusBadRequest, "path_outside_root",
			"library root is no longer inside the configured photo root")
		return
	}
	if err := requireDirectory(lib.RootPath); err != nil {
		writeError(w, log, http.StatusBadRequest, "root_unavailable", err.Error())
		return
	}

	if err := s.scanner.StartAsync(r.Context(), lib, force); err != nil {
		if errors.Is(err, ingest.ErrScanInProgress) {
			writeError(w, log, http.StatusConflict, "scan_in_progress",
				"a scan is already running for this library; pass ?force=true to override a stuck scan")
			return
		}
		log.Error("starting scan", "error", err, "library_id", lib.ID)
		writeError(w, log, http.StatusInternalServerError, "internal", "failed to start scan")
		return
	}

	writeJSON(w, log, http.StatusAccepted, map[string]any{
		"library_id": lib.ID,
		"status":     "scanning",
		"message":    "scan started; poll GET /api/libraries/" + lib.ID + " for progress",
	})
}

func (s *Server) handleListPhotos(w http.ResponseWriter, r *http.Request) {
	log := loggerFrom(r.Context(), s.log)

	lib, ok := s.lookupLibrary(w, r)
	if !ok {
		return
	}

	q := r.URL.Query()
	opts := store.ListPhotosOptions{
		Limit:  atoiDefault(q.Get("limit"), 100),
		Offset: atoiDefault(q.Get("offset"), 0),
		State:  photos.State(q.Get("state")),
	}

	list, err := s.store.ListPhotos(r.Context(), lib.ID, opts)
	if err != nil {
		log.Error("listing photos", "error", err, "library_id", lib.ID)
		writeError(w, log, http.StatusInternalServerError, "internal", "failed to list photos")
		return
	}

	total, err := s.store.CountPhotos(r.Context(), lib.ID)
	if err != nil {
		log.Error("counting photos", "error", err, "library_id", lib.ID)
		writeError(w, log, http.StatusInternalServerError, "internal", "failed to count photos")
		return
	}

	writeJSON(w, log, http.StatusOK, map[string]any{
		"photos": list,
		"total":  total,
		"limit":  opts.Limit,
		"offset": opts.Offset,
	})
}

// lookupLibrary resolves the {id} path segment, writing the error response
// itself. Returns ok=false when the caller should stop.
func (s *Server) lookupLibrary(w http.ResponseWriter, r *http.Request) (*photos.Library, bool) {
	log := loggerFrom(r.Context(), s.log)

	id := r.PathValue("id")
	if id == "" {
		writeError(w, log, http.StatusBadRequest, "invalid_request", "library id is required")
		return nil, false
	}

	lib, err := s.store.GetLibrary(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, log, http.StatusNotFound, "not_found", "library not found")
			return nil, false
		}
		// An invalid UUID reaches Postgres as a cast error rather than
		// ErrNotFound. From the client's perspective it is still "no such
		// library", and a 500 would be misleading.
		if isInvalidUUID(err) {
			writeError(w, log, http.StatusNotFound, "not_found", "library not found")
			return nil, false
		}
		log.Error("looking up library", "error", err, "library_id", id)
		writeError(w, log, http.StatusInternalServerError, "internal", "failed to look up library")
		return nil, false
	}
	return lib, true
}

// decodeJSON reads a JSON body with a size limit and strict field checking.
//
// DisallowUnknownFields turns a typo in a client's payload into a clear 400
// instead of a silently ignored field, which is a genuinely common source of
// "why isn't my setting taking effect".
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any, log *slog.Logger) error {
	// 1MB is far more than any request this API takes; the cap stops an
	// unbounded body from consuming memory.
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)

	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()

	if err := dec.Decode(dst); err != nil {
		switch {
		case errors.Is(err, io.EOF):
			writeError(w, log, http.StatusBadRequest, "invalid_request", "request body is empty")
		default:
			writeError(w, log, http.StatusBadRequest, "invalid_request", "invalid JSON: "+err.Error())
		}
		return err
	}

	// A second value in the body means the client sent something we are only
	// half-reading. Better to reject than to silently use the first object.
	if dec.More() {
		writeError(w, log, http.StatusBadRequest, "invalid_request", "request body must contain a single JSON object")
		return errors.New("multiple JSON values")
	}
	return nil
}

func atoiDefault(s string, def int) int {
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return n
}

func isInvalidUUID(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "invalid input syntax for type uuid") ||
		strings.Contains(msg, "invalid UUID")
}

// handleExtractMetadata starts metadata extraction for a library.
//
// Async for the same reason scanning is: a large library takes longer than a
// sensible HTTP timeout. Progress is polled from GET /api/libraries/{id},
// which reads the counts extraction is already writing.
//
// In Phase 5 this endpoint stays but its body changes: instead of running the
// work in-process it will enqueue one EXTRACT_METADATA job per photo and let
// the worker pool consume them. The extraction code itself does not move.
func (s *Server) handleExtractMetadata(w http.ResponseWriter, r *http.Request) {
	log := loggerFrom(r.Context(), s.log)

	lib, ok := s.lookupLibrary(w, r)
	if !ok {
		return
	}

	if err := requireDirectory(lib.RootPath); err != nil {
		writeError(w, log, http.StatusBadRequest, "root_unavailable", err.Error())
		return
	}

	if err := s.processor.StartAsync(r.Context(), lib); err != nil {
		if errors.Is(err, ingest.ErrProcessingInProgress) {
			writeError(w, log, http.StatusConflict, "processing_in_progress",
				"metadata extraction is already running for this library")
			return
		}
		log.Error("starting metadata extraction", "error", err, "library_id", lib.ID)
		writeError(w, log, http.StatusInternalServerError, "internal", "failed to start extraction")
		return
	}

	writeJSON(w, log, http.StatusAccepted, map[string]any{
		"library_id": lib.ID,
		"status":     "extracting",
		"message":    "metadata extraction started; poll GET /api/libraries/" + lib.ID + " for progress",
	})
}

// handleHashLibrary starts content hashing, which rebuilds duplicate groups on
// completion.
func (s *Server) handleHashLibrary(w http.ResponseWriter, r *http.Request) {
	log := loggerFrom(r.Context(), s.log)

	lib, ok := s.lookupLibrary(w, r)
	if !ok {
		return
	}
	if err := requireDirectory(lib.RootPath); err != nil {
		writeError(w, log, http.StatusBadRequest, "root_unavailable", err.Error())
		return
	}

	if err := s.processor.StartHashAsync(r.Context(), lib); err != nil {
		if errors.Is(err, ingest.ErrProcessingInProgress) {
			writeError(w, log, http.StatusConflict, "processing_in_progress",
				"hashing is already running for this library")
			return
		}
		log.Error("starting hashing", "error", err, "library_id", lib.ID)
		writeError(w, log, http.StatusInternalServerError, "internal", "failed to start hashing")
		return
	}

	writeJSON(w, log, http.StatusAccepted, map[string]any{
		"library_id": lib.ID,
		"status":     "hashing",
		"message":    "hashing started; poll GET /api/libraries/" + lib.ID + " for progress",
	})
}

// handleListDuplicates returns exact-duplicate groups, biggest win first.
func (s *Server) handleListDuplicates(w http.ResponseWriter, r *http.Request) {
	log := loggerFrom(r.Context(), s.log)

	lib, ok := s.lookupLibrary(w, r)
	if !ok {
		return
	}

	q := r.URL.Query()
	groups, err := s.store.ListDuplicateGroups(r.Context(), lib.ID,
		atoiDefault(q.Get("limit"), 50), atoiDefault(q.Get("offset"), 0))
	if err != nil {
		log.Error("listing duplicates", "error", err, "library_id", lib.ID)
		writeError(w, log, http.StatusInternalServerError, "internal", "failed to list duplicates")
		return
	}

	summary, err := s.store.SummariseDuplicates(r.Context(), lib.ID)
	if err != nil {
		log.Error("summarising duplicates", "error", err, "library_id", lib.ID)
		writeError(w, log, http.StatusInternalServerError, "internal", "failed to summarise duplicates")
		return
	}

	writeJSON(w, log, http.StatusOK, map[string]any{
		"groups":  groups,
		"summary": summary,
		// Stated explicitly in the payload so no client can mistake these for
		// actions already taken. Nothing is ever deleted by this system.
		"note": "suggestions only; no files have been or will be deleted by this application",
	})
}

// handleProcessLibrary fans out the per-photo pipeline as queued jobs.
//
// This supersedes the in-process /metadata and /hash endpoints: instead of
// doing the work in the API, it enqueues and lets the worker pool consume.
// The endpoint returns immediately with how many jobs were created.
func (s *Server) handleProcessLibrary(w http.ResponseWriter, r *http.Request) {
	log := loggerFrom(r.Context(), s.log)

	lib, ok := s.lookupLibrary(w, r)
	if !ok {
		return
	}

	enqueued, err := s.scanner.EnqueuePhotoJobs(r.Context(), lib.ID)
	if err != nil {
		log.Error("enqueuing jobs", "error", err, "library_id", lib.ID)
		writeError(w, log, http.StatusInternalServerError, "internal", "failed to enqueue jobs")
		return
	}

	counts, err := s.store.CountAllJobs(r.Context(), lib.ID)
	if err != nil {
		log.Error("counting jobs", "error", err, "library_id", lib.ID)
		writeError(w, log, http.StatusInternalServerError, "internal", "failed to count jobs")
		return
	}

	log.Info("enqueued jobs", "library_id", lib.ID, "new_jobs", enqueued)
	writeJSON(w, log, http.StatusAccepted, map[string]any{
		"library_id": lib.ID,
		"enqueued":   enqueued,
		"queue":      counts,
		"message":    "jobs queued; workers will consume them. Poll GET /api/libraries/" + lib.ID + "/jobs",
	})
}

// handleListJobs reports queue state for a library.
func (s *Server) handleListJobs(w http.ResponseWriter, r *http.Request) {
	log := loggerFrom(r.Context(), s.log)

	lib, ok := s.lookupLibrary(w, r)
	if !ok {
		return
	}

	byType, err := s.store.CountJobsByType(r.Context(), lib.ID)
	if err != nil {
		log.Error("counting jobs by type", "error", err, "library_id", lib.ID)
		writeError(w, log, http.StatusInternalServerError, "internal", "failed to count jobs")
		return
	}

	total, err := s.store.CountAllJobs(r.Context(), lib.ID)
	if err != nil {
		log.Error("counting jobs", "error", err, "library_id", lib.ID)
		writeError(w, log, http.StatusInternalServerError, "internal", "failed to count jobs")
		return
	}

	writeJSON(w, log, http.StatusOK, map[string]any{
		"total":   total,
		"by_type": byType,
	})
}

// handleListWorkers reports the worker fleet.
func (s *Server) handleListWorkers(w http.ResponseWriter, r *http.Request) {
	log := loggerFrom(r.Context(), s.log)

	workers, err := s.store.ListWorkers(r.Context())
	if err != nil {
		log.Error("listing workers", "error", err)
		writeError(w, log, http.StatusInternalServerError, "internal", "failed to list workers")
		return
	}

	active, capacity, running := 0, 0, 0
	for _, wk := range workers {
		if wk.Status == "active" {
			active++
			capacity += wk.Concurrency
		}
		running += wk.RunningJobs
	}

	writeJSON(w, log, http.StatusOK, map[string]any{
		"workers": workers,
		"summary": map[string]int{
			"active":       active,
			"total_slots":  capacity,
			"running_jobs": running,
		},
	})
}
