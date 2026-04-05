package handlers

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/url"

	"github.com/Shikachuu/kogia/internal/api/errdefs"
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

// respondError writes a JSON error response using errdefs to determine the HTTP
// status code. For errdefs errors the public message is sent to the client and
// the internal error (if any) is logged. Plain (non-errdefs) errors are treated
// as 500s: the full error is logged but only "internal server error" is sent.
func respondError(w http.ResponseWriter, err error) {
	code := errdefs.StatusCode(err)

	// Log internal cause when available.
	if internal := errdefs.InternalErr(err); internal != nil {
		slog.Error("request error",
			"status", code,
			"public", err.Error(),
			"internal", internal.Error(),
		)
	}

	// Extract the client-safe message, traversing any fmt.Errorf wrapping.
	msg := errdefs.SafeMessage(err)

	// For plain errors (non-errdefs, which map to 500), never leak the real
	// message to the client.
	if code == http.StatusInternalServerError {
		slog.Error("request error", "status", code, "err", err.Error())

		msg = "internal server error"
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)

	if encErr := json.NewEncoder(w).Encode(map[string]string{"message": msg}); encErr != nil {
		slog.Error("failed to encode error response", "err", encErr)
	}
}

// errorJSON writes a Docker API error response. For 500s the real error is
// replaced with a generic message. For other codes the safe message from
// errdefs is used (falling back to err.Error() for non-errdefs errors).
func errorJSON(w http.ResponseWriter, status int, err error) {
	msg := errdefs.SafeMessage(err)
	if status == http.StatusInternalServerError {
		msg = "internal server error"
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)

	resp := struct {
		Message string `json:"message"`
	}{Message: msg}

	if encErr := json.NewEncoder(w).Encode(resp); encErr != nil {
		slog.Error("failed to encode error response", "err", encErr)
	}
}
