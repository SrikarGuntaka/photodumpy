package api

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/srikarguntaka/photo-organizer/internal/quality"
	"github.com/srikarguntaka/photo-organizer/internal/store"
)

// parseTriState reads a query parameter that can be absent, true or false.
//
// Absent and false are different requests -- "any photo" versus "photos with
// no GPS" -- so this returns nil for absent rather than collapsing both into
// the zero value.
func parseTriState(q map[string][]string, key string) (*bool, error) {
	vals, ok := q[key]
	if !ok || len(vals) == 0 || vals[0] == "" {
		return nil, nil
	}
	switch strings.ToLower(vals[0]) {
	case "true", "1", "yes":
		t := true
		return &t, nil
	case "false", "0", "no":
		f := false
		return &f, nil
	default:
		return nil, fmt.Errorf("%s must be true or false, got %q", key, vals[0])
	}
}

// parseTime reads an RFC3339 timestamp, or a bare date.
//
// A bare date is accepted because a date picker sends one and requiring
// callers to append T00:00:00Z is friction with no benefit. It is interpreted
// as UTC midnight, consistent with how EXIF wall-clock times are read
// throughout -- see DESIGN_DECISIONS.md on timezones.
func parseTime(s string) (*time.Time, error) {
	if s == "" {
		return nil, nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return &t, nil
	}
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		return nil, fmt.Errorf("expected RFC3339 or YYYY-MM-DD, got %q", s)
	}
	return &t, nil
}

// knownFlags is the closed set of quality flags the filter accepts.
//
// Not a security boundary -- the value is bound, not interpolated -- but a
// typo like "possibly_blury" would otherwise return zero results and look like
// a finding rather than a mistake.
//
// Built from quality.AllFlags rather than retyped here. Hand-copying this list
// was wrong within minutes: it had "shadow_clipping" and "highlight_clipping"
// against the real "shadows_clipped" and "highlights_clipped", so filtering by
// two of the seven flags would have 400'd on a name the API itself emits.
var knownFlags = func() map[string]bool {
	m := make(map[string]bool, len(quality.AllFlags))
	for _, f := range quality.AllFlags {
		m[f] = true
	}
	return m
}()

// parsePhotoQuery turns request parameters into a store query.
//
// Bad input is rejected with 400 rather than silently ignored. A filter that
// quietly does nothing is worse than an error: the caller sees a plausible
// result set and has no way to tell it was unfiltered.
func parsePhotoQuery(r *http.Request) (store.ListPhotosQuery, error) {
	q := r.URL.Query()
	out := store.ListPhotosQuery{
		Limit:  atoiDefault(q.Get("limit"), 100),
		Offset: atoiDefault(q.Get("offset"), 0),
		Filter: store.PhotoFilter{
			State:        q.Get("state"),
			Flag:         q.Get("flag"),
			PathContains: q.Get("q"),
		},
	}

	if out.Filter.Flag != "" && !knownFlags[out.Filter.Flag] {
		return out, fmt.Errorf("unknown flag %q", out.Filter.Flag)
	}

	var err error
	if out.Filter.HasGPS, err = parseTriState(q, "has_gps"); err != nil {
		return out, err
	}
	if out.Filter.HasFlags, err = parseTriState(q, "has_flags"); err != nil {
		return out, err
	}
	if out.Filter.HasDuplicates, err = parseTriState(q, "has_duplicates"); err != nil {
		return out, err
	}
	if out.Filter.HasSimilar, err = parseTriState(q, "has_similar"); err != nil {
		return out, err
	}
	if out.Filter.CapturedFrom, err = parseTime(q.Get("from")); err != nil {
		return out, fmt.Errorf("from: %w", err)
	}
	if out.Filter.CapturedTo, err = parseTime(q.Get("to")); err != nil {
		return out, fmt.Errorf("to: %w", err)
	}
	if out.Filter.CapturedFrom != nil && out.Filter.CapturedTo != nil &&
		out.Filter.CapturedTo.Before(*out.Filter.CapturedFrom) {
		return out, errors.New("to must not be before from")
	}

	// Sort is validated here as well as allow-listed in the store. The store's
	// map is the boundary that keeps user text out of SQL; this check is so a
	// typo produces an error instead of silently falling back to path order
	// and looking like the sort was ignored.
	if s := q.Get("sort"); s != "" {
		sort := store.PhotoSort(s)
		if !store.ValidSort(sort) {
			return out, fmt.Errorf("unknown sort %q; valid: %s", s,
				strings.Join(store.ValidSorts(), ", "))
		}
		out.Sort = sort
	}
	switch strings.ToLower(q.Get("order")) {
	case "", "asc":
	case "desc":
		out.Descending = true
	default:
		return out, fmt.Errorf("order must be asc or desc, got %q", q.Get("order"))
	}

	return out, nil
}

// handleListPhotoViews is the read API's photo listing: filtered, sorted and
// paginated.
func (s *Server) handleListPhotoViews(w http.ResponseWriter, r *http.Request) {
	log := loggerFrom(r.Context(), s.log)

	lib, ok := s.lookupLibrary(w, r)
	if !ok {
		return
	}

	query, err := parsePhotoQuery(r)
	if err != nil {
		writeError(w, log, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}

	list, total, err := s.store.ListPhotoViews(r.Context(), lib.ID, query)
	if err != nil {
		log.Error("listing photos", "error", err, "library_id", lib.ID)
		writeError(w, log, http.StatusInternalServerError, "internal", "failed to list photos")
		return
	}

	writeJSON(w, log, http.StatusOK, map[string]any{
		"photos": list,
		// total is the count MATCHING THE FILTER, not the library size, so a
		// paginator built on it cannot offer pages that do not exist.
		"total":  total,
		"limit":  query.Limit,
		"offset": query.Offset,
		"sort":   defaultString(string(query.Sort), string(store.SortPath)),
		"order":  map[bool]string{true: "desc", false: "asc"}[query.Descending],
	})
}

// handleGetPhoto returns one photo with everything known about it, including
// the groups it belongs to.
func (s *Server) handleGetPhoto(w http.ResponseWriter, r *http.Request) {
	log := loggerFrom(r.Context(), s.log)

	id := r.PathValue("id")
	if id == "" {
		writeError(w, log, http.StatusBadRequest, "invalid_request", "photo id is required")
		return
	}

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

	relations, err := s.store.GetPhotoRelations(r.Context(), id)
	if err != nil {
		log.Error("loading photo relations", "error", err, "photo_id", id)
		writeError(w, log, http.StatusInternalServerError, "internal", "failed to load relations")
		return
	}

	writeJSON(w, log, http.StatusOK, map[string]any{
		"photo":     view,
		"relations": relations,
		"note": "scores describe pixels, not merit; flags are hedged on purpose. " +
			"suggested_keep is a suggestion and nothing is ever deleted",
	})
}

func defaultString(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
