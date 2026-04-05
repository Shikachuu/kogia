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

	"github.com/Shikachuu/kogia/internal/api"
	"github.com/Shikachuu/kogia/internal/api/handlers"
	"github.com/Shikachuu/kogia/internal/image"
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

	h := handlers.New(s, images, cfg.Version, cfg.Commit, cfg.Date, cfg.DockerAPIVersion)
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

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err = srv.Shutdown(shutdownCtx); err != nil { //nolint:contextcheck // Fresh context for graceful shutdown after parent cancel.
		slog.Error("server shutdown error", "err", err)
	}

	return nil
}

func writePID(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o710); err != nil {
		return fmt.Errorf("mkdir: %w", err)
	}

	pid := strconv.Itoa(os.Getpid())

	//nolint:gosec // PID file needs to be readable by other processes.
	if err := os.WriteFile(path, []byte(pid), 0o640); err != nil {
		return fmt.Errorf("write: %w", err)
	}

	return nil
}
