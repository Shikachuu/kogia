// Package log defines the container log driver interface.
package log

import "time"

// Message represents a single log entry from a container.
type Message struct {
	Timestamp time.Time
	Stream    string
	Line      []byte
}

// ReadOpts controls which log entries to return.
type ReadOpts struct {
	Since  time.Time
	Until  time.Time
	Tail   int
	Stdout bool
	Stderr bool
}

// Reader provides access to log entries.
type Reader struct {
	// Lines receives log messages. Closed when reading is complete.
	Lines <-chan *Message
	// Err holds any error encountered during reading (check after Lines is closed).
	Err error
	// Close cancels reading and releases resources.
	Close func()
}

// Driver is the interface for container log drivers.
type Driver interface {
	// Log writes a log entry.
	Log(msg *Message) error
	// Close flushes and closes the driver.
	Close() error
	// ReadLogs returns log entries matching the given options.
	ReadLogs(opts ReadOpts) (*Reader, error)
}
