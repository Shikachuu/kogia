package runtime

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Shikachuu/kogia/internal/image"
	"github.com/Shikachuu/kogia/internal/log/jsonfile"
	"github.com/Shikachuu/kogia/internal/store"
	"github.com/moby/moby/api/types/container"
)

var (
	// ErrAlreadyRunning is returned when trying to start an already running container.
	ErrAlreadyRunning = errors.New("container is already running")
	// ErrNotRunning is returned when trying to kill/stop a non-running container.
	ErrNotRunning = errors.New("container is not running")
	// ErrContainerRunning is returned when trying to remove a running container without force.
	ErrContainerRunning = errors.New("container is running, use force to remove")
	// ErrNoSpecRoot is returned when the OCI spec has no root field.
	ErrNoSpecRoot = errors.New("spec has no root field")
	// ErrConfigRequired is returned when container config is nil.
	ErrConfigRequired = errors.New("config is required")
)

// Manager manages container lifecycle via crun.
type Manager struct {
	crun       *CrunConfig
	store      *store.Store
	images     *image.Store
	active     map[string]*activeContainer
	pidMap     map[int]string
	bundleRoot string
	mu         sync.Mutex
}

// activeContainer tracks a running container's ephemeral state.
type activeContainer struct {
	done           chan struct{}
	io             *containerIO
	id             string
	pid            int
	manuallyStopped bool // set by Stop() to prevent restart for unless-stopped policy
}

// ManagerConfig holds configuration for the runtime Manager.
type ManagerConfig struct {
	Store      *store.Store
	Images     *image.Store
	CrunBinary string
	CrunRoot   string
	BundleRoot string
}

// NewManager creates a new container runtime manager.
func NewManager(cfg ManagerConfig) *Manager {
	return &Manager{
		crun: &CrunConfig{
			BinaryPath: cfg.CrunBinary,
			RootDir:    cfg.CrunRoot,
		},
		store:      cfg.Store,
		images:     cfg.Images,
		bundleRoot: cfg.BundleRoot,
		active:     make(map[string]*activeContainer),
		pidMap:     make(map[int]string),
	}
}

// RecoverOrphans cleans up containers left in "running" or "created" state
// from a previous daemon crash. Should be called once at startup before
// accepting API requests.
func (m *Manager) RecoverOrphans(ctx context.Context) {
	all, err := m.store.ListContainers(&store.ContainerFilters{All: true})
	if err != nil {
		slog.Error("failed to list containers for orphan recovery", "err", err)

		return
	}

	for _, record := range all {
		if record.State == nil {
			continue
		}

		switch record.State.Status {
		case container.StateRunning, container.StateRestarting:
			// Container was "running" but daemon crashed — mark as exited.
			slog.Info("recovering orphaned container", "id", record.ID[:12], "status", record.State.Status)

			// Try to kill/delete any leftover crun state.
			_ = m.crun.kill(ctx, record.ID, DefaultKillSignal)
			_ = m.crun.deleteContainer(ctx, record.ID)

			record.State.Status = container.StateExited
			record.State.Running = false
			record.State.Restarting = false
			record.State.Pid = 0
			record.State.ExitCode = 137 // Killed.
			record.State.FinishedAt = time.Now().UTC().Format(time.RFC3339Nano)
			record.State.Error = "daemon crashed while container was running"

			if updateErr := m.store.UpdateContainer(record); updateErr != nil {
				slog.Error("failed to update orphaned container", "id", record.ID[:12], "err", updateErr)
			}

		case container.StateExited, container.StateDead, container.StatePaused, container.StateRemoving:
			// Already exited or in a terminal state — no action needed.

		case container.StateCreated:
			// Container was created but never started — clean up entirely.
			slog.Info("cleaning up orphaned created container", "id", record.ID[:12])

			_ = m.crun.deleteContainer(ctx, record.ID)

			rawStore := m.images.RawStore()
			_, _ = rawStore.Unmount(record.ID, true)
			_ = rawStore.DeleteContainer(record.ID)

			bundleDir, _ := m.store.GetContainerBundle(record.ID)
			if bundleDir != "" {
				_ = os.RemoveAll(bundleDir)
			}

			_ = m.store.DeleteContainer(record.ID, record.Name)
		}
	}
}

