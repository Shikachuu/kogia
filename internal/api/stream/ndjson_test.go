package stream

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestNDJSONWriter(t *testing.T) {
	t.Parallel()

	type msg struct {
		Name  string `json:"name"`
		Value int    `json:"value,omitempty"`
	}

	tests := []struct {
		name       string
		wantCT     string
		values     []any
		wantLines  int
		wantStatus int
	}{
		{
			name:       "single struct",
			values:     []any{msg{Name: "test", Value: 42}},
			wantLines:  1,
			wantStatus: http.StatusOK,
			wantCT:     "application/json",
		},
		{
			name:       "multiple values",
			values:     []any{msg{Name: "a"}, msg{Name: "b"}, msg{Name: "c"}},
			wantLines:  3,
			wantStatus: http.StatusOK,
			wantCT:     "application/json",
		},
		{
			name:       "map value",
			values:     []any{map[string]string{"status": "pulling"}},
			wantLines:  1,
			wantStatus: http.StatusOK,
			wantCT:     "application/json",
		},
		{
			name:       "string value",
			values:     []any{"hello"},
			wantLines:  1,
			wantStatus: http.StatusOK,
			wantCT:     "application/json",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			rec := httptest.NewRecorder()
			nw := NewNDJSON(rec)

			for _, v := range tt.values {
				if err := nw.Encode(v); err != nil {
					t.Fatalf("Encode() error = %v", err)
				}
			}

			if rec.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d", rec.Code, tt.wantStatus)
			}

			if got := rec.Header().Get("Content-Type"); got != tt.wantCT {
				t.Errorf("Content-Type = %q, want %q", got, tt.wantCT)
			}

			lines := splitNDJSON(rec.Body.String())
			if len(lines) != tt.wantLines {
				t.Fatalf("got %d lines, want %d: %v", len(lines), tt.wantLines, lines)
			}

			for i, line := range lines {
				if !json.Valid([]byte(line)) {
					t.Errorf("line[%d] is not valid JSON: %q", i, line)
				}
			}
		})
	}
}

func TestNDJSONWriter_MarshalError(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	nw := NewNDJSON(rec)

	// Channels cannot be marshaled to JSON.
	err := nw.Encode(make(chan int))
	if err == nil {
		t.Fatal("Encode(chan) should return error")
	}
}

func TestNDJSONWriter_Concurrent(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	nw := NewNDJSON(rec)

	const (
		goroutines       = 50
		msgsPerGoroutine = 20
	)

	var wg sync.WaitGroup

	wg.Add(goroutines)

	for i := range goroutines {
		go func(id int) {
			defer wg.Done()

			for j := range msgsPerGoroutine {
				_ = nw.Encode(map[string]string{
					"id": fmt.Sprintf("g%d-m%d", id, j),
				})
			}
		}(i)
	}

	wg.Wait()

	lines := splitNDJSON(rec.Body.String())

	want := goroutines * msgsPerGoroutine
	if len(lines) != want {
		t.Fatalf("got %d lines, want %d", len(lines), want)
	}

	for i, line := range lines {
		if !json.Valid([]byte(line)) {
			t.Errorf("line[%d] is not valid JSON: %q", i, line)
		}
	}
}

func TestNDJSONWriter_WriterAndFlusher(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	nw := NewNDJSON(rec)

	if nw.Writer() == nil {
		t.Error("Writer() returned nil")
	}

	// httptest.ResponseRecorder implements http.Flusher.
	if nw.Flusher() == nil {
		t.Error("Flusher() returned nil for httptest.ResponseRecorder")
	}
}

// splitNDJSON splits NDJSON output into individual lines, skipping empty lines.
func splitNDJSON(s string) []string {
	var lines []string

	for _, line := range strings.Split(strings.TrimSpace(s), "\n") {
		if line != "" {
			lines = append(lines, line)
		}
	}

	return lines
}
