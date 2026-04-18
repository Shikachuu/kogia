package stream

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
)

var _ Encoder = (*NDJSONWriter)(nil)

// NDJSONWriter streams NDJSON-encoded values to an HTTP response.
// Each call to Encode marshals the value as JSON, appends a newline,
// and flushes — atomically under a mutex. Safe for concurrent use.
type NDJSONWriter struct {
	w       io.Writer
	flusher http.Flusher
	mu      sync.Mutex
}

// NewNDJSON sets Content-Type to application/json, sends HTTP 200,
// asserts http.Flusher on w, and returns a ready-to-use writer.
func NewNDJSON(w http.ResponseWriter) *NDJSONWriter {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)

	flusher, _ := w.(http.Flusher)

	return &NDJSONWriter{w: w, flusher: flusher}
}

// Encode marshals v as JSON, writes it followed by a newline, and flushes.
func (nw *NDJSONWriter) Encode(v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("ndjson encode: %w", err)
	}

	nw.mu.Lock()
	defer nw.mu.Unlock()

	_, _ = nw.w.Write(data)
	_, _ = nw.w.Write([]byte("\n"))

	if nw.flusher != nil {
		nw.flusher.Flush()
	}

	return nil
}

// Writer returns the underlying io.Writer.
func (nw *NDJSONWriter) Writer() io.Writer {
	return nw.w
}

// Flusher returns the underlying http.Flusher, which may be nil.
func (nw *NDJSONWriter) Flusher() http.Flusher {
	return nw.flusher
}
