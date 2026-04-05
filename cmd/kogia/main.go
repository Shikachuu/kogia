package main

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/Shikachuu/kogia/internal/daemon"
	"github.com/Shikachuu/kogia/internal/image"
)

// Build-time variables set via ldflags.
var (
	version          = "dev"
	commit           = "none"
	date             = "unknown"
	dockerAPIVersion = "0.0"
)

func main() {
	if handleReexec() {
		return
	}

	if err := rootCmd().Execute(); err != nil {
		os.Exit(1)
	}
}

func rootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "kogia",
		Short: "Lightweight Docker-compatible container runtime",
	}

	root.AddCommand(daemonCmd())

	return root
}

func daemonCmd() *cobra.Command {
	var (
		socket        string
		root          string
		logLevel      string
		storageDriver string
	)

	// Validated in PreRunE, used in RunE.
	var (
		parsedLogLevel      slog.Level
		parsedStorageDriver image.StorageDriver
	)

	cmd := &cobra.Command{
		Use:   "daemon",
		Short: "Start the kogia daemon",
		PreRunE: func(_ *cobra.Command, _ []string) error {
			if socket == "" {
				return errors.New("--socket must not be empty")
			}

			if root == "" {
				return errors.New("--root must not be empty")
			}

			// Resolve to absolute paths so the daemon works regardless of cwd.
			var err error

			socket, err = filepath.Abs(socket)
			if err != nil {
				return fmt.Errorf("resolving --socket path: %w", err)
			}

			root, err = filepath.Abs(root)
			if err != nil {
				return fmt.Errorf("resolving --root path: %w", err)
			}

			level, err := parseLogLevel(logLevel)
			if err != nil {
				return err
			}

			parsedLogLevel = level

			driver, err := image.ParseStorageDriver(storageDriver)
			if err != nil {
				return err
			}

			parsedStorageDriver = driver

			return nil
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: parsedLogLevel})))

			ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGTERM, syscall.SIGINT)
			defer stop()

			return daemon.Run(ctx, &daemon.Config{
				SocketPath:       socket,
				RootDir:          root,
				StorageDriver:    parsedStorageDriver,
				DockerAPIVersion: dockerAPIVersion,
				Version:          version,
				Commit:           commit,
				Date:             date,
			})
		},
	}

	cmd.Flags().StringVar(&socket, "socket", "/run/kogia.sock", "Unix socket path")
	cmd.Flags().StringVar(&root, "root", "/var/lib/kogia", "Root data directory")
	cmd.Flags().StringVar(&logLevel, "log-level", "info", "Log level (debug, info, warn, error)")
	cmd.Flags().StringVar(&storageDriver, "storage-driver", "overlay", "Storage driver (overlay, vfs, fuse-overlayfs)")

	return cmd
}

func parseLogLevel(s string) (slog.Level, error) {
	switch s {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return slog.LevelInfo, fmt.Errorf("unknown log level %q", s)
	}
}
