package api

import (
	"encoding/json"
	"log/slog"
	"net/http"
)

// writeJSON renders v with the given status. Encoding failures are logged
// rather than swallowed: by the time encoding fails the status line is already
// on the wire, so there is nothing useful to send the client, but a silent
// truncated body is exactly the sort of thing that wastes an afternoon.
func writeJSON(w http.ResponseWriter, log *slog.Logger, status int, v any) {
	buf, err := json.Marshal(v)
	if err != nil {
		log.Error("encoding response body", "error", err)
		http.Error(w, `{"error":{"code":"internal","message":"failed to encode response"}}`, http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if _, err := w.Write(buf); err != nil {
		log.Warn("writing response body", "error", err)
	}
}

// apiError is the single error shape every endpoint returns, so the CLI and the
// frontend need exactly one parser.
type apiError struct {
	Error errorBody `json:"error"`
}

type errorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func writeError(w http.ResponseWriter, log *slog.Logger, status int, code, message string) {
	writeJSON(w, log, status, apiError{Error: errorBody{Code: code, Message: message}})
}