// Create creates a new container without starting it.
func (m *Manager) Create(_ context.Context, cfg *container.Config, hostCfg *container.HostConfig, name string) (string, error) {
	id, err := generateID()
	if err != nil {
		return "", fmt.Errorf("runtime: generate id: %w", err)
	}

	if name == "" {
		name = m.generateUniqueName()
	}

	// Ensure name has leading slash (Docker convention).
	if !strings.HasPrefix(name, "/") {
		name = "/" + name
	}

	// Resolve image.
	imgInspect, err := m.images.Get(cfg.Image)
	if err != nil {
		return "", fmt.Errorf("runtime: resolve image %q: %w", cfg.Image, err)
	}

	imgCfg, err := m.images.GetConfig(cfg.Image)
	if err != nil {
		return "", fmt.Errorf("runtime: get image config: %w", err)
	}

	// Create RW layer via containers/storage.
	rawStore := m.images.RawStore()

	storageContainer, err := rawStore.CreateContainer(id, nil, imgInspect.ID, "", "", nil)
	if err != nil {
		return "", fmt.Errorf("runtime: create storage container: %w", err)
	}

	// Mount rootfs.
	rootPath, err := rawStore.Mount(storageContainer.ID, "")
	if err != nil {
		_ = rawStore.DeleteContainer(storageContainer.ID)

		return "", fmt.Errorf("runtime: mount rootfs: %w", err)
	}

	// Create bundle directory.
	bundleDir := filepath.Join(m.bundleRoot, id)

	if mkdirErr := os.MkdirAll(bundleDir, 0o750); mkdirErr != nil {
		_, _ = rawStore.Unmount(storageContainer.ID, true)
		_ = rawStore.DeleteContainer(storageContainer.ID)

		return "", fmt.Errorf("runtime: mkdir bundle: %w", mkdirErr)
	}

	record, err := m.buildContainerRecord(id, name, cfg, hostCfg, imgCfg, imgInspect.ID, bundleDir, rootPath)
	if err != nil {
		_ = os.RemoveAll(bundleDir)

		_, _ = rawStore.Unmount(storageContainer.ID, true)
		_ = rawStore.DeleteContainer(storageContainer.ID)

		return "", err
	}

	// Persist to bbolt.
	if storeErr := m.store.CreateContainer(record); storeErr != nil {
		_ = os.RemoveAll(bundleDir)

		_, _ = rawStore.Unmount(storageContainer.ID, true)
		_ = rawStore.DeleteContainer(storageContainer.ID)

		return "", fmt.Errorf("runtime: persist container: %w", storeErr)
	}

	if bundleErr := m.store.SetContainerBundle(id, bundleDir); bundleErr != nil {
		_ = m.store.DeleteContainer(id, name)
		_ = os.RemoveAll(bundleDir)

		_, _ = rawStore.Unmount(storageContainer.ID, true)
		_ = rawStore.DeleteContainer(storageContainer.ID)

		return "", fmt.Errorf("runtime: persist bundle path: %w", bundleErr)
	}

	slog.Info("container created", "id", id[:12], "name", name, "image", cfg.Image)

	return id, nil
}

