package runtime

import (
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unsafe"

	"golang.org/x/sys/unix"
)

// startOOMWatch starts an inotify watch on the container's cgroup v2
// memory.events file. When the oom_kill count increases, it updates the
// container state in bbolt. Returns a cancel function to stop watching.
//
// The watcher runs in a goroutine and stops when cancel is called or
// the inotify fd is closed. The existing post-mortem check in reapChildren
// is kept as a fallback.
//
//nolint:gocognit // OOM watch manages inotify lifecycle with event parsing.
func (m *Manager) startOOMWatch(containerID, cgroupPath string) func() {
	eventsPath := filepath.Join(cgroupPath, "memory.events")

	// Verify the file exists before creating the inotify watch.
	if _, err := os.Stat(eventsPath); err != nil {
		slog.Debug("skipping OOM watch, memory.events not found", "path", eventsPath, "err", err)

		return func() {}
	}

	fd, err := unix.InotifyInit1(unix.IN_CLOEXEC | unix.IN_NONBLOCK)
	if err != nil {
		slog.Debug("inotify init failed, skipping OOM watch", "err", err)

		return func() {}
	}

	wd, err := unix.InotifyAddWatch(fd, eventsPath, unix.IN_MODIFY)
	if err != nil {
		_ = unix.Close(fd)

		slog.Debug("inotify add watch failed", "path", eventsPath, "err", err)

		return func() {}
	}

	slog.Debug("started OOM watch", "container", containerID[:12], "path", eventsPath)

	go func() {
		buf := make([]byte, unix.SizeofInotifyEvent+unix.PathMax+1)
		oomDetected := false

		for {
			n, readErr := unix.Read(fd, buf)
			if readErr != nil {
				// fd was closed by cancel or EAGAIN (no data available with non-blocking).
				if errors.Is(readErr, unix.EAGAIN) {
					// Use epoll to wait for events without busy-spinning.
					pollFds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}} //nolint:gosec // File descriptor fits in int32.

					_, pollErr := unix.Poll(pollFds, -1) // -1 = block indefinitely
					if pollErr != nil {
						break // fd closed or error
					}

					continue
				}

				break
			}

			if n < unix.SizeofInotifyEvent {
				continue
			}

			// Parse inotify events.
			offset := 0
			for offset < n {
				if offset+unix.SizeofInotifyEvent > n {
					break
				}

				event := (*unix.InotifyEvent)(unsafe.Pointer(&buf[offset]))
				offset += unix.SizeofInotifyEvent + int(event.Len)

				if oomDetected {
					continue // already flagged, just drain events
				}

				if checkOOMKill(eventsPath) {
					oomDetected = true

					slog.Info("OOM kill detected", "container", containerID[:12])

					// Update container state in bbolt.
					record, getErr := m.store.GetContainer(containerID)
					if getErr == nil {
						record.State.OOMKilled = true

						if updateErr := m.store.UpdateContainer(record); updateErr != nil {
							slog.Error("failed to update OOM state", "container", containerID[:12], "err", updateErr)
						}
					}
				}
			}
		}

		// Clean up watch descriptor (may fail if fd already closed, that's fine).
		_, _ = unix.InotifyRmWatch(fd, uint32(wd)) //nolint:gosec // Watch descriptor fits in uint32.
	}()

	return func() {
		// Close the fd, which causes the goroutine's Read() to return an error.
		_ = unix.Close(fd)
	}
}

// checkOOMKill reads memory.events and returns true if oom_kill > 0.
func checkOOMKill(eventsPath string) bool {
	data, err := os.ReadFile(eventsPath) //nolint:gosec // path constructed from resolved cgroup path.
	if err != nil {
		return false
	}

	for _, line := range strings.Split(string(data), "\n") {
		key, value, ok := strings.Cut(line, " ")
		if !ok || key != "oom_kill" {
			continue
		}

		count, parseErr := strconv.Atoi(value)

		return parseErr == nil && count > 0
	}

	return false
}
