package stream

import (
	"bytes"
	"encoding/binary"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestStdcopyWriter_WriteFrame(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		data       []byte
		wantSize   uint32
		streamType byte
		wantType   byte
	}{
		{
			name:       "stdout frame",
			streamType: Stdout,
			data:       []byte("hello stdout\n"),
			wantType:   1,
			wantSize:   13,
		},
		{
			name:       "stderr frame",
			streamType: Stderr,
			data:       []byte("error message\n"),
			wantType:   2,
			wantSize:   14,
		},
		{
			name:       "empty payload",
			streamType: Stdout,
			data:       []byte{},
			wantType:   1,
			wantSize:   0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			rec := httptest.NewRecorder()
			sw := NewStdcopy(rec)

			if err := sw.WriteFrame(tt.streamType, tt.data); err != nil {
				t.Fatalf("WriteFrame() error = %v", err)
			}

			if got := rec.Header().Get("Content-Type"); got != "application/vnd.docker.raw-stream" {
				t.Errorf("Content-Type = %q, want %q", got, "application/vnd.docker.raw-stream")
			}

			if rec.Code != http.StatusOK {
				t.Errorf("status = %d, want %d", rec.Code, http.StatusOK)
			}

			assertFrame(t, rec.Body.Bytes(), tt.wantType, tt.wantSize, tt.data)
		})
	}
}

func TestFrame(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		data       []byte
		wantSize   uint32
		streamType byte
		wantType   byte
	}{
		{
			name:       "stdout",
			streamType: Stdout,
			data:       []byte("test data"),
			wantType:   1,
			wantSize:   9,
		},
		{
			name:       "stderr",
			streamType: Stderr,
			data:       []byte("err"),
			wantType:   2,
			wantSize:   3,
		},
		{
			name:       "large payload",
			streamType: Stdout,
			data:       make([]byte, 1024),
			wantType:   1,
			wantSize:   1024,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			assertFrame(t, Frame(tt.streamType, tt.data), tt.wantType, tt.wantSize, tt.data)
		})
	}
}

// assertFrame validates a stdcopy frame's header and payload.
func assertFrame(t *testing.T, frame []byte, wantType byte, wantSize uint32, wantData []byte) {
	t.Helper()

	wantLen := 8 + len(wantData)
	if len(frame) != wantLen {
		t.Fatalf("frame length = %d, want %d", len(frame), wantLen)
	}

	if frame[0] != wantType {
		t.Errorf("stream type = %d, want %d", frame[0], wantType)
	}

	for i := 1; i < 4; i++ {
		if frame[i] != 0 {
			t.Errorf("padding byte[%d] = %d, want 0", i, frame[i])
		}
	}

	gotSize := binary.BigEndian.Uint32(frame[4:8])
	if gotSize != wantSize {
		t.Errorf("size = %d, want %d", gotSize, wantSize)
	}

	if !bytes.Equal(frame[8:], wantData) {
		t.Errorf("payload = %q, want %q", frame[8:], wantData)
	}
}
