package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"
	"time"
)

type ctxKey int

const (
	ctxKeyRequestID ctxKey = iota
	ctxKeyLogger
)

// statusRecorder captures the status code so the access log can report it.
// net/http gives no way to read back what a handler wrote.
type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	n, err := r.ResponseWriter.Write(b)
	r.bytes += n
	return n, err
}

// withRequestLogging assigns each request an ID, threads a logger carrying that
// ID through the request context, and emits one structured line per request.
// One line per request (not one on entry and one on exit) keeps the log
// readable at the volumes a scan produces.
func (s *Server) withRequestLogging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqID := r.Header.Get("X-Request-Id")
		if reqID == "" {
			reqID = newRequestID()
		}

		log := s.log.With("request_id", reqID)
		ctx := context.WithValue(r.Context(), ctxKeyRequestID, reqID)
		ctx = context.WithValue(ctx, ctxKeyLogger, log)

		rec := &statusRecorder{ResponseWriter: w}
		rec.Header().Set("X-Request-Id", reqID)

		start := time.Now()
		next.ServeHTTP(rec, r.WithContext(ctx))
		dur := time.Since(start)

		if rec.status == 0 {
			rec.status = http.StatusOK
		}

		level := slog.LevelInfo
		switch {
		case rec.status >= 500:
			level = slog.LevelError
		case rec.status >= 400:
			level = slog.LevelWarn
		// Health checks fire constantly; logging them at info drowns everything
		// else. They stay visible at debug.
		case r.URL.Path == "/healthz" || r.URL.Path == "/readyz":
			level = slog.LevelDebug
		}

		log.Log(r.Context(), level, "http request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"bytes", rec.bytes,
			"duration_ms", dur.Milliseconds(),
		)
	})
}

// withRecovery converts a panic in a handler into a 500 instead of killing the
// whole process and every other in-flight request with it.
func (s *Server) withRecovery(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				log := loggerFrom(r.Context(), s.log)
				log.Error("panic in handler", "panic", v, "path", r.URL.Path)
				writeError(w, log, http.StatusInternalServerError, "internal", "internal server error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func loggerFrom(ctx context.Context, fallback *slog.Logger) *slog.Logger {
	if l, ok := ctx.Value(ctxKeyLogger).(*slog.Logger); ok {
		return l
	}
	return fallback
}

func newRequestID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failing is essentially fatal for the machine, but a
		// request ID is not worth crashing over -- fall back to a timestamp.
		return hex.EncodeToString([]byte(time.Now().Format("150405.000000")))
	}
	return hex.EncodeToString(b[:])
}
