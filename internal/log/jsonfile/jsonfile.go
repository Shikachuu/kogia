// Package jsonfile implements the json-file log driver for containers.
// It writes NDJSON log entries matching Docker's jsonfilelog format.
package jsonfile

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	clog "github.com/Shikachuu/kogia/internal/log"
)

// logEntry matches Docker's json-file log format.
type logEntry struct {
	Log    string `json:"log"`
	Stream string `json:"stream"`
	Time   string `json:"time"`
}

// Driver implements clog.Driver for the json-file log driver.
type Driver struct {
	file     *os.File
	logPath  string
	maxSize  int64
	maxFiles int
	mu       sync.Mutex
}

// Options configures the json-file driver.
type Options struct {
	// LogPath is the path to the primary log file (e.g., /var/lib/kogia/containers/{id}/json.log).
	LogPath string
	// MaxSize is the maximum log file size in bytes before rotation. 0 = no rotation.
	MaxSize int64
	// MaxFiles is the maximum number of rotated files to keep. 0 = 1 file only.
	MaxFiles int
}

// ParseOptions extracts json-file driver options from Docker LogConfig.Config map.
func ParseOptions(cfg map[string]string) Options {
	opts := Options{}

	if v, ok := cfg["max-size"]; ok {
		opts.MaxSize = parseSize(v)
	}

	if v, ok := cfg["max-file"]; ok {
		n, _ := strconv.Atoi(v)
		opts.MaxFiles = n
	}

	return opts
}

// New creates a new json-file log driver.
func New(opts Options) (*Driver, error) {
	dir := filepath.Dir(opts.LogPath)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("jsonfile: mkdir: %w", err)
	}

	f, err := os.OpenFile(opts.LogPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("jsonfile: open: %w", err)
	}

	return &Driver{
		file:     f,
		logPath:  opts.LogPath,
		maxSize:  opts.MaxSize,
		maxFiles: opts.MaxFiles,
	}, nil
}

// Log writes a log entry.
func (d *Driver) Log(msg *clog.Message) error {
	entry := logEntry{
		Log:    string(msg.Line),
		Stream: msg.Stream,
		Time:   msg.Timestamp.Format(time.RFC3339Nano),
	}

	data, err := json.Marshal(entry)
	if err != nil {
		return fmt.Errorf("jsonfile: marshal: %w", err)
	}

	data = append(data, '\n')

	d.mu.Lock()
	defer d.mu.Unlock()

	if d.maxSize > 0 {
		if rotateErr := d.rotateIfNeeded(int64(len(data))); rotateErr != nil {
			return rotateErr
		}
	}

	if _, writeErr := d.file.Write(data); writeErr != nil {
		return fmt.Errorf("jsonfile: write: %w", writeErr)
	}

	return nil
}

// Close flushes and closes the log file.
func (d *Driver) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()

	if err := d.file.Close(); err != nil {
		return fmt.Errorf("jsonfile: close: %w", err)
	}

	return nil
}

// ReadLogs reads log entries matching the options.
func (d *Driver) ReadLogs(opts clog.ReadOpts) (*clog.Reader, error) {
	ch := make(chan *clog.Message, 128)

	go func() {
		defer close(ch)

		d.readFiles(ch, opts)
	}()

	return &clog.Reader{
		Lines: ch,
		Close: func() {},
	}, nil
}

func (d *Driver) readFiles(ch chan<- *clog.Message, opts clog.ReadOpts) {
	d.mu.Lock()
	logPath := d.logPath
	maxFiles := d.maxFiles
	d.mu.Unlock()

	// Collect rotated files in order (oldest first).
	files := rotatedFiles(logPath, maxFiles)
	files = append(files, logPath)

	var allMessages []*clog.Message

	for _, path := range files {
		messages, err := readLogFile(path, opts)
		if err != nil {
			continue
		}

		allMessages = append(allMessages, messages...)
	}

	// Apply tail.
	if opts.Tail >= 0 && len(allMessages) > opts.Tail {
		allMessages = allMessages[len(allMessages)-opts.Tail:]
	}

	for _, msg := range allMessages {
		ch <- msg
	}
}

func readLogFile(path string, opts clog.ReadOpts) ([]*clog.Message, error) {
	f, err := os.Open(path) //nolint:gosec // Path is constructed from container log config, not user input.
	if err != nil {
		return nil, fmt.Errorf("jsonfile: open log: %w", err)
	}

	defer func() { _ = f.Close() }()

	var messages []*clog.Message

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		var entry logEntry
		if unmarshalErr := json.Unmarshal(scanner.Bytes(), &entry); unmarshalErr != nil {
			continue
		}

		ts, parseErr := time.Parse(time.RFC3339Nano, entry.Time)
		if parseErr != nil {
			continue
		}

		if !opts.Since.IsZero() && ts.Before(opts.Since) {
			continue
		}

		if !opts.Until.IsZero() && ts.After(opts.Until) {
			continue
		}

		if entry.Stream == "stdout" && !opts.Stdout {
			continue
		}

		if entry.Stream == "stderr" && !opts.Stderr {
			continue
		}

		messages = append(messages, &clog.Message{
			Stream:    entry.Stream,
			Line:      []byte(entry.Log),
			Timestamp: ts,
		})
	}

	return messages, nil
}

