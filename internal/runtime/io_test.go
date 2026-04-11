package runtime

import (
	"bytes"
	"errors"
	"io"
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

func TestNewContainerIO_NonTTY(t *testing.T) {
	t.Parallel()

	driver := &mockDriver{}

	cio, err := newContainerIO(driver, false, false)
	if err != nil {
		t.Fatal(err)
	}

	defer cio.Close()

	stdoutW, stderrW := cio.WriterFds()

	if stdoutW == nil {
		t.Error("expected non-nil stdout write fd")
	}

	if stderrW == nil {
		t.Error("expected non-nil stderr write fd")
	}

	if cio.StdinFd() != nil {
		t.Error("expected nil stdin fd when openStdin=false")
	}

	if cio.tty {
		t.Error("expected tty=false")
	}
}

func TestNewContainerIO_TTY(t *testing.T) {
	t.Parallel()

	driver := &mockDriver{}

	cio, err := newContainerIO(driver, true, true)
	if err != nil {
		t.Fatal(err)
	}

	defer cio.Close()

	stdoutW, stderrW := cio.WriterFds()

	if stdoutW != nil {
		t.Error("expected nil stdout write fd in TTY mode")
	}

	if stderrW != nil {
		t.Error("expected nil stderr write fd in TTY mode")
	}

	if cio.ptyMaster != nil {
		t.Error("ptyMaster should be nil before SetPTYMaster()")
	}

	if !cio.tty {
		t.Error("expected tty=true")
	}
}

func TestNewContainerIO_WithStdin(t *testing.T) {
	t.Parallel()

	driver := &mockDriver{}

	cio, err := newContainerIO(driver, false, true)
	if err != nil {
		t.Fatal(err)
	}

	defer cio.Close()

	if cio.StdinFd() == nil {
		t.Error("expected non-nil stdin fd when openStdin=true")
	}

	if cio.stdin == nil {
		t.Error("expected non-nil stdin write end")
	}
}

func TestContainerIO_StdinPipe(t *testing.T) {
	t.Parallel()

	driver := &mockDriver{}

	cio, err := newContainerIO(driver, false, true)
	if err != nil {
		t.Fatal(err)
	}

	defer cio.Close()

	// Write through WriteStdin, read from StdinFd.
	msg := []byte("hello stdin\n")

	n, err := cio.WriteStdin(msg)
	if err != nil {
		t.Fatal(err)
	}

	if n != len(msg) {
		t.Errorf("WriteStdin: got %d, want %d", n, len(msg))
	}

	// Read from the read end.
	buf := make([]byte, len(msg))

	n, err = cio.StdinFd().Read(buf)
	if err != nil {
		t.Fatal(err)
	}

	if !bytes.Equal(buf[:n], msg) {
		t.Errorf("stdin read: got %q, want %q", string(buf[:n]), string(msg))
	}
}

func TestContainerIO_CloseStdin(t *testing.T) {
	t.Parallel()

	driver := &mockDriver{}

	cio, err := newContainerIO(driver, false, true)
	if err != nil {
		t.Fatal(err)
	}

	defer cio.Close()

	stdinR := cio.StdinFd()

	cio.CloseStdin()

	// Read should return EOF.
	buf := make([]byte, 1)

	_, err = stdinR.Read(buf)
	if !errors.Is(err, io.EOF) {
		t.Errorf("expected EOF after CloseStdin, got %v", err)
	}
}

func TestContainerIO_WriteStdin_NoStdin(t *testing.T) {
	t.Parallel()

	driver := &mockDriver{}

	cio, err := newContainerIO(driver, false, false)
	if err != nil {
		t.Fatal(err)
	}

	defer cio.Close()

	_, err = cio.WriteStdin([]byte("data"))
	if err == nil {
		t.Error("expected error when writing to stdin without openStdin")
	}
}

func TestContainerIO_AttachWriter(t *testing.T) {
	t.Parallel()

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}

	driver := &mockDriver{}
	cio := &containerIO{driver: driver}
	cio.wg.Add(1)

	// Register an attach writer.
	var attachBuf bytes.Buffer
	cio.AddAttachWriter(&attachBuf)

	go cio.copyStream(r, "stdout")

	_, _ = w.WriteString("hello\nworld\n")
	_ = w.Close()

	cio.wg.Wait()

	_ = r.Close()

	// Verify log driver received data.
	lines := driver.lines()
	if len(lines) != 2 {
		t.Fatalf("expected 2 log lines, got %d", len(lines))
	}

	// Verify attach writer received data.
	got := attachBuf.String()
	if got != "hello\nworld\n" {
		t.Errorf("attach writer: got %q, want %q", got, "hello\nworld\n")
	}
}

func TestContainerIO_MultipleAttachWriters(t *testing.T) {
	t.Parallel()

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}

	driver := &mockDriver{}
	cio := &containerIO{driver: driver}
	cio.wg.Add(1)

	var buf1, buf2 bytes.Buffer
	cio.AddAttachWriter(&buf1)
	cio.AddAttachWriter(&buf2)

	go cio.copyStream(r, "stdout")

	_, _ = w.WriteString("data\n")
	_ = w.Close()

	cio.wg.Wait()

	_ = r.Close()

	if buf1.String() != "data\n" {
		t.Errorf("writer 1: got %q, want %q", buf1.String(), "data\n")
	}

	if buf2.String() != "data\n" {
		t.Errorf("writer 2: got %q, want %q", buf2.String(), "data\n")
	}
}

func TestContainerIO_RemoveAttachWriter(t *testing.T) {
	t.Parallel()

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}

	driver := &mockDriver{}
	cio := &containerIO{driver: driver}

	var buf bytes.Buffer
	cio.AddAttachWriter(&buf)
	cio.RemoveAttachWriter(&buf)

	cio.wg.Add(1)

	go cio.copyStream(r, "stdout")

	_, _ = w.WriteString("data\n")
	_ = w.Close()

	cio.wg.Wait()

	_ = r.Close()

	// Attach writer was removed, should have no data.
	if buf.Len() != 0 {
		t.Errorf("removed writer should have no data, got %q", buf.String())
	}

	// Log driver should still have data.
	if len(driver.lines()) != 1 {
		t.Errorf("expected 1 log line, got %d", len(driver.lines()))
	}
}