// buildContainerRecord generates the OCI spec, writes it to disk, and builds an InspectResponse.
func (m *Manager) buildContainerRecord(
	id, name string,
	cfg *container.Config,
	hostCfg *container.HostConfig,
	imgCfg *image.Config,
	imageID, bundleDir, rootPath string,
) (*container.InspectResponse, error) {
	hostname := id[:12]

	// Generate OCI spec.
	spec, err := GenerateSpec(&SpecOpts{
		Config:          cfg,
		HostConfig:      hostCfg,
		ImageEnv:        imgCfg.Env,
		ImageEntrypoint: imgCfg.Entrypoint,
		ImageCmd:        imgCfg.Cmd,
		ImageCwd:        imgCfg.WorkingDir,
		ImageUser:       imgCfg.User,
		RootPath:        rootPath,
		Hostname:        hostname,
	})
	if err != nil {
		return nil, fmt.Errorf("runtime: generate spec: %w", err)
	}

	// Write config.json.
	specJSON, err := json.Marshal(spec)
	if err != nil {
		return nil, fmt.Errorf("runtime: marshal spec: %w", err)
	}

	configPath := filepath.Join(bundleDir, "config.json")

	if writeErr := os.WriteFile(configPath, specJSON, 0o600); writeErr != nil {
		return nil, fmt.Errorf("runtime: write config.json: %w", writeErr)
	}

	// Build the args string for Path/Args fields.
	args := BuildArgs(&SpecOpts{
		Config:          cfg,
		ImageEntrypoint: imgCfg.Entrypoint,
		ImageCmd:        imgCfg.Cmd,
	})

	path := ""

	var cmdArgs []string

	if len(args) > 0 {
		path = args[0]
		cmdArgs = args[1:]
	}

	now := time.Now().UTC().Format(time.RFC3339Nano)

	return &container.InspectResponse{
		ID:      id,
		Created: now,
		Path:    path,
		Args:    cmdArgs,
		State: &container.State{
			Status:     container.StateCreated,
			Running:    false,
			Pid:        0,
			ExitCode:   0,
			StartedAt:  "",
			FinishedAt: "",
		},
		Image:          imageID,
		Name:           name,
		Driver:         "overlay",
		Platform:       "linux",
		LogPath:        filepath.Join(bundleDir, "json.log"),
		Config:         cfg,
		HostConfig:     hostCfg,
		ResolvConfPath: filepath.Join(bundleDir, "resolv.conf"),
		HostnamePath:   filepath.Join(bundleDir, "hostname"),
		HostsPath:      filepath.Join(bundleDir, "hosts"),
	}, nil
}

