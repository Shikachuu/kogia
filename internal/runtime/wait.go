//go:build linux

package runtime

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"golang.org/x/sys/unix"
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

		oomKilled := isOOMKilled(pid)

		slog.Debug("reaped child process", "pid", pid, "exitCode", exitCode, "oomKilled", oomKilled)
		m.HandleExit(pid, exitCode, oomKilled)
	}
}

// isOOMKilled checks if a process was OOM killed by reading cgroup memory events.
// This is best-effort — if we can't determine, we return false.
func isOOMKilled(pid int) bool {
	// For cgroup v2, check /proc/{pid}/cgroup to find the cgroup path,
	// then check memory.events for "oom_kill" count.
	// Since the process has already exited, we can't read /proc/{pid}.
	// The cgroup may still exist briefly. This is best-effort.
	// TODO: implement proper OOM detection via cgroup memory events.
	_ = pid

	return false
}
