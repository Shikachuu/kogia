package handlers

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/url"
)

// respondJSON encodes v as JSON and writes it to w with the given status code.
func respondJSON[T any](w http.ResponseWriter, status int, v T) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)

	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Error("failed to encode response", "err", err)
	}
}

// pathValue extracts a path parameter and URL-decodes it.
// This is needed because the slashy-path middleware encodes slashes in image
// names as %2F for ServeMux matching — PathValue returns the encoded form.
//

func pathValue(r *http.Request, key string) string {
	v := r.PathValue(key)

	decoded, err := url.PathUnescape(v)
	if err != nil {
		return v
	}

	return decoded
}

// errorJSON writes a Docker API error response.
func errorJSON(w http.ResponseWriter, status int, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)

	resp := struct {
		Message string `json:"message"`
	}{Message: err.Error()}

	if encErr := json.NewEncoder(w).Encode(resp); encErr != nil {
		slog.Error("failed to encode error response", "err", encErr)
	}
}
