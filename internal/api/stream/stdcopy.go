package stream

import (
	"encoding/binary"
	"fmt"
	"io"
	"net/http"
)

var _ FrameWriter = (*StdcopyWriter)(nil)

// Docker stdcopy stream type constants.
const (
	Stdout byte = 1
	Stderr byte = 2
)

// StdcopyWriter streams Docker stdcopy multiplexed frames to an HTTP response.
// Each frame has an 8-byte header [stream_type, 0, 0, 0, size(4 bytes BE)]
// followed by the payload. Not safe for concurrent use.
type StdcopyWriter struct {
	w       io.Writer
	flusher http.Flusher
}

// NewStdcopy sets Content-Type to application/vnd.docker.raw-stream, sends
// HTTP 200, asserts http.Flusher on w, and returns a ready-to-use writer.
func NewStdcopy(w http.ResponseWriter) *StdcopyWriter {
	w.Header().Set("Content-Type", "application/vnd.docker.raw-stream")
	w.WriteHeader(http.StatusOK)

	flusher, _ := w.(http.Flusher)

	return &StdcopyWriter{w: w, flusher: flusher}
}

// WriteFrame writes a single stdcopy frame and flushes.
func (sw *StdcopyWriter) WriteFrame(streamType byte, data []byte) error {
	var header [8]byte

	header[0] = streamType
	binary.BigEndian.PutUint32(header[4:8], uint32(len(data))) //nolint:gosec // Frame size is bounded by caller.

	if _, err := sw.w.Write(header[:]); err != nil {
		return fmt.Errorf("stdcopy header: %w", err)
	}

	if _, err := sw.w.Write(data); err != nil {
		return fmt.Errorf("stdcopy payload: %w", err)
	}

	if sw.flusher != nil {
		sw.flusher.Flush()
	}

	return nil
}

// Frame builds a stdcopy frame as a byte slice without writing it anywhere.
// Use this when frames need to be buffered in memory (e.g. runtime I/O fan-out).
func Frame(streamType byte, data []byte) []byte {
	frame := make([]byte, 8+len(data))
	frame[0] = streamType
	binary.BigEndian.PutUint32(frame[4:8], uint32(len(data))) //nolint:gosec // Frame size is bounded by caller.
	copy(frame[8:], data)

	return frame
}