// Start starts a created or stopped container.
func (m *Manager) Start(ctx context.Context, id string) error {
	record, err := m.store.GetContainer(id)
	if err != nil {
		return fmt.Errorf("runtime: %w", err)
	}

	if record.State.Status == container.StateRunning {
		return fmt.Errorf("runtime: %w: %s", ErrAlreadyRunning, id[:12])
	}

	bundleDir, err := m.store.GetContainerBundle(record.ID)
	if err != nil {
		return fmt.Errorf("runtime: %w", err)
	}

	// Ensure rootfs is mounted.
	rawStore := m.images.RawStore()

	rootPath, err := rawStore.Mount(record.ID, "")
	if err != nil {
		return fmt.Errorf("runtime: mount rootfs: %w", err)
	}

	// Update spec root path in case it changed after remount.
	if specErr := m.updateSpecRootPath(bundleDir, rootPath); specErr != nil {
		return fmt.Errorf("runtime: update spec root: %w", specErr)
	}

	// Create log driver.
	logCfg := make(map[string]string)
	if record.HostConfig != nil && record.HostConfig.LogConfig.Config != nil {
		logCfg = record.HostConfig.LogConfig.Config
	}

	logDriver, err := jsonfile.NewFromLogConfig(record.LogPath, logCfg)
	if err != nil {
		return fmt.Errorf("runtime: create log driver: %w", err)
	}

	// Create stdio pipes.
	cio, err := newContainerIO(logDriver)
	if err != nil {
		_ = logDriver.Close()

		return fmt.Errorf("runtime: create stdio: %w", err)
	}

	// Start copy goroutines BEFORE crun create. This ensures the pipe read-ends
	// are being drained, preventing deadlock if crun or the container writes to
	// stdout/stderr during creation.
	//
	// GOROUTINE LEAK GUARD: Every error path after this point MUST call
	// cio.Close() to close the write-end pipes, allowing the copy goroutines
	// to receive EOF and exit. Without this, the goroutines block forever on
	// read. No standard Go linter detects goroutine leaks statically; use
	// goleak (go.uber.org/goleak) in tests to catch them at runtime.
	cio.startCopyLoop()

	// crun create — pass container stdout/stderr pipe write-ends so the
	// double-forked container process inherits them.
	pidFile := filepath.Join(bundleDir, "container.pid")
	stdoutW, stderrW := cio.WriterFds()

	slog.Debug("crun create starting", "id", record.ID[:12], "bundle", bundleDir, "root", rootPath)

	createCtx, createCancel := context.WithTimeout(ctx, CrunOperationTimeout)
	defer createCancel()

	if createErr := m.crun.createWithIO(createCtx, record.ID, bundleDir, pidFile, stdoutW, stderrW); createErr != nil {
		slog.Error("crun create failed", "id", record.ID[:12], "err", createErr)

		cio.Close()

		return fmt.Errorf("runtime: crun create: %w", createErr)
	}

	slog.Debug("crun create succeeded", "id", record.ID[:12])

	// Close our copy of the write-ends. The container process (double-forked
	// by crun) holds its own references. If we don't close these, our read
	// goroutines will never get EOF because the write-end refcount stays > 0.
	_ = stdoutW.Close()
	_ = stderrW.Close()

	cio.MarkWritersClosed()

	// Read PID from pid-file. crun create writes this (it's the container
	// init PID), so it's available before crun start.
	pid, err := readPIDFile(pidFile)
	if err != nil {
		_ = m.crun.deleteContainer(ctx, record.ID)

		cio.Close()

		return fmt.Errorf("runtime: read pid: %w", err)
	}

	// Register in active map + pidMap BEFORE crun start. This prevents a
	// race where the container exits instantly (e.g., hello-world) and the
	// reaper processes SIGCHLD before we register the PID.
	m.mu.Lock()
	ac := &activeContainer{
		pid:  pid,
		id:   record.ID,
		done: make(chan struct{}),
		io:   cio,
	}
	m.active[record.ID] = ac
	m.pidMap[pid] = record.ID
	m.mu.Unlock()

	// Update state in bbolt.
	now := time.Now().UTC().Format(time.RFC3339Nano)
	record.State.Status = container.StateRunning
	record.State.Running = true
	record.State.Pid = pid
	record.State.StartedAt = now
	record.State.FinishedAt = ""

	if updateErr := m.store.UpdateContainer(record); updateErr != nil {
		slog.Error("failed to update container state", "id", record.ID[:12], "err", updateErr)
	}

	// crun start — signals the container to exec. The PID is already
	// registered so the reaper can handle exit immediately.
	slog.Debug("crun start", "id", record.ID[:12], "pid", pid)

	startCtx, startCancel := context.WithTimeout(ctx, CrunOperationTimeout)
	defer startCancel()

	if startErr := m.crun.start(startCtx, record.ID); startErr != nil {
		slog.Error("crun start failed", "id", record.ID[:12], "err", startErr)

		// Unregister from active map.
		m.mu.Lock()
		delete(m.active, record.ID)
		delete(m.pidMap, pid)
		m.mu.Unlock()

		_ = m.crun.deleteContainer(ctx, record.ID)

		cio.Close()

		return fmt.Errorf("runtime: crun start: %w", startErr)
	}

	slog.Info("container started", "id", record.ID[:12], "pid", pid)

	return nil
}

// Stop sends SIGTERM, waits for timeout, then SIGKILL.
func (m *Manager) Stop(ctx context.Context, id string, timeout int) error {
	record, err := m.store.GetContainer(id)
	if err != nil {
		return fmt.Errorf("runtime: %w", err)
	}

	if !record.State.Running {
		return nil // Already stopped.
	}

	if timeout <= 0 {
		timeout = DefaultStopTimeout
	}

	// Mark as manually stopped so unless-stopped policy doesn't restart.
	m.mu.Lock()
	if ac := m.active[record.ID]; ac != nil {
		ac.manuallyStopped = true
	}
	m.mu.Unlock()

	// Send SIGTERM.
	termCtx, termCancel := context.WithTimeout(ctx, CrunOperationTimeout)
	defer termCancel()

	if killErr := m.crun.kill(termCtx, record.ID, DefaultStopSignal); killErr != nil {
		slog.Debug("crun kill SIGTERM failed (container may have already exited)", "id", record.ID[:12], "err", killErr)
	}

	// Wait for exit or timeout.
	m.mu.Lock()
	ac := m.active[record.ID]
	m.mu.Unlock()

	if ac != nil {
		select {
		case <-ac.done:
			return nil
		case <-time.After(time.Duration(timeout) * time.Second):
		}

		// Send SIGKILL.
		sigkillCtx, sigkillCancel := context.WithTimeout(ctx, CrunOperationTimeout)
		defer sigkillCancel()

		if sigkillErr := m.crun.kill(sigkillCtx, record.ID, DefaultKillSignal); sigkillErr != nil {
			slog.Debug("crun kill SIGKILL failed", "id", record.ID[:12], "err", sigkillErr)
		}

		// Wait for the reaper to collect.
		select {
		case <-ac.done:
		case <-time.After(5 * time.Second):
			slog.Warn("timeout waiting for container to die after SIGKILL", "id", record.ID[:12])
		}
	}

	// Clean up crun state.
	_ = m.crun.deleteContainer(ctx, record.ID)

	return nil
}

