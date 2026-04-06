package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sync"

	"github.com/moby/moby/api/types/container"
	ocispec "github.com/opencontainers/runtime-spec/specs-go"
)

var (
	// ErrExecNotFound is returned when an exec session ID is unknown.
	ErrExecNotFound = errors.New("exec not found")
	// ErrExecAlreadyStarted is returned when ExecStart is called twice.
	ErrExecAlreadyStarted = errors.New("exec already started")
)

// ExecSession tracks state for a single exec instance.
type ExecSession struct {
	mu          sync.Mutex
	ID          string
	ContainerID string
	Config      container.ExecCreateRequest
	Running     bool
	ExitCode    *int
	Pid         int
	ptyMaster   *os.File // for resize, nil if non-TTY
	started     bool     // true after ExecStart called
}

// ExecStartOpts configures an exec start session.
type ExecStartOpts struct {
	Conn   io.ReadWriteCloser
	Detach bool
	TTY    bool
}

// ExecCreate creates a new exec instance for the given container.
func (m *Manager) ExecCreate(ctx context.Context, containerID string, req container.ExecCreateRequest) (string, error) {
	record, err := m.store.GetContainer(containerID)
	if err != nil {
		return "", fmt.Errorf("runtime: exec create: %w", err)
	}

	if record.State.Status != container.StateRunning {
		return "", fmt.Errorf("runtime: exec create: %w: %s", ErrNotRunning, record.ID[:12])
	}

	execID, err := generateID()
	if err != nil {
		return "", fmt.Errorf("runtime: exec create: %w", err)
	}

	session := &ExecSession{
		ID:          execID,
		ContainerID: record.ID,
		Config:      req,
	}

	m.execMu.Lock()
	m.execSessions[execID] = session
	m.execMu.Unlock()

	slog.Debug("exec created", "execID", execID[:12], "container", record.ID[:12], "cmd", req.Cmd)

	return execID, nil
}

// ExecStart runs the exec session. For non-detached sessions, it blocks until
// the exec process exits. The caller provides a hijacked connection for I/O.
func (m *Manager) ExecStart(ctx context.Context, execID string, opts *ExecStartOpts) error {
	m.execMu.Lock()
	session := m.execSessions[execID]
	m.execMu.Unlock()

	if session == nil {
		return fmt.Errorf("runtime: exec start: %w: %s", ErrExecNotFound, execID[:12])
	}

	session.mu.Lock()
	if session.started {
		session.mu.Unlock()
		return fmt.Errorf("runtime: exec start: %w: %s", ErrExecAlreadyStarted, execID[:12])
	}

	session.started = true
	session.mu.Unlock()

	// Look up container's active state for bundle dir.
	m.mu.Lock()
	ac := m.active[session.ContainerID]
	m.mu.Unlock()

	if ac == nil {
		return fmt.Errorf("runtime: exec start: %w: %s", ErrNotRunning, session.ContainerID[:12])
	}

	// Build OCI process spec for exec.
	processFile := filepath.Join(ac.bundleDir, "exec-"+execID[:12]+".json")

	if err := m.writeExecProcessSpec(processFile, session, ac); err != nil {
		return fmt.Errorf("runtime: exec start: %w", err)
	}

	defer os.Remove(processFile)

	if session.Config.Tty {
		return m.execWithTTY(ctx, session, ac, processFile, opts)
	}

	return m.execWithPipes(ctx, session, ac, processFile, opts)
}

// execWithTTY runs an exec session with a PTY (console-socket flow).
func (m *Manager) execWithTTY(ctx context.Context, session *ExecSession, ac *activeContainer, processFile string, opts *ExecStartOpts) error {
	consoleSock := filepath.Join(ac.bundleDir, "exec-console-"+session.ID[:12]+".sock")

	type ptyResult struct {
		file *os.File
		err  error
	}

	ptyCh := make(chan ptyResult, 1)

	go func() {
		pty, err := ReceivePTYMaster(consoleSock, CrunOperationTimeout)
		ptyCh <- ptyResult{pty, err}
	}()

	cmd := m.crun.execWithConsole(ctx, session.ContainerID, processFile, consoleSock)

	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("crun exec start: %w: %s", err, stderr.String())
	}

	// Wait for PTY master fd.
	result := <-ptyCh
	if result.err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()

		return fmt.Errorf("receive exec pty: %w", result.err)
	}

	pty := result.file
	defer pty.Close()

	session.mu.Lock()
	session.Running = true
	session.Pid = cmd.Process.Pid
	session.ptyMaster = pty
	session.mu.Unlock()

	// Bidirectional copy between connection and PTY.
	if opts.Conn != nil && !opts.Detach {
		done := make(chan struct{}, 2)

		go func() {
			_, _ = io.Copy(opts.Conn, pty)
			done <- struct{}{}
		}()

		go func() {
			_, _ = io.Copy(pty, opts.Conn)
			done <- struct{}{}
		}()

		// Wait for cmd to finish.
		waitErr := cmd.Wait()

		<-done // wait for at least one copy to finish

		return m.finishExec(session, waitErr)
	}

	// Detached: just wait for completion.
	waitErr := cmd.Wait()

	return m.finishExec(session, waitErr)
}

