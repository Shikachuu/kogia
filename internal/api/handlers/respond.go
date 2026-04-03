package handlers

import (
	"encoding/json"
	"log/slog"
	"net/http"
)

// respondJSON encodes v as JSON and writes it to w with the given status code.
func respondJSON[T any](w http.ResponseWriter, status int, v T) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)

	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Error("failed to encode response", "err", err)
	}
}