// Kill sends a signal to a running container.
func (m *Manager) Kill(ctx context.Context, id, signal string) error {
	record, err := m.store.GetContainer(id)
	if err != nil {
		return fmt.Errorf("runtime: %w", err)
	}

	if !record.State.Running {
		return fmt.Errorf("runtime: %w: %s", ErrNotRunning, id)
	}

	if signal == "" {
		signal = DefaultKillSignal
	}

	killCtx, killCancel := context.WithTimeout(ctx, CrunOperationTimeout)
	defer killCancel()

	return m.crun.kill(killCtx, record.ID, signal)
}

// Remove removes a container.
func (m *Manager) Remove(ctx context.Context, id string, force bool) error {
	record, err := m.store.GetContainer(id)
	if err != nil {
		return fmt.Errorf("runtime: %w", err)
	}

	if record.State.Running {
		if !force {
			return fmt.Errorf("runtime: %w: %s", ErrContainerRunning, id[:12])
		}

		if stopErr := m.Stop(ctx, record.ID, DefaultStopTimeout); stopErr != nil {
			slog.Warn("failed to stop container during force remove", "id", record.ID[:12], "err", stopErr)
		}
	}

	// Clean up crun state.
	delCtx, delCancel := context.WithTimeout(ctx, CrunOperationTimeout)
	defer delCancel()

	_ = m.crun.deleteContainer(delCtx, record.ID)

	// Unmount and delete storage container.
	rawStore := m.images.RawStore()
	_, _ = rawStore.Unmount(record.ID, true)
	_ = rawStore.DeleteContainer(record.ID)

	// Remove bundle directory.
	bundleDir, _ := m.store.GetContainerBundle(record.ID)
	if bundleDir != "" {
		_ = os.RemoveAll(bundleDir)
	}

	// Remove from bbolt.
	if delErr := m.store.DeleteContainer(record.ID, record.Name); delErr != nil {
		return fmt.Errorf("runtime: delete from store: %w", delErr)
	}

	slog.Info("container removed", "id", record.ID[:12])

	return nil
}

// Restart stops and starts a container.
func (m *Manager) Restart(ctx context.Context, id string, timeout int) error {
	if err := m.Stop(ctx, id, timeout); err != nil {
		return err
	}

	return m.Start(ctx, id)
}

