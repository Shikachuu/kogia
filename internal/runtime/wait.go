//go:build linux

package runtime

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

var (
	// errNoCgroupV2 is returned when /proc/{pid}/cgroup has no cgroup v2 entry.
	errNoCgroupV2 = errors.New("no cgroup v2 entry found")
	// errEmptyBundleDir is returned when an empty bundle dir is passed.
	errEmptyBundleDir = errors.New("empty bundle dir")
	// errMalformedExitCode is returned when the exitcode file has invalid format.
	errMalformedExitCode = errors.New("malformed exitcode file")
)

// SetSubreaper sets the current process as a subreaper so that orphaned
// child processes (e.g., container PIDs after crun exits) are reparented to us.
// Must be called early in daemon startup, before any containers are started.
func SetSubreaper() error {
	if err := unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0); err != nil {
		return fmt.Errorf("set subreaper: %w", err)
	}

	return nil
}

// StartReaper starts a goroutine that handles SIGCHLD signals by reaping
// all exited child processes and dispatching exit events to the Manager.
// It runs until ctx is canceled.
func (m *Manager) StartReaper(ctx context.Context) {
	sigCh := make(chan os.Signal, 32)
	signal.Notify(sigCh, syscall.SIGCHLD)

	go func() {
		defer signal.Stop(sigCh)

		for {
			select {
			case <-ctx.Done():
				// Drain any remaining children.
				m.reapChildren() //nolint:contextcheck // reapChildren uses syscall.Wait4, no context needed.

				return
			case <-sigCh:
				m.reapChildren() //nolint:contextcheck // reapChildren uses syscall.Wait4, no context needed.
			}
		}
	}()
}

// reapChildren calls Wait4 in a loop to collect all exited children.
func (m *Manager) reapChildren() {
	for {
		var ws syscall.WaitStatus

		pid, err := syscall.Wait4(-1, &ws, syscall.WNOHANG, nil)
		if pid <= 0 || err != nil {
			break
		}

		exitCode := 0
		if ws.Exited() {
			exitCode = ws.ExitStatus()
		} else if ws.Signaled() {
			// Process killed by signal: exit code = 128 + signal number.
			exitCode = 128 + int(ws.Signal())
		}

		var oomKilled bool

		var bundleDir, cgroupPath string

		m.mu.Lock()
		id, ok := m.pidMap[pid]

		if ok {
			if ac := m.active[id]; ac != nil {
				bundleDir = ac.bundleDir
				cgroupPath = ac.cgroupPath
			}
		}
		m.mu.Unlock()

		if ok && cgroupPath != "" {
			oomKilled = isOOMKilled(cgroupPath)
		}

		// Persist exit status to file before HandleExit so RecoverOrphans
		// can read the real exit code if the daemon crashes mid-update.
		if bundleDir != "" {
			writeExitCodeFile(bundleDir, exitCode, oomKilled)
		}

		slog.Debug("reaped child process", "pid", pid, "exitCode", exitCode, "oomKilled", oomKilled)
		m.HandleExit(pid, exitCode, oomKilled)
	}
}

// isOOMKilled checks whether a container was OOM-killed by reading its
// cgroup v2 memory.events file. The cgroup directory may still exist
// briefly after the process exits. This is best-effort — if the file
// is already gone or unparseable, we return false.
func isOOMKilled(cgroupPath string) bool {
	data, err := os.ReadFile(cgroupPath + "/memory.events") //nolint:gosec // path is constructed from resolved cgroup path.
	if err != nil {
		return false
	}

	for line := range strings.SplitSeq(string(data), "\n") {
		key, value, ok := strings.Cut(line, " ")
		if !ok || key != "oom_kill" {
			continue
		}

		count, parseErr := strconv.Atoi(value)

		return parseErr == nil && count > 0
	}

	return false
}

// resolveCgroupPath reads /proc/{pid}/cgroup to find the container's cgroup v2 path.
// Returns the full sysfs path (e.g., /sys/fs/cgroup/kogia/<id>).
func resolveCgroupPath(pid int) (string, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/cgroup", pid))
	if err != nil {
		return "", fmt.Errorf("read cgroup file: %w", err)
	}

	for line := range strings.SplitSeq(string(data), "\n") {
		// cgroup v2 unified hierarchy line: "0::<path>"
		if !strings.HasPrefix(line, "0::") {
			continue
		}

		relPath := strings.TrimPrefix(line, "0::")

		return "/sys/fs/cgroup" + relPath, nil
	}

	return "", fmt.Errorf("%w in /proc/%d/cgroup", errNoCgroupV2, pid)
}

const exitCodeFileName = "exitcode"

// writeExitCodeFile persists the exit code and OOM status to a file in the
// bundle directory. This is best-effort — used by RecoverOrphans to read
// real exit codes after a daemon crash.
func writeExitCodeFile(bundleDir string, exitCode int, oomKilled bool) {
	content := strconv.Itoa(exitCode) + "\n" + strconv.FormatBool(oomKilled) + "\n"
	path := filepath.Join(bundleDir, exitCodeFileName)

	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		slog.Warn("failed to write exitcode file", "path", path, "err", err)
	}
}

// readExitCodeFile reads a previously written exitcode file.
// Returns an error if the file is missing or malformed.
func readExitCodeFile(bundleDir string) (exitCode int, oomKilled bool, err error) {
	if bundleDir == "" {
		return 0, false, errEmptyBundleDir
	}

	data, err := os.ReadFile(filepath.Join(bundleDir, exitCodeFileName)) //nolint:gosec // path is constructed from trusted bundle dir.
	if err != nil {
		return 0, false, fmt.Errorf("read exitcode file: %w", err)
	}

	lines := strings.SplitN(string(data), "\n", 3) //nolint:mnd // exactly 2 data lines + optional trailing empty.
	if len(lines) < 2 {
		return 0, false, fmt.Errorf("%w: expected 2 lines", errMalformedExitCode)
	}

	exitCode, err = strconv.Atoi(lines[0])
	if err != nil {
		return 0, false, fmt.Errorf("parse exit code: %w", err)
	}

	oomKilled, err = strconv.ParseBool(lines[1])
	if err != nil {
		return 0, false, fmt.Errorf("parse oom killed: %w", err)
	}

	return exitCode, oomKilled, nil
}
