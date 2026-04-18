package image

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
)

func TestProgressWriter(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		writes     []string
		wantLines  []string
		wantBufLen int
	}{
		{
			name:      "single complete line",
			writes:    []string{"Copying blob sha256:abc123\n"},
			wantLines: []string{"Copying blob sha256:abc123"},
		},
		{
			name:      "multiple lines in one write",
			writes:    []string{"line one\nline two\n"},
			wantLines: []string{"line one", "line two"},
		},
		{
			name:       "partial line buffered",
			writes:     []string{"partial"},
			wantLines:  nil,
			wantBufLen: 7,
		},
		{
			name:      "partial then completed",
			writes:    []string{"start of ", "line\n"},
			wantLines: []string{"start of line"},
		},
		{
			name:      "empty lines skipped",
			writes:    []string{"first\n\n\nsecond\n"},
			wantLines: []string{"first", "second"},
		},
		{
			name:      "whitespace-only lines skipped",
			writes:    []string{"hello\n   \nworld\n"},
			wantLines: []string{"hello", "world"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var buf bytes.Buffer
			pw := &progressWriter{w: &buf}

			for _, w := range tt.writes {
				n, err := pw.Write([]byte(w))
				if err != nil {
					t.Fatalf("Write() error = %v", err)
				}

				if n != len(w) {
					t.Fatalf("Write() = %d, want %d", n, len(w))
				}
			}

			got := parseProgressStatuses(t, buf.String())

			if len(got) != len(tt.wantLines) {
				t.Fatalf("got %d messages %v, want %d %v", len(got), got, len(tt.wantLines), tt.wantLines)
			}

			for i, want := range tt.wantLines {
				if got[i] != want {
					t.Errorf("message[%d] = %q, want %q", i, got[i], want)
				}
			}

			if tt.wantBufLen > 0 && len(pw.buf) != tt.wantBufLen {
				t.Errorf("remaining buf len = %d, want %d", len(pw.buf), tt.wantBufLen)
			}
		})
	}
}

func TestProgressWriterConcurrent(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	pw := &progressWriter{w: &buf}

	const goroutines = 50
	const linesPerGoroutine = 20

	var wg sync.WaitGroup

	wg.Add(goroutines)

	for i := range goroutines {
		go func(id int) {
			defer wg.Done()

			for j := range linesPerGoroutine {
				_, _ = pw.Write([]byte(fmt.Sprintf("goroutine-%d-line-%d\n", id, j)))
			}
		}(i)
	}

	wg.Wait()

	got := parseProgressStatuses(t, buf.String())

	want := goroutines * linesPerGoroutine
	if len(got) != want {
		t.Fatalf("got %d messages, want %d", len(got), want)
	}

	// Every message must be well-formed (no interleaved data from other goroutines).
	for i, status := range got {
		if !strings.HasPrefix(status, "goroutine-") {
			t.Errorf("message[%d] = %q, does not start with expected prefix", i, status)
		}
	}
}

func TestWriteProgress(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	pw := &progressWriter{w: &buf}

	writeProgress(pw, &progressMsg{Status: "Pulling from library/nginx", ID: "latest"})

	var got progressMsg
	if err := json.Unmarshal([]byte(strings.TrimSpace(buf.String())), &got); err != nil {
		t.Fatalf("failed to parse NDJSON: %v", err)
	}

	if got.Status != "Pulling from library/nginx" {
		t.Errorf("status = %q, want %q", got.Status, "Pulling from library/nginx")
	}

	if got.ID != "latest" {
		t.Errorf("id = %q, want %q", got.ID, "latest")
	}
}

func TestWriteError(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer

	WriteError(&buf, nil, fmt.Errorf("something broke"))

	var got progressMsg
	if err := json.Unmarshal([]byte(strings.TrimSpace(buf.String())), &got); err != nil {
		t.Fatalf("failed to parse NDJSON: %v", err)
	}

	if got.Error == "" {
		t.Fatal("expected non-empty error field")
	}

	if got.ErrorDetail == nil {
		t.Fatal("expected non-nil errorDetail")
	}
}

// parseProgressStatuses decodes NDJSON lines and returns the status field of each.
func parseProgressStatuses(t *testing.T, raw string) []string {
	t.Helper()

	var statuses []string

	for _, line := range strings.Split(strings.TrimSpace(raw), "\n") {
		if line == "" {
			continue
		}

		var msg progressMsg
		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			t.Fatalf("invalid NDJSON line %q: %v", line, err)
		}

		statuses = append(statuses, msg.Status)
	}

	return statuses
}