// Wait blocks until the container exits and returns the exit code.
// The Docker CLI sends wait before start, so we must handle "created" state
// by blocking until the container eventually exits.
func (m *Manager) Wait(ctx context.Context, id string) (int, error) {
	record, err := m.store.GetContainer(id)
	if err != nil {
		return -1, fmt.Errorf("runtime: %w", err)
	}

	// Only return immediately if the container has already exited/died.
	if record.State.Status == container.StateExited || record.State.Status == container.StateDead {
		return record.State.ExitCode, nil
	}

	// For "created" or "running" containers, we need to wait for exit.
	// Try to find an active entry; if not yet started, poll until one appears.
	for {
		m.mu.Lock()
		ac := m.active[record.ID]
		m.mu.Unlock()

		if ac != nil {
			// Container is running — block on the done channel.
			select {
			case <-ac.done:
				record, err = m.store.GetContainer(record.ID)
				if err != nil {
					return -1, fmt.Errorf("runtime: re-read container: %w", err)
				}

				return record.State.ExitCode, nil
			case <-ctx.Done():
				return -1, fmt.Errorf("runtime: wait canceled: %w", ctx.Err())
			}
		}

		// Container not yet in active map (still "created", not started yet).
		// Re-check state — it may have started and already exited.
		record, err = m.store.GetContainer(record.ID)
		if err != nil {
			return -1, fmt.Errorf("runtime: re-read container: %w", err)
		}

		if record.State.Status == container.StateExited || record.State.Status == container.StateDead {
			return record.State.ExitCode, nil
		}

		// Brief sleep before polling again — avoids busy-wait while waiting
		// for start to be called.
		select {
		case <-time.After(WaitPollInterval):
		case <-ctx.Done():
			return -1, fmt.Errorf("runtime: wait canceled: %w", ctx.Err())
		}
	}
}

// Inspect returns the full container inspect response.
func (m *Manager) Inspect(_ context.Context, id string) (*container.InspectResponse, error) {
	resp, err := m.store.GetContainer(id)
	if err != nil {
		return nil, fmt.Errorf("runtime: inspect: %w", err)
	}

	return resp, nil
}

// List returns containers matching filters.
func (m *Manager) List(_ context.Context, filters *store.ContainerFilters) ([]*container.InspectResponse, error) {
	results, err := m.store.ListContainers(filters)
	if err != nil {
		return nil, fmt.Errorf("runtime: list: %w", err)
	}

	return results, nil
}

// HandleExit is called by the reaper when a child process exits.
func (m *Manager) HandleExit(pid, exitCode int, oomKilled bool) {
	m.mu.Lock()

	containerID, ok := m.pidMap[pid]
	if !ok {
		m.mu.Unlock()

		return
	}

	ac := m.active[containerID]
	delete(m.pidMap, pid)
	delete(m.active, containerID)
	m.mu.Unlock()

	slog.Info("container exited", "id", containerID[:12], "pid", pid, "exitCode", exitCode)

	// Close stdio and log driver.
	if ac != nil && ac.io != nil {
		ac.io.Close()
	}

	// Update state in bbolt.
	record, err := m.store.GetContainer(containerID)
	if err != nil {
		slog.Error("failed to get container for exit update", "id", containerID[:12], "err", err)

		if ac != nil {
			close(ac.done)
		}

		return
	}

	now := time.Now().UTC().Format(time.RFC3339Nano)
	record.State.Status = container.StateExited
	record.State.Running = false
	record.State.Pid = 0
	record.State.ExitCode = exitCode
	record.State.FinishedAt = now
	record.State.OOMKilled = oomKilled

	if updateErr := m.store.UpdateContainer(record); updateErr != nil {
		slog.Error("failed to update container exit state", "id", containerID[:12], "err", updateErr)
	}

	// Signal waiters.
	if ac != nil {
		close(ac.done)
	}

	// Handle auto-remove.
	if record.HostConfig != nil && record.HostConfig.AutoRemove {
		go func() {
			if rmErr := m.Remove(context.Background(), containerID, true); rmErr != nil {
				slog.Error("auto-remove failed", "id", containerID[:12], "err", rmErr)
			}
		}()

		return
	}

	// Handle restart policy.
	wasManuallyStopped := ac != nil && ac.manuallyStopped
	m.handleRestartPolicy(record, exitCode, wasManuallyStopped)
}

