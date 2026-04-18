// Package stream provides unified streaming helpers for Docker-compatible
// HTTP responses. It handles NDJSON progress streams and Docker stdcopy
// multiplexed frames, owning HTTP response setup (Content-Type, status code,
// flusher assertion) so callers get a ready-to-use writer.
package stream

// Encoder streams JSON-encoded values to an HTTP response with automatic flushing.
type Encoder interface {
	Encode(v any) error
}

// FrameWriter streams Docker stdcopy multiplexed frames to an HTTP response
// with automatic flushing.
type FrameWriter interface {
	WriteFrame(streamType byte, data []byte) error
}
