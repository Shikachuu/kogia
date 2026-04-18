package runtime

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"
	"time"

	clog "github.com/Shikachuu/kogia/internal/log"
)

const (
	streamStdout = "stdout"
	streamStderr = "stderr"
)

// ErrStdinNotAvailable is returned when writing to stdin on a container that has no stdin pipe.
var ErrStdinNotAvailable = errors.New("stdin not available")

// containerIO manages stdio pipes/PTY and logging for a container.
// It supports three modes:
//   - Non-TTY without stdin: stdout/stderr pipes only (docker run -d)
//   - Non-TTY with stdin: stdout/stderr + stdin pipes (docker run -i)
//   - TTY: single PTY master fd for bidirectional I/O (docker run -it)
type containerIO struct {
	driver           clog.Driver
	ptyMaster        *os.File
	stderrRead       *os.File
	stdin            *os.File
	stdinRead        *os.File
	stdout           *os.File
	stderr           *os.File
	stdoutRead       *os.File
	attachOut        []io.Writer
	attachBuf        []byte
	wg               sync.WaitGroup
	mu               sync.Mutex
	tty              bool
	writersClosed    bool
	attachBufFlushed bool
}

// newContainerIO creates a containerIO in the appropriate mode.
//
// When tty=true, no pipes are created — the PTY master fd arrives later via
// SetPTYMaster(). When openStdin=true, a stdin pipe pair is created.
func newContainerIO(driver clog.Driver, tty, openStdin bool) (*containerIO, error) {
	cio := &containerIO{
		driver: driver,
		tty:    tty,
	}

	if openStdin {
		stdinRead, stdinWrite, err := os.Pipe()
		if err != nil {
			return nil, fmt.Errorf("create stdin pipe: %w", err)
		}

		cio.stdin = stdinWrite
		cio.stdinRead = stdinRead
	}

	// TTY mode: no stdout/stderr pipes needed. crun uses console-socket instead.
	if tty {
		return cio, nil
	}

	stdoutRead, stdoutWrite, err := os.Pipe()
	if err != nil {
		cio.closeStdinPipes()
		return nil, fmt.Errorf("create stdout pipe: %w", err)
	}

	stderrRead, stderrWrite, err := os.Pipe()
	if err != nil {
		_ = stdoutRead.Close()
		_ = stdoutWrite.Close()

		cio.closeStdinPipes()

		return nil, fmt.Errorf("create stderr pipe: %w", err)
	}

	cio.stdout = stdoutWrite
	cio.stderr = stderrWrite
	cio.stdoutRead = stdoutRead
	cio.stderrRead = stderrRead

	return cio, nil
}

// SetPTYMaster stores the PTY master fd and starts the copy loop for TTY mode.
// Must be called after ReceivePTYMaster() returns the fd from the console-socket.
func (cio *containerIO) SetPTYMaster(pty *os.File) {
	cio.ptyMaster = pty
}

// startCopyLoop starts goroutines that read from the container's output
// and write to the log driver (and any attached writers).
//
// For non-TTY: two goroutines drain stdout and stderr pipes.
// For TTY: one goroutine drains the PTY master.
func (cio *containerIO) startCopyLoop() {
	if cio.tty {
		if cio.ptyMaster != nil {
			cio.wg.Add(1)

			go cio.copyStreamRaw(cio.ptyMaster, streamStdout)
		}

		return
	}

	cio.wg.Add(2)

	go cio.copyStream(cio.stdoutRead, streamStdout)
	go cio.copyStream(cio.stderrRead, streamStderr)
}