func (m *Manager) handleRestartPolicy(record *container.InspectResponse, exitCode int, manuallyStopped bool) {
	if record.HostConfig == nil {
		return
	}

	policy := record.HostConfig.RestartPolicy

	var shouldRestart bool

	switch policy.Name {
	case container.RestartPolicyAlways:
		shouldRestart = !manuallyStopped
	case container.RestartPolicyOnFailure:
		shouldRestart = exitCode != 0 && !manuallyStopped &&
			(policy.MaximumRetryCount <= 0 || record.RestartCount < policy.MaximumRetryCount)
	case container.RestartPolicyUnlessStopped:
		shouldRestart = !manuallyStopped
	default:
		return
	}

	if !shouldRestart {
		return
	}

	// Increment restart count.
	record.RestartCount++
	record.State.Status = container.StateRestarting
	record.State.Restarting = true
	_ = m.store.UpdateContainer(record)

	// Exponential backoff: RestartBackoffBase * RestartBackoffMultiplier^(restartCount-1), capped at RestartBackoffMax.
	delay := RestartBackoffBase
	for i := 1; i < record.RestartCount && delay < RestartBackoffMax; i++ {
		delay *= RestartBackoffMultiplier
	}

	if delay > RestartBackoffMax {
		delay = RestartBackoffMax
	}

	go func() {
		time.Sleep(delay)

		slog.Info("restarting container", "id", record.ID[:12], "attempt", record.RestartCount, "delay", delay)

		if err := m.Start(context.Background(), record.ID); err != nil {
			slog.Error("restart failed", "id", record.ID[:12], "err", err)

			// Mark as exited if restart fails.
			record.State.Status = container.StateExited
			record.State.Restarting = false
			_ = m.store.UpdateContainer(record)
		}
	}()
}

// ActiveContainers returns the IDs of all currently running containers.
func (m *Manager) ActiveContainers() []string {
	m.mu.Lock()
	defer m.mu.Unlock()

	ids := make([]string, 0, len(m.active))
	for id := range m.active {
		ids = append(ids, id)
	}

	return ids
}

// Shutdown stops all running containers gracefully.
func (m *Manager) Shutdown(ctx context.Context, timeout int) {
	ids := m.ActiveContainers()
	if len(ids) == 0 {
		return
	}

	slog.Info("stopping all containers", "count", len(ids))

	var wg sync.WaitGroup

	for _, id := range ids {
		wg.Add(1)

		go func(containerID string) {
			defer wg.Done()

			if err := m.Stop(ctx, containerID, timeout); err != nil {
				slog.Error("failed to stop container during shutdown", "id", containerID[:12], "err", err)
			}

			// Clean up crun state.
			_ = m.crun.deleteContainer(ctx, containerID)
		}(id)
	}

	wg.Wait()
}

func (m *Manager) updateSpecRootPath(bundleDir, rootPath string) error {
	configPath := filepath.Join(bundleDir, "config.json")

	data, err := os.ReadFile(configPath) //nolint:gosec // Path is constructed from bundle dir, not user input.
	if err != nil {
		return fmt.Errorf("read spec: %w", err)
	}

	var spec map[string]any

	if unmarshalErr := json.Unmarshal(data, &spec); unmarshalErr != nil {
		return fmt.Errorf("unmarshal spec: %w", unmarshalErr)
	}

	root, ok := spec["root"].(map[string]any)
	if !ok {
		return ErrNoSpecRoot
	}

	root["path"] = rootPath

	data, err = json.Marshal(spec)
	if err != nil {
		return fmt.Errorf("marshal spec: %w", err)
	}

	if writeErr := os.WriteFile(configPath, data, 0o600); writeErr != nil {
		return fmt.Errorf("write spec: %w", writeErr)
	}

	return nil
}

func (m *Manager) generateUniqueName() string {
	for range 10 {
		name := generateName()

		exists, err := m.store.ContainerNameExists("/" + name)
		if err != nil {
			slog.Error("failed to check container name existence", "name", name, "err", err)

			break
		}

		if !exists {
			return name
		}
	}

	// Fallback: use a random hex string.
	b := make([]byte, 8)
	rand.Read(b)

	return "container_" + hex.EncodeToString(b)
}

func generateID() (string, error) {
	b := make([]byte, ContainerIDBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate id: %w", err)
	}

	return hex.EncodeToString(b), nil
}

func readPIDFile(path string) (int, error) {
	data, err := os.ReadFile(path) //nolint:gosec // Path is constructed from bundle dir, not user input.
	if err != nil {
		return 0, fmt.Errorf("read pid file: %w", err)
	}

	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return 0, fmt.Errorf("parse pid: %w", err)
	}

	return pid, nil
}
