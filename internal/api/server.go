// Package api holds the HTTP surface. Handlers here do three things and only
// three things: parse the request, call into a service or store, and render the
// result. Business logic that lives in a handler cannot be tested without
// spinning up an HTTP server, so none of it does.
package api

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/srikarguntaka/photo-organizer/internal/config"
	"github.com/srikarguntaka/photo-organizer/internal/ingest"
	"github.com/srikarguntaka/photo-organizer/internal/store"
)

// Server wires dependencies into an http.Handler.
type Server struct {
	cfg       *config.Config
	pool      *pgxpool.Pool
	store     *store.Store
	scanner   *ingest.Scanner
	processor *ingest.Processor
	log       *slog.Logger
	// startedAt lets /healthz report uptime, which is a cheap way to notice a
	// process that is silently crash-looping.
	startedAt time.Time
}

func NewServer(cfg *config.Config, pool *pgxpool.Pool, st *store.Store, scanner *ingest.Scanner, processor *ingest.Processor, log *slog.Logger) *Server {
	return &Server{
		cfg:       cfg,
		pool:      pool,
		store:     st,
		scanner:   scanner,
		processor: processor,
		log:       log,
		startedAt: time.Now(),
	}
}

// Handler builds the routing table. Go 1.22+ method-aware patterns are used, so
// a GET-only route returns 405 rather than 404 for a POST -- no router
// dependency required.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// Liveness: is the process running? Deliberately does NOT touch the
	// database. A container orchestrator restarting the API because Postgres is
	// briefly down would turn a recoverable blip into an outage.
	mux.HandleFunc("GET /healthz", s.handleHealthz)

	// Readiness: can this process actually serve requests? This one does ping
	// the database, and returns 503 when it cannot.
	mux.HandleFunc("GET /readyz", s.handleReadyz)

	// Libraries.
	mux.HandleFunc("POST /api/libraries", s.handleCreateLibrary)
	mux.HandleFunc("GET /api/libraries", s.handleListLibraries)
	mux.HandleFunc("GET /api/libraries/{id}", s.handleGetLibrary)
	mux.HandleFunc("POST /api/libraries/{id}/scan", s.handleScanLibrary)
	mux.HandleFunc("GET /api/libraries/{id}/photos", s.handleListPhotos)
	mux.HandleFunc("POST /api/libraries/{id}/metadata", s.handleExtractMetadata)
	mux.HandleFunc("POST /api/libraries/{id}/hash", s.handleHashLibrary)
	mux.HandleFunc("GET /api/libraries/{id}/duplicates", s.handleListDuplicates)
	mux.HandleFunc("GET /api/libraries/{id}/similar", s.handleListSimilar)
	mux.HandleFunc("GET /api/libraries/{id}/quality", s.handleListQuality)
	mux.HandleFunc("GET /api/libraries/{id}/clusters", s.handleListClusters)

	// The read API proper: a filtered photo listing and a single-photo detail
	// view. The detail route is rooted at /api/photos rather than nested under
	// a library, because a photo id is globally unique and the client
	// following a link from a duplicate group has the photo id but not
	// necessarily the library it came from.
	mux.HandleFunc("GET /api/libraries/{id}/photos/search", s.handleListPhotoViews)
	mux.HandleFunc("GET /api/photos/{id}", s.handleGetPhoto)
	mux.HandleFunc("GET /api/photos/{id}/thumbnail", s.handleGetThumbnail)
	mux.HandleFunc("GET /api/photos/{id}/original", s.handleGetOriginal)

	// Job queue (Phase 5). /process enqueues work for the worker pool; the
	// older /metadata and /hash endpoints still run in-process and are kept
	// so the pipeline can be exercised without any workers running.
	mux.HandleFunc("POST /api/libraries/{id}/process", s.handleProcessLibrary)
	mux.HandleFunc("GET /api/libraries/{id}/jobs", s.handleListJobs)
	mux.HandleFunc("GET /api/workers", s.handleListWorkers)

	// Any /api/ path not matched above is a JSON 404, never the UI. More
	// specific patterns always win in ServeMux, so this only catches what
	// nothing else claimed.
	mux.HandleFunc("/api/", s.apiNotFound)

	// The web UI, when a build is present. Registered on "/" -- the least
	// specific pattern -- so every route above takes precedence over it.
	if s.cfg.WebDir != "" {
		mux.Handle("/", webHandler(s.cfg.WebDir))
	}

	return s.withRequestLogging(s.withRecovery(mux))
}