// copyStreamRaw reads raw chunks from the PTY master and fans them out to
// attach writers immediately (for interactive responsiveness), while also
// accumulating lines for the log driver. This is used for TTY mode where
// line-buffering would break interactive echo.
//
//nolint:gocognit // Raw stream copy handles multiple edge cases for TTY I/O.
func (cio *containerIO) copyStreamRaw(r *os.File, stream string) {
	defer cio.wg.Done()

	buf := make([]byte, 32*1024)

	var lineBuf []byte // accumulates partial lines for the log driver

	for {
		n, err := r.Read(buf)

		if n > 0 {
			chunk := buf[:n]

			// Send raw bytes to attach writers, or buffer if none registered yet.
			cio.fanOutOrBuffer(chunk, stream)

			// Accumulate into lines for the log driver.
			lineBuf = append(lineBuf, chunk...)

			for {
				idx := bytes.IndexByte(lineBuf, '\n')
				if idx < 0 {
					break
				}

				line := lineBuf[:idx+1]

				msg := &clog.Message{
					Stream:    stream,
					Line:      line,
					Timestamp: time.Now(),
				}

				if logErr := cio.driver.Log(msg); logErr != nil {
					slog.Error("log driver write error", "stream", stream, "err", logErr)
				}

				lineBuf = lineBuf[idx+1:]
			}
		}

		if err != nil {
			// Flush any remaining partial line to the log driver.
			if len(lineBuf) > 0 {
				lineBuf = append(lineBuf, '\n')

				msg := &clog.Message{
					Stream:    stream,
					Line:      lineBuf,
					Timestamp: time.Now(),
				}

				if logErr := cio.driver.Log(msg); logErr != nil {
					slog.Error("log driver write error", "stream", stream, "err", logErr)
				}
			}

			if !errors.Is(err, io.EOF) {
				slog.Debug("pty read ended", "stream", stream, "err", err)
			}

			break
		}
	}
}

func (cio *containerIO) copyStream(r *os.File, stream string) {
	defer cio.wg.Done()

	br := bufio.NewReaderSize(r, 64*1024)

	for {
		line, err := br.ReadBytes('\n')

		// Log any data we got, even on error (handles final partial lines on EOF).
		if len(line) > 0 {
			// If the line doesn't end with a newline (partial line at EOF), append one.
			if line[len(line)-1] != '\n' {
				line = append(line, '\n')
			}

			msg := &clog.Message{
				Stream:    stream,
				Line:      line,
				Timestamp: time.Now(),
			}

			if logErr := cio.driver.Log(msg); logErr != nil {
				slog.Error("log driver write error", "stream", stream, "err", logErr)
			}

			// Fan out to attached writers, or buffer if none registered yet.
			cio.fanOutOrBuffer(line, stream)
		}

		if err != nil {
			if !errors.Is(err, io.EOF) {
				slog.Debug("stdio read ended", "stream", stream, "err", err)
			}

			break
		}
	}
}

// fanOutOrBuffer sends data to attached writers, or buffers it if none are
// registered yet. Used by both copyStream and copyStreamRaw.
// For non-TTY containers, data is wrapped in Docker stdcopy framing so the
// Docker CLI can demultiplex stdout/stderr.
func (cio *containerIO) fanOutOrBuffer(data []byte, stream string) {
	cio.mu.Lock()
	defer cio.mu.Unlock()

	// For non-TTY, wrap with stdcopy header so Docker CLI can parse the stream.
	out := data
	if !cio.tty {
		out = stdcopyFrame(stream, data)
	}

	if len(cio.attachOut) == 0 && !cio.attachBufFlushed {
		cio.attachBuf = append(cio.attachBuf, out...)

		return
	}

	for _, w := range cio.attachOut {
		if _, wErr := w.Write(out); wErr != nil {
			slog.Debug("attach writer error", "stream", stream, "err", wErr)
		}
	}
}

// stdcopyFrame wraps data in a Docker stdcopy multiplexed frame.
// Format: [stream_type, 0, 0, 0, size(4 bytes big-endian)] + payload.
func stdcopyFrame(stream string, data []byte) []byte {
	streamType := byte(1) // stdout
	if stream == streamStderr {
		streamType = 2
	}

	frame := make([]byte, 8+len(data))
	frame[0] = streamType
	binary.BigEndian.PutUint32(frame[4:8], uint32(len(data))) //nolint:gosec // Line length is bounded by scanner buffer size.
	copy(frame[8:], data)

	return frame
}

