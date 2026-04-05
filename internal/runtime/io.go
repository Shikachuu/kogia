package runtime

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"
	"time"

	clog "github.com/Shikachuu/kogia/internal/log"
)

// containerIO manages stdio pipes and logging for a non-TTY container.
type containerIO struct {
	stdout        *os.File // write end — passed to crun as stdout
	stderr        *os.File // write end — passed to crun as stderr
	stdoutRead    *os.File // read end — we read from this
	stderrRead    *os.File // read end — we read from this
	driver        clog.Driver
	wg            sync.WaitGroup
	writersClosed bool // true if write-ends were closed by Start()
}

// newContainerIO creates stdio pipe pairs and a log driver for the container.
func newContainerIO(driver clog.Driver) (*containerIO, error) {
	stdoutRead, stdoutWrite, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("create stdout pipe: %w", err)
	}

	stderrRead, stderrWrite, err := os.Pipe()
	if err != nil {
		_ = stdoutRead.Close()
		_ = stdoutWrite.Close()

		return nil, fmt.Errorf("create stderr pipe: %w", err)
	}

	cio := &containerIO{
		stdout:     stdoutWrite,
		stderr:     stderrWrite,
		stdoutRead: stdoutRead,
		stderrRead: stderrRead,
		driver:     driver,
	}

	return cio, nil
}

// startCopyLoop starts goroutines that read from the pipe read ends and write
// each line to the log driver. Call Close() when the container exits.
func (cio *containerIO) startCopyLoop() {
	cio.wg.Add(2)

	go cio.copyStream(cio.stdoutRead, "stdout")
	go cio.copyStream(cio.stderrRead, "stderr")
}

func (cio *containerIO) copyStream(r *os.File, stream string) {
	defer cio.wg.Done()

	scanner := bufio.NewScanner(r)
	// Increase buffer size to handle long lines.
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	for scanner.Scan() {
		line := scanner.Bytes()
		// Append newline to match Docker behavior.
		lineWithNL := make([]byte, len(line)+1)
		copy(lineWithNL, line)
		lineWithNL[len(lineWithNL)-1] = '\n'

		msg := &clog.Message{
			Stream:    stream,
			Line:      lineWithNL,
			Timestamp: time.Now(),
		}

		if err := cio.driver.Log(msg); err != nil {
			slog.Error("log driver write error", "stream", stream, "err", err)
		}
	}

	if err := scanner.Err(); err != nil && !errors.Is(err, io.EOF) {
		slog.Debug("stdio scan ended", "stream", stream, "err", err)
	}
}

// WriterFds returns the file descriptors to pass to the crun process as stdout and stderr.
func (cio *containerIO) WriterFds() (*os.File, *os.File) {
	return cio.stdout, cio.stderr
}

// MarkWritersClosed records that the write-end FDs were closed by the caller
// (typically after crun create, since the container holds its own references).
func (cio *containerIO) MarkWritersClosed() {
	cio.writersClosed = true
}

// Close closes the write ends of the pipes (unblocking readers),
// waits for copy goroutines to finish, then closes everything.
func (cio *containerIO) Close() {
	// Close write ends — this will cause the scanners to get EOF.
	// Skip if already closed by Start() after passing to crun.
	if !cio.writersClosed {
		_ = cio.stdout.Close()
		_ = cio.stderr.Close()
	}

	// Wait for copy goroutines to drain remaining data.
	cio.wg.Wait()

	// Close read ends.
	_ = cio.stdoutRead.Close()
	_ = cio.stderrRead.Close()

	// Flush and close the log driver.
	if err := cio.driver.Close(); err != nil {
		slog.Error("log driver close error", "err", err)
	}
}
