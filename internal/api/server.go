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

	return s.withRequestLogging(s.withRecovery(mux))
}