// WriterFds returns the file descriptors to pass to crun as stdout and stderr.
// Returns (nil, nil) for TTY mode since crun uses the console-socket instead.
func (cio *containerIO) WriterFds() (*os.File, *os.File) {
	return cio.stdout, cio.stderr
}

// StdinFd returns the read end of the stdin pipe to pass to crun.
// Returns nil if stdin was not requested.
func (cio *containerIO) StdinFd() *os.File {
	return cio.stdinRead
}

// MarkWritersClosed records that the write-end FDs were closed by the caller
// (typically after crun create, since the container holds its own references).
func (cio *containerIO) MarkWritersClosed() {
	cio.writersClosed = true
}

// AddAttachWriter registers a writer that will receive container output.
// For non-TTY containers, output is automatically wrapped with stdcopy framing.
// If there's buffered early output, it's flushed to the new writer.
func (cio *containerIO) AddAttachWriter(w io.Writer) {
	cio.mu.Lock()
	defer cio.mu.Unlock()

	// Replay any buffered output from before the first writer registered.
	if len(cio.attachBuf) > 0 {
		_, _ = w.Write(cio.attachBuf)
		cio.attachBuf = nil
		cio.attachBufFlushed = true
	}

	cio.attachOut = append(cio.attachOut, w)
}

// RemoveAttachWriter deregisters an attach writer.
func (cio *containerIO) RemoveAttachWriter(w io.Writer) {
	cio.mu.Lock()
	defer cio.mu.Unlock()

	for i, aw := range cio.attachOut {
		if aw == w {
			cio.attachOut = append(cio.attachOut[:i], cio.attachOut[i+1:]...)
			break
		}
	}
}

// WriteStdin writes data to the container's stdin pipe.
// Returns an error if stdin is not available.
func (cio *containerIO) WriteStdin(p []byte) (int, error) {
	if cio.tty && cio.ptyMaster != nil {
		n, err := cio.ptyMaster.Write(p)
		if err != nil {
			return n, fmt.Errorf("write pty: %w", err)
		}

		return n, nil
	}

	if cio.stdin == nil {
		return 0, ErrStdinNotAvailable
	}

	n, err := cio.stdin.Write(p)
	if err != nil {
		return n, fmt.Errorf("write stdin: %w", err)
	}

	return n, nil
}

// CloseStdin closes the write end of the stdin pipe, delivering EOF to the
// container process. No-op if stdin is not available.
func (cio *containerIO) CloseStdin() {
	if cio.stdin != nil {
		_ = cio.stdin.Close()
		cio.stdin = nil
	}
}

// ResizePTY sets the terminal window size. No-op if not in TTY mode.
func (cio *containerIO) ResizePTY(height, width uint16) error {
	if cio.ptyMaster == nil {
		return nil
	}

	return resizePTY(cio.ptyMaster, height, width)
}

// Close closes all file descriptors, waits for copy goroutines to finish,
// and flushes the log driver.
func (cio *containerIO) Close() {
	// Close PTY master if in TTY mode.
	if cio.ptyMaster != nil {
		_ = cio.ptyMaster.Close()
	}

	// Close write ends — this will cause the readers to get EOF.
	// Skip if already closed by Start() after passing to crun.
	if !cio.writersClosed {
		if cio.stdout != nil {
			_ = cio.stdout.Close()
		}

		if cio.stderr != nil {
			_ = cio.stderr.Close()
		}
	}

	// Wait for copy goroutines to drain remaining data.
	cio.wg.Wait()

	// Close read ends.
	if cio.stdoutRead != nil {
		_ = cio.stdoutRead.Close()
	}

	if cio.stderrRead != nil {
		_ = cio.stderrRead.Close()
	}

	// Close stdin pipes.
	cio.closeStdinPipes()

	// Flush and close the log driver.
	if err := cio.driver.Close(); err != nil {
		slog.Error("log driver close error", "err", err)
	}
}

// closeStdinPipes closes both ends of the stdin pipe if they exist.
func (cio *containerIO) closeStdinPipes() {
	if cio.stdin != nil {
		_ = cio.stdin.Close()
	}

	if cio.stdinRead != nil {
		_ = cio.stdinRead.Close()
	}
}
