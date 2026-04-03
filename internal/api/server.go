// Package api provides the HTTP server for the Docker Engine API.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/Shikachuu/kogia/internal/api/gen"
	"github.com/Shikachuu/kogia/internal/api/handlers"
)

var versionPrefixRe = regexp.MustCompile(`^/v\d+\.\d+(/|$)`)

// Server serves the Docker Engine API over a Unix socket.
type Server struct {
	listener   net.Listener
	httpServer *http.Server
	socketPath string
}

// New creates and configures a new Server.
func New(socketPath, dockerAPIVersion string, h *handlers.Handlers) *Server {
	basePath := "/v" + dockerAPIVersion
	mux := http.NewServeMux()

	gen.RegisterRoutes(mux, basePath, h)

	handler := withMiddleware(mux, basePath)

	return &Server{
		socketPath: socketPath,
		httpServer: &http.Server{
			Handler:           handler,
			ReadHeaderTimeout: 10 * time.Second,
		},
	}
}

// Start begins listening on the Unix socket.
func (s *Server) Start() error {
	_ = os.Remove(s.socketPath)

	l, err := net.Listen("unix", s.socketPath)
	if err != nil {
		return fmt.Errorf("api: listen %s: %w", s.socketPath, err)
	}

	//nolint:gosec // Socket needs group read/write for Docker CLI access.
	if err = os.Chmod(s.socketPath, 0o660); err != nil {
		_ = l.Close()

		return fmt.Errorf("api: chmod socket: %w", err)
	}

	s.listener = l

	go func() {
		if serveErr := s.httpServer.Serve(l); serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			slog.Error("api server error", "err", serveErr)
		}
	}()

	return nil
}

// Shutdown gracefully stops the server.
func (s *Server) Shutdown(ctx context.Context) error {
	if err := s.httpServer.Shutdown(ctx); err != nil {
		return fmt.Errorf("api: shutdown: %w", err)
	}

	_ = os.Remove(s.socketPath)

	return nil
}

func withMiddleware(next http.Handler, basePath string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rw := &responseWriter{ResponseWriter: w, status: http.StatusOK}

		defer func() {
			if p := recover(); p != nil {
				rw.Header().Set("Content-Type", "application/json")
				rw.WriteHeader(http.StatusInternalServerError)

				_ = json.NewEncoder(rw).Encode(map[string]string{"message": "internal server error"})

				slog.Error("panic in handler", "panic", p)
			}

			slog.Debug("api request",
				"method", r.Method,
				"path", r.URL.Path,
				"status", rw.status,
				"duration", time.Since(start),
			)
		}()

		rewriteVersionPrefix(r, basePath)

		next.ServeHTTP(rw, r)
	})
}

// rewriteVersionPrefix normalises the Docker CLI's version-prefixed paths.
// /v1.45/containers/json → /v1.54/containers/json
// /_ping → /v1.54/_ping (bare ping without version prefix).
func rewriteVersionPrefix(r *http.Request, basePath string) {
	path := r.URL.Path

	switch {
	case path == "/_ping":
		r.URL.Path = basePath + "/_ping"
	case versionPrefixRe.MatchString(path):
		idx := strings.Index(path[1:], "/")
		if idx < 0 {
			r.URL.Path = basePath + "/"
		} else {
			r.URL.Path = basePath + path[idx+1:]
		}
	}
}

type responseWriter struct {
	http.ResponseWriter
	status int
}

func (rw *responseWriter) WriteHeader(code int) {
	rw.status = code
	rw.ResponseWriter.WriteHeader(code)
}
