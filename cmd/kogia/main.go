package main

import (
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/Shikachuu/kogia/internal/daemon"
)

// Build-time variables set via ldflags.
var (
	version          = "dev"
	commit           = "none"
	date             = "unknown"
	dockerAPIVersion = "0.0"
)

func main() {
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
		socket   string
		root     string
		logLevel string
	)

	cmd := &cobra.Command{
		Use:   "daemon",
		Short: "Start the kogia daemon",
		RunE: func(cmd *cobra.Command, _ []string) error {
			level, err := parseLogLevel(logLevel)
			if err != nil {
				return err
			}

			slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: level})))

			ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGTERM, syscall.SIGINT)
			defer stop()

			return daemon.Run(ctx, &daemon.Config{
				SocketPath:       socket,
				RootDir:          root,
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
