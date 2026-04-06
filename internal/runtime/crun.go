// Package runtime manages container lifecycle via the crun OCI runtime.
package runtime

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
)

// CrunConfig configures the crun binary location and state directory.
type CrunConfig struct {
	// BinaryPath is the path to the crun binary.
	BinaryPath string
	// RootDir is the crun --root state directory (e.g., /run/kogia/crun/).
	RootDir string
}

// run executes a crun command with the configured --root flag.
// On non-zero exit, the error includes crun's stderr for diagnostics.
func (c *CrunConfig) run(ctx context.Context, args ...string) error {
	fullArgs := append([]string{"--root", c.RootDir}, args...)

	slog.Debug("crun exec", "args", fullArgs)

	cmd := exec.CommandContext(ctx, c.BinaryPath, fullArgs...) //nolint:gosec // BinaryPath is the embedded crun binary, not user input.

	var stderr bytes.Buffer

	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("crun %s: %w: %s", args[0], err, stderr.String())
	}

	return nil
}

// createWithIO runs `crun create` passing the container's stdout/stderr pipes
// as inherited FDs. The double-forked container process inherits these FDs,
// so it can write to our pipes even after crun itself exits.
//
// IMPORTANT: Both stdout and stderr MUST be *os.File (not io.Writer wrappers).
// If cmd.Stdout/Stderr is a non-File io.Writer, Go creates an internal pipe +
// copy goroutine. The double-forked container inherits the internal pipe FD,
// and cmd.Wait() blocks forever waiting for EOF that never comes.
// createWithIO runs `crun create` passing the container's stdio pipes as
// inherited FDs. The double-forked container process inherits these FDs,
// so it can write to our pipes even after crun itself exits.
//
// IMPORTANT: stdin, stdout, and stderr MUST be *os.File (not io.Writer wrappers).
// If cmd.Stdout/Stderr is a non-File io.Writer, Go creates an internal pipe +
// copy goroutine. The double-forked container inherits the internal pipe FD,
// and cmd.Wait() blocks forever waiting for EOF that never comes.
// stdin may be nil if the container does not accept input.
func (c *CrunConfig) createWithIO(ctx context.Context, id, bundleDir, pidFile string, stdin, stdout, stderr *os.File) error {
	fullArgs := []string{
		"--root", c.RootDir,
		"create",
		"--bundle", bundleDir,
		"--pid-file", pidFile,
		"--no-new-keyring",
		id,
	}

	slog.Debug("crun exec", "args", fullArgs)

	cmd := exec.CommandContext(ctx, c.BinaryPath, fullArgs...) //nolint:gosec // BinaryPath is the embedded crun binary, not user input.

	// Pass *os.File directly — Go passes the FD to the child without creating
	// internal pipes. The container process (double-forked by crun) inherits
	// these FDs.
	if stdin != nil {
		cmd.Stdin = stdin
	}

	cmd.Stdout = stdout
	cmd.Stderr = stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("crun create: %w", err)
	}

	return nil
}

// createWithConsole runs `crun create` with --console-socket for TTY containers.
// crun connects to the console socket and sends the PTY master fd via SCM_RIGHTS.
// No stdout/stderr pipes are needed — the container's stdio goes through the PTY.
func (c *CrunConfig) createWithConsole(ctx context.Context, id, bundleDir, pidFile, consoleSock string) error {
	fullArgs := []string{
		"--root", c.RootDir,
		"create",
		"--bundle", bundleDir,
		"--pid-file", pidFile,
		"--console-socket", consoleSock,
		"--no-new-keyring",
		id,
	}

	slog.Debug("crun exec", "args", fullArgs)

	cmd := exec.CommandContext(ctx, c.BinaryPath, fullArgs...) //nolint:gosec // BinaryPath is the embedded crun binary, not user input.

	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("crun create (console): %w: %s", err, stderr.String())
	}

	return nil
}

// execCmd returns an unstarted *exec.Cmd for `crun exec --process=<file> <id>`.
// The caller manages stdio assignment and lifecycle (Start/Wait).
func (c *CrunConfig) execCmd(ctx context.Context, id, processFile string) *exec.Cmd {
	fullArgs := []string{
		"--root", c.RootDir,
		"exec",
		"--process", processFile,
		id,
	}

	slog.Debug("crun exec cmd", "args", fullArgs)

	return exec.CommandContext(ctx, c.BinaryPath, fullArgs...) //nolint:gosec // BinaryPath is the embedded crun binary, not user input.
}

// execWithConsole returns an unstarted *exec.Cmd for `crun exec` with --console-socket.
func (c *CrunConfig) execWithConsole(ctx context.Context, id, processFile, consoleSock string) *exec.Cmd {
	fullArgs := []string{
		"--root", c.RootDir,
		"exec",
		"--process", processFile,
		"--console-socket", consoleSock,
		id,
	}

	slog.Debug("crun exec cmd (console)", "args", fullArgs)

	return exec.CommandContext(ctx, c.BinaryPath, fullArgs...) //nolint:gosec // BinaryPath is the embedded crun binary, not user input.
}

// start runs `crun start` to begin execution of the container's process.
func (c *CrunConfig) start(ctx context.Context, id string) error {
	return c.run(ctx, "start", id)
}

// kill sends a signal to the container's init process.
func (c *CrunConfig) kill(ctx context.Context, id, signal string) error {
	return c.run(ctx, "kill", id, signal)
}

// killAll sends a signal to all processes in the container's cgroup.
// Used by Stop() to ensure child processes also receive the signal.
func (c *CrunConfig) killAll(ctx context.Context, id, signal string) error {
	return c.run(ctx, "kill", "--all", id, signal)
}

// deleteContainer runs `crun delete --force` to clean up container state.
func (c *CrunConfig) deleteContainer(ctx context.Context, id string) error {
	return c.run(ctx, "delete", "--force", id)
}

// pause freezes all processes in the container.
func (c *CrunConfig) pause(ctx context.Context, id string) error {
	return c.run(ctx, "pause", id)
}

// resume unfreezes a paused container.
func (c *CrunConfig) resume(ctx context.Context, id string) error {
	return c.run(ctx, "resume", id)
}