func (d *Driver) rotateIfNeeded(writeSize int64) error {
	info, err := d.file.Stat()
	if err != nil {
		return fmt.Errorf("jsonfile: stat: %w", err)
	}

	if info.Size()+writeSize <= d.maxSize {
		return nil
	}

	// Close current file.
	if closeErr := d.file.Close(); closeErr != nil {
		return fmt.Errorf("jsonfile: close for rotation: %w", closeErr)
	}

	// Rotate files: json.log.2 → json.log.3, json.log.1 → json.log.2, json.log → json.log.1
	maxRotated := d.maxFiles
	if maxRotated <= 0 {
		maxRotated = 1
	}

	// Remove oldest if at limit.
	oldest := d.logPath + "." + strconv.Itoa(maxRotated)
	_ = os.Remove(oldest)

	// Shift existing rotated files.
	for i := maxRotated - 1; i >= 1; i-- {
		src := d.logPath + "." + strconv.Itoa(i)
		dst := d.logPath + "." + strconv.Itoa(i+1)
		_ = os.Rename(src, dst)
	}

	// Rename current to .1
	if renameErr := os.Rename(d.logPath, d.logPath+".1"); renameErr != nil {
		return fmt.Errorf("jsonfile: rename for rotation: %w", renameErr)
	}

	// Open new log file.
	f, err := os.OpenFile(d.logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("jsonfile: open after rotation: %w", err)
	}

	d.file = f

	return nil
}

// rotatedFiles returns paths to rotated log files in order (oldest first).
func rotatedFiles(logPath string, maxFiles int) []string {
	if maxFiles <= 0 {
		maxFiles = 1
	}

	var files []string

	for i := 1; i <= maxFiles; i++ {
		path := logPath + "." + strconv.Itoa(i)
		if _, err := os.Stat(path); err == nil {
			files = append(files, path)
		}
	}

	// Sort by number descending (oldest = highest number → first).
	sort.Slice(files, func(i, j int) bool {
		ni := extractRotationNumber(files[i])
		nj := extractRotationNumber(files[j])

		return ni > nj
	})

	return files
}

func extractRotationNumber(path string) int {
	parts := strings.Split(path, ".")
	if len(parts) < 2 {
		return 0
	}

	n, _ := strconv.Atoi(parts[len(parts)-1])

	return n
}

// parseSize parses Docker-style size strings (e.g., "10m", "1g", "500k").
func parseSize(s string) int64 {
	s = strings.TrimSpace(strings.ToLower(s))
	if s == "" || s == "0" || s == "-1" {
		return 0
	}

	multiplier := int64(1)

	switch {
	case strings.HasSuffix(s, "k"):
		multiplier = 1024
		s = s[:len(s)-1]
	case strings.HasSuffix(s, "m"):
		multiplier = 1024 * 1024
		s = s[:len(s)-1]
	case strings.HasSuffix(s, "g"):
		multiplier = 1024 * 1024 * 1024
		s = s[:len(s)-1]
	}

	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0
	}

	return n * multiplier
}

// Ensure Driver implements clog.Driver at compile time.
var _ clog.Driver = (*Driver)(nil)

// NewFromLogConfig creates a Driver from the container's log path and Docker LogConfig.
func NewFromLogConfig(logPath string, cfg map[string]string) (*Driver, error) {
	opts := ParseOptions(cfg)
	opts.LogPath = logPath

	return New(opts)
}

// ReadLogsFrom reads logs from a specific path with the given options.
// This is a convenience function for reading logs without a running driver.
func ReadLogsFrom(logPath string, maxFiles int, opts clog.ReadOpts) (*clog.Reader, error) {
	d := &Driver{
		logPath:  logPath,
		maxFiles: maxFiles,
	}

	// Open the file temporarily just to check it exists.
	f, err := os.Open(logPath) //nolint:gosec // Path is from container config, not user input.
	if err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("jsonfile: open for read: %w", err)
	}

	if f != nil {
		_ = f.Close()
	}

	return d.ReadLogs(opts)
}

// Flush is a convenience to sync the log file to disk.
func (d *Driver) Flush() error {
	d.mu.Lock()
	defer d.mu.Unlock()

	if err := d.file.Sync(); err != nil {
		return fmt.Errorf("jsonfile: sync: %w", err)
	}

	return nil
}

// Path returns the path to the primary log file.
func (d *Driver) Path() string {
	return d.logPath
}

// ReaderFromPath creates a Reader that reads from an existing log file path.
// Used by the logs endpoint when the container's driver is not running.
func ReaderFromPath(logPath string, maxFiles int) io.ReadCloser {
	r, w := io.Pipe()

	go func() {
		defer func() { _ = w.Close() }()

		files := rotatedFiles(logPath, maxFiles)
		files = append(files, logPath)

		for _, path := range files {
			f, err := os.Open(path) //nolint:gosec // Path is from container config, not user input.
			if err != nil {
				continue
			}

			_, _ = io.Copy(w, f)
			_ = f.Close()
		}
	}()

	return r
}
