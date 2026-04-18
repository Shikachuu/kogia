package stream

import (
	"net/http"

	"github.com/Shikachuu/kogia/internal/api/errdefs"
)

// ProgressMsg is the Docker NDJSON progress message format used by image
// pull, push, and load endpoints.
type ProgressMsg struct {
	Status         string       `json:"status,omitempty"`
	ID             string       `json:"id,omitempty"`
	Progress       string       `json:"progress,omitempty"`
	ProgressDetail any          `json:"progressDetail,omitempty"`
	ErrorDetail    *ErrorDetail `json:"errorDetail,omitempty"`
	Error          string       `json:"error,omitempty"`
}

// ErrorDetail carries the error message inside a ProgressMsg.
type ErrorDetail struct {
	Message string `json:"message"`
}

// WriteError writes an error as an in-band NDJSON progress message.
// For errdefs errors the client-safe message is used. For plain errors
// a generic message is sent to prevent leaking internal details.
func WriteError(nw *NDJSONWriter, err error) {
	msg := errdefs.SafeMessage(err)

	if errdefs.StatusCode(err) == http.StatusInternalServerError {
		msg = "internal server error"
	}

	_ = nw.Encode(&ProgressMsg{
		ErrorDetail: &ErrorDetail{Message: msg},
		Error:       msg,
	})
}
