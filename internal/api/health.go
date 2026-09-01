package api

import (
	"context"
	"net/http"
	"time"
)

type healthResponse struct {
	Status     string `json:"status"`
	UptimeSecs int64  `json:"uptime_seconds"`
}

type readyResponse struct {
	Status     string `json:"status"`
	Database   string `json:"database"`
	LatencyMS  int64  `json:"database_latency_ms"`
	DetailText string `json:"detail,omitempty"`
}

// handleHealthz is liveness. No dependencies are checked on purpose: this
// endpoint answers "should this process be restarted", and the answer is no
// just because Postgres is rebooting.
func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, loggerFrom(r.Context(), s.log), http.StatusOK, healthResponse{
		Status:     "ok",
		UptimeSecs: int64(time.Since(s.startedAt).Seconds()),
	})
}

// handleReadyz is readiness: it verifies the database is actually reachable.
// A short independent timeout is used so a hung database produces a fast 503
// rather than a request that hangs until the client gives up.
func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	log := loggerFrom(r.Context(), s.log)

	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()

	start := time.Now()
	err := s.pool.Ping(ctx)
	latency := time.Since(start).Milliseconds()

	if err != nil {
		log.Warn("readiness check failed", "error", err)
		writeJSON(w, log, http.StatusServiceUnavailable, readyResponse{
			Status:     "unavailable",
			Database:   "down",
			LatencyMS:  latency,
			DetailText: err.Error(),
		})
		return
	}

	writeJSON(w, log, http.StatusOK, readyResponse{
		Status:    "ok",
		Database:  "up",
		LatencyMS: latency,
	})
}
