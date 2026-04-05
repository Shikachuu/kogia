// Package api provides the HTTP server for the Docker Engine API.
package api

import (
	"bufio"
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
		encodeSlashyPathParams(r, basePath)

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

// slashyPrefixes are API path prefixes where the {name} parameter can contain
// slashes (e.g. "docker.io/library/alpine"). Go's ServeMux {name} only matches
// one path segment, so we URL-encode the slashes in the name before dispatch,
// then PathValue("name") returns the decoded value automatically.
//
// Pattern: /<prefix>/<name-with-slashes>[/<suffix>]
// The known suffixes for each prefix are listed so we can identify where the
// name ends and the suffix begins.
var slashyRoutes = []struct {
	prefix   string
	suffixes []string
}{
	{"/images/", []string{"/json", "/history", "/push", "/tag", "/get"}},
	{"/plugins/", []string{"/json", "/enable", "/disable", "/push", "/set", "/upgrade"}},
	{"/distribution/", []string{"/json"}},
}

// encodeSlashyPathParams rewrites request paths so that image/plugin names
// containing slashes are URL-encoded into a single path segment for ServeMux
// matching. For example:
//
//	/v1.54/images/docker.io/library/alpine/json
//	→ /v1.54/images/docker.io%2Flibrary%2Falpine/json
func encodeSlashyPathParams(r *http.Request, basePath string) {
	path := r.URL.Path

	for _, route := range slashyRoutes {
		fullPrefix := basePath + route.prefix
		if !strings.HasPrefix(path, fullPrefix) {
			continue
		}

		rest := path[len(fullPrefix):]
		if rest == "" {
			continue
		}

		// Try to find a known suffix at the end of the remaining path.
		var name, suffix string

		for _, s := range route.suffixes {
			if strings.HasSuffix(rest, s) {
				name = rest[:len(rest)-len(s)]
				suffix = s

				break
			}
		}

		if name == "" {
			// No suffix matched — the entire rest is the name (e.g. DELETE /images/{name}).
			name = rest
		}

		// Only rewrite if the name actually contains slashes.
		if !strings.Contains(name, "/") {
			return
		}

		encoded := strings.ReplaceAll(name, "/", "%2F")
		r.URL.Path = fullPrefix + encoded + suffix

		return
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

// Flush delegates to the underlying ResponseWriter if it supports flushing.
// This is required for streaming endpoints (pull, push, load, events) to send
// NDJSON progress lines incrementally instead of buffering the entire response.
func (rw *responseWriter) Flush() {
	if f, ok := rw.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// errHijackUnsupported is returned when the underlying ResponseWriter does not support hijacking.
var errHijackUnsupported = errors.New("api: underlying ResponseWriter does not support hijacking")

// Hijack delegates to the underlying ResponseWriter for connection upgrades.
// Required for attach, exec, and other endpoints that upgrade to raw TCP streams.
func (rw *responseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if h, ok := rw.ResponseWriter.(http.Hijacker); ok {
		conn, buf, err := h.Hijack()
		if err != nil {
			return nil, nil, fmt.Errorf("api: hijack: %w", err)
		}

		return conn, buf, nil
	}

	return nil, nil, errHijackUnsupported
}