// execWithPipes runs an exec session with stdin/stdout/stderr pipes.
func (m *Manager) execWithPipes(ctx context.Context, session *ExecSession, ac *activeContainer, processFile string, opts *ExecStartOpts) error {
	cmd := m.crun.execCmd(ctx, session.ContainerID, processFile)

	var stderr bytes.Buffer

	if opts.Conn != nil && !opts.Detach {
		if session.Config.AttachStdin {
			cmd.Stdin = opts.Conn
		}

		cmd.Stdout = opts.Conn
		cmd.Stderr = opts.Conn
	} else {
		cmd.Stderr = &stderr
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("crun exec start: %w: %s", err, stderr.String())
	}

	session.mu.Lock()
	session.Running = true
	session.Pid = cmd.Process.Pid
	session.mu.Unlock()

	waitErr := cmd.Wait()

	return m.finishExec(session, waitErr)
}

// finishExec records the exit code and marks the session as not running.
func (m *Manager) finishExec(session *ExecSession, waitErr error) error {
	exitCode := 0
	if waitErr != nil {
		var exitErr *exec.ExitError
		if errors.As(waitErr, &exitErr) {
			exitCode = exitErr.ExitCode()
		} else {
			exitCode = -1
		}
	}

	session.mu.Lock()
	session.Running = false
	session.ExitCode = &exitCode
	session.ptyMaster = nil
	session.mu.Unlock()

	slog.Debug("exec finished", "execID", session.ID[:12], "exitCode", exitCode)

	return nil
}

// ExecInspect returns the state of an exec session.
func (m *Manager) ExecInspect(_ context.Context, execID string) (*container.ExecInspectResponse, error) {
	m.execMu.Lock()
	session := m.execSessions[execID]
	m.execMu.Unlock()

	if session == nil {
		return nil, fmt.Errorf("runtime: exec inspect: %w: %s", ErrExecNotFound, execID)
	}

	session.mu.Lock()
	defer session.mu.Unlock()

	entrypoint := ""
	var args []string

	if len(session.Config.Cmd) > 0 {
		entrypoint = session.Config.Cmd[0]
		args = session.Config.Cmd[1:]
	}

	return &container.ExecInspectResponse{
		ID:          session.ID,
		ContainerID: session.ContainerID,
		Running:     session.Running,
		ExitCode:    session.ExitCode,
		Pid:         session.Pid,
		OpenStdin:   session.Config.AttachStdin,
		OpenStdout:  session.Config.AttachStdout,
		OpenStderr:  session.Config.AttachStderr,
		ProcessConfig: &container.ExecProcessConfig{
			Tty:        session.Config.Tty,
			Entrypoint: entrypoint,
			Arguments:  args,
			User:       session.Config.User,
		},
	}, nil
}

// ExecResize sets the terminal window size for a running exec session.
func (m *Manager) ExecResize(_ context.Context, execID string, height, width uint16) error {
	m.execMu.Lock()
	session := m.execSessions[execID]
	m.execMu.Unlock()

	if session == nil {
		return fmt.Errorf("runtime: exec resize: %w: %s", ErrExecNotFound, execID)
	}

	session.mu.Lock()
	defer session.mu.Unlock()

	if session.ptyMaster == nil {
		return nil
	}

	return resizePTY(session.ptyMaster, height, width)
}

// cleanupExecSessions marks all exec sessions for a container as finished.
// Called from HandleExit when the container exits.
func (m *Manager) cleanupExecSessions(containerID string) {
	m.execMu.Lock()
	defer m.execMu.Unlock()

	exitCode := -1

	for _, session := range m.execSessions {
		if session.ContainerID == containerID {
			session.mu.Lock()
			if session.Running {
				session.Running = false
				session.ExitCode = &exitCode
			}
			session.mu.Unlock()
		}
	}
}

// writeExecProcessSpec writes the OCI process spec for an exec session.
func (m *Manager) writeExecProcessSpec(path string, session *ExecSession, ac *activeContainer) error {
	// Read the container's OCI spec to inherit defaults.
	specPath := filepath.Join(ac.bundleDir, "config.json")

	specData, err := os.ReadFile(specPath) //nolint:gosec // Path is constructed from bundle dir.
	if err != nil {
		return fmt.Errorf("read container spec: %w", err)
	}

	var spec ocispec.Spec
	if err := json.Unmarshal(specData, &spec); err != nil {
		return fmt.Errorf("parse container spec: %w", err)
	}

	// Build exec process from container defaults + exec config.
	env := spec.Process.Env
	if len(session.Config.Env) > 0 {
		env = append(env, session.Config.Env...)
	}

	cwd := spec.Process.Cwd
	if session.Config.WorkingDir != "" {
		cwd = session.Config.WorkingDir
	}

	user := spec.Process.User
	if session.Config.User != "" {
		// TODO: resolve user from /etc/passwd like spec.go does.
		// For now, only support numeric uid.
		user = ocispec.User{}
	}

	process := &ocispec.Process{
		Terminal:     session.Config.Tty,
		Args:         session.Config.Cmd,
		Env:          env,
		Cwd:          cwd,
		User:         user,
		Capabilities: spec.Process.Capabilities,
	}

	data, err := json.Marshal(process)
	if err != nil {
		return fmt.Errorf("marshal exec process: %w", err)
	}

	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("write exec process: %w", err)
	}

	return nil
}
