package runtime

import (
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	clog "github.com/Shikachuu/kogia/internal/log"
)

// mockDriver collects log messages for testing.
type mockDriver struct {
	msgs []*clog.Message
	mu   sync.Mutex
}

func (d *mockDriver) Log(msg *clog.Message) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	// Copy the line to avoid referencing the caller's buffer.
	line := make([]byte, len(msg.Line))
	copy(line, msg.Line)

	d.msgs = append(d.msgs, &clog.Message{
		Stream:    msg.Stream,
		Line:      line,
		Timestamp: msg.Timestamp,
	})

	return nil
}

func (d *mockDriver) Close() error { return nil }

func (d *mockDriver) ReadLogs(_ clog.ReadOpts) (*clog.Reader, error) {
	return &clog.Reader{Lines: make(<-chan *clog.Message), Close: func() {}}, nil
}

func (d *mockDriver) lines() []string {
	d.mu.Lock()
	defer d.mu.Unlock()

	out := make([]string, len(d.msgs))
	for i, m := range d.msgs {
		out[i] = string(m.Line)
	}

	return out
}

func TestCopyStream_FullLines(t *testing.T) {
	t.Parallel()

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}

	driver := &mockDriver{}
	cio := &containerIO{driver: driver}
	cio.wg.Add(1)

	go cio.copyStream(r, "stdout")

	_, _ = w.WriteString("line one\nline two\nline three\n")
	_ = w.Close()

	cio.wg.Wait()

	_ = r.Close()

	lines := driver.lines()
	if len(lines) != 3 {
		t.Fatalf("expected 3 lines, got %d: %v", len(lines), lines)
	}

	want := []string{"line one\n", "line two\n", "line three\n"}
	for i, got := range lines {
		if got != want[i] {
			t.Errorf("line %d: got %q, want %q", i, got, want[i])
		}
	}
}

func TestCopyStream_PartialLineAtEOF(t *testing.T) {
	t.Parallel()

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}

	driver := &mockDriver{}
	cio := &containerIO{driver: driver}
	cio.wg.Add(1)

	go cio.copyStream(r, "stdout")

	// Write a complete line followed by a partial line (no trailing newline).
	_, _ = w.WriteString("complete\npartial")
	_ = w.Close()

	cio.wg.Wait()

	_ = r.Close()

	lines := driver.lines()
	if len(lines) != 2 {
		t.Fatalf("expected 2 lines, got %d: %v", len(lines), lines)
	}

	if lines[0] != "complete\n" {
		t.Errorf("line 0: got %q, want %q", lines[0], "complete\n")
	}

	// Partial line should have newline appended.
	if lines[1] != "partial\n" {
		t.Errorf("line 1: got %q, want %q", lines[1], "partial\n")
	}
}

func TestCopyStream_EmptyOutput(t *testing.T) {
	t.Parallel()

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}

	driver := &mockDriver{}
	cio := &containerIO{driver: driver}
	cio.wg.Add(1)

	go cio.copyStream(r, "stdout")

	_ = w.Close()

	cio.wg.Wait()

	_ = r.Close()

	if len(driver.lines()) != 0 {
		t.Fatalf("expected 0 lines, got %d", len(driver.lines()))
	}
}

func TestCopyStream_LongLine(t *testing.T) {
	t.Parallel()

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}

	driver := &mockDriver{}
	cio := &containerIO{driver: driver}
	cio.wg.Add(1)

	go cio.copyStream(r, "stdout")

	// Write a line longer than the 64KB buffer.
	long := strings.Repeat("x", 128*1024) + "\n"
	_, _ = w.WriteString(long)
	_ = w.Close()

	cio.wg.Wait()

	_ = r.Close()

	lines := driver.lines()
	if len(lines) != 1 {
		t.Fatalf("expected 1 line, got %d: %v", len(lines), lines)
	}

	if lines[0] != long {
		t.Errorf("long line length: got %d, want %d", len(lines[0]), len(long))
	}
}

func TestCopyStream_StreamLabel(t *testing.T) {
	t.Parallel()

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}

	driver := &mockDriver{}
	cio := &containerIO{driver: driver}
	cio.wg.Add(1)

	go cio.copyStream(r, "stderr")

	_, _ = w.WriteString("error msg\n")
	_ = w.Close()

	cio.wg.Wait()

	_ = r.Close()

	driver.mu.Lock()
	defer driver.mu.Unlock()

	if len(driver.msgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(driver.msgs))
	}

	if driver.msgs[0].Stream != "stderr" {
		t.Errorf("stream: got %q, want %q", driver.msgs[0].Stream, "stderr")
	}

	if driver.msgs[0].Timestamp.IsZero() {
		t.Error("timestamp should not be zero")
	}

	if time.Since(driver.msgs[0].Timestamp) > 5*time.Second {
		t.Error("timestamp should be recent")
	}
}
