// Package daemon manages kogia daemon lifecycle.
package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/Shikachuu/kogia/embed"
	"github.com/Shikachuu/kogia/internal/api"
	"github.com/Shikachuu/kogia/internal/api/handlers"
	"github.com/Shikachuu/kogia/internal/image"
	"github.com/Shikachuu/kogia/internal/runtime"
	"github.com/Shikachuu/kogia/internal/store"
)

// Config holds daemon startup configuration.
// All fields are expected to be pre-validated by the CLI layer.
type Config struct {
	SocketPath       string
	RootDir          string
	StorageDriver    image.StorageDriver
	DockerAPIVersion string
	Version          string
	Commit           string
	Date             string
}

// Run starts the kogia daemon and blocks until ctx is canceled.
func Run(ctx context.Context, cfg *Config) error {
	if err := os.MkdirAll(cfg.RootDir, 0o710); err != nil {
		return fmt.Errorf("daemon: mkdir root: %w", err)
	}

	socketDir := filepath.Dir(cfg.SocketPath)

	if err := os.MkdirAll(socketDir, 0o710); err != nil {
		return fmt.Errorf("daemon: mkdir socket dir: %w", err)
	}

	pidPath := filepath.Join(socketDir, "kogia.pid")

	if err := writePID(pidPath); err != nil {
		return fmt.Errorf("daemon: write pid: %w", err)
	}

	defer func() { _ = os.Remove(pidPath) }()

	// Extract embedded crun binary.
	crunPath := filepath.Join(socketDir, "crun")

	if err := extractCrun(crunPath); err != nil {
		return fmt.Errorf("daemon: extract crun: %w", err)
	}

	defer func() { _ = os.Remove(crunPath) }()

	// Set up subreaper so we receive SIGCHLD for orphaned container processes.
	if err := runtime.SetSubreaper(); err != nil {
		slog.Warn("failed to set subreaper (container supervision may not work)", "err", err)
	}

	// Open bbolt state store.
	dbPath := filepath.Join(cfg.RootDir, "kogia.db")

	s, err := store.New(dbPath)
	if err != nil {
		return fmt.Errorf("daemon: open store: %w", err)
	}

	defer func() {
		if cerr := s.Close(); cerr != nil {
			slog.Error("failed to close store", "err", cerr)
		}
	}()

	// Open image store.
	images, err := image.NewStore(image.StoreOptions{
		GraphRoot: filepath.Join(cfg.RootDir, "image"),
		RunRoot:   filepath.Join(socketDir, "image"),
		Driver:    cfg.StorageDriver,
	})
	if err != nil {
		return fmt.Errorf("daemon: open image store: %w", err)
	}

	defer func() {
		if cerr := images.Close(); cerr != nil {
			slog.Error("failed to close image store", "err", cerr)
		}
	}()

	// Initialize runtime manager.
	crunRootDir := filepath.Join(socketDir, "crun-state")

	if mkdirErr := os.MkdirAll(crunRootDir, 0o710); mkdirErr != nil {
		return fmt.Errorf("daemon: mkdir crun root: %w", mkdirErr)
	}

	bundleRoot := filepath.Join(cfg.RootDir, "containers")

	if mkdirErr := os.MkdirAll(bundleRoot, 0o710); mkdirErr != nil {
		return fmt.Errorf("daemon: mkdir bundle root: %w", mkdirErr)
	}

	rt := runtime.NewManager(runtime.ManagerConfig{
		CrunBinary: crunPath,
		CrunRoot:   crunRootDir,
		BundleRoot: bundleRoot,
		Store:      s,
		Images:     images,
	})

	// Clean up containers orphaned by a previous crash.
	rt.RecoverOrphans(ctx)

	// Start process reaper.
	rt.StartReaper(ctx)

	// Create HTTP handlers and server.
	h := handlers.New(s, images, rt, cfg.Version, cfg.Commit, cfg.Date, cfg.DockerAPIVersion)
	srv := api.New(cfg.SocketPath, cfg.DockerAPIVersion, h)

	if err = srv.Start(); err != nil {
		return fmt.Errorf("daemon: start server: %w", err)
	}

	slog.Info("kogia daemon started",
		"socket", cfg.SocketPath,
		"root", cfg.RootDir,
		"api_version", cfg.DockerAPIVersion,
	)

	<-ctx.Done()

	slog.Info("shutting down...")

	// Stop accepting new connections.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err = srv.Shutdown(shutdownCtx); err != nil { //nolint:contextcheck // Fresh context for graceful shutdown after parent cancel.
		slog.Error("server shutdown error", "err", err)
	}

	// Stop all running containers.
	rt.Shutdown(context.Background(), 10) //nolint:contextcheck // Fresh context for graceful shutdown after parent cancel.

	return nil
}

// extractCrun writes the embedded crun binary to disk.
func extractCrun(path string) error {
	data, err := embed.Crun()
	if err != nil {
		return fmt.Errorf("read embedded crun: %w", err)
	}

	//nolint:gosec // crun binary needs to be executable.
	if writeErr := os.WriteFile(path, data, 0o755); writeErr != nil {
		return fmt.Errorf("write crun: %w", writeErr)
	}

	return nil
}

func writePID(path string) error {
	if mkdirErr := os.MkdirAll(filepath.Dir(path), 0o710); mkdirErr != nil {
		return fmt.Errorf("mkdir: %w", mkdirErr)
	}

	pid := strconv.Itoa(os.Getpid())

	//nolint:gosec // PID file needs to be readable by other processes.
	if writeErr := os.WriteFile(path, []byte(pid), 0o640); writeErr != nil {
		return fmt.Errorf("write: %w", writeErr)
	}

	return nil
}
