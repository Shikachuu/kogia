package handlers

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"runtime"

	"github.com/Shikachuu/kogia/internal/api/stream"
	"github.com/Shikachuu/kogia/internal/events"
	"github.com/moby/moby/api/types/system"
	"golang.org/x/sys/unix"

	"github.com/Shikachuu/kogia/internal/store"
)

const daemonIDKey = "daemon-id"

// SystemPing handles GET /_ping.
func (h *Handlers) SystemPing(w http.ResponseWriter, _ *http.Request) {
	h.writePingHeaders(w)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("OK"))
}

// SystemPingHead handles HEAD /_ping.
func (h *Handlers) SystemPingHead(w http.ResponseWriter, _ *http.Request) {
	h.writePingHeaders(w)
	w.WriteHeader(http.StatusOK)
}

func (h *Handlers) writePingHeaders(w http.ResponseWriter) {
	w.Header().Set("Api-Version", h.dockerAPIVersion)
	w.Header().Set("Docker-Experimental", "false")
	w.Header().Set("OSType", "linux")
	w.Header().Set("Server", "kogia")
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
}

// SystemVersion handles GET /version.
func (h *Handlers) SystemVersion(w http.ResponseWriter, _ *http.Request) {
	resp := system.VersionResponse{
		Version:       h.version,
		APIVersion:    h.dockerAPIVersion,
		MinAPIVersion: "1.25",
		GitCommit:     h.commit,
		GoVersion:     runtime.Version(),
		Os:            runtime.GOOS,
		Arch:          runtime.GOARCH,
		KernelVersion: kernelVersion(),
		BuildTime:     h.date,
	}

	respondJSON(w, http.StatusOK, resp)
}

// SystemInfo handles GET /info.
func (h *Handlers) SystemInfo(w http.ResponseWriter, _ *http.Request) {
	id, err := daemonID(h.store)
	if err != nil {
		slog.Error("failed to get daemon ID", "err", err)

		id = "unknown"
	}

	hostname, _ := os.Hostname()

	resp := system.Info{
		ID:            id,
		Name:          hostname,
		ServerVersion: h.version,
		OSType:        "linux",
		Architecture:  runtime.GOARCH,
		Driver:        "overlay2",
	}

	respondJSON(w, http.StatusOK, resp)
}

// SystemEvents handles GET /events — streams NDJSON lifecycle events.
func (h *Handlers) SystemEvents(w http.ResponseWriter, r *http.Request) {
	filters := events.ParseFilters(r.URL.Query())
	sub := h.events.Subscribe(filters)

	defer sub.Close()

	nw := stream.NewNDJSON(w)

	for {
		select {
		case msg, ok := <-sub.C:
			if !ok {
				return
			}

			if err := nw.Encode(msg); err != nil {
				return
			}
		case <-r.Context().Done():
			return
		}
	}
}

func kernelVersion() string {
	var uname unix.Utsname

	if err := unix.Uname(&uname); err != nil {
		return "unknown"
	}

	release := make([]byte, 0, len(uname.Release))

	for _, c := range uname.Release {
		if c == 0 {
			break
		}

		release = append(release, c)
	}

	return string(release)
}

func daemonID(s *store.Store) (string, error) {
	const bucket = "meta"

	id, err := s.Get(bucket, daemonIDKey)
	if err == nil {
		return string(id), nil
	}

	if !errors.Is(err, store.ErrNotFound) {
		return "", fmt.Errorf("get daemon ID: %w", err)
	}

	raw := make([]byte, 16)
	if _, err = rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate daemon ID: %w", err)
	}

	newID := hex.EncodeToString(raw)

	if err = s.Put(bucket, daemonIDKey, []byte(newID)); err != nil {
		return "", fmt.Errorf("store daemon ID: %w", err)
	}

	return newID, nil
}
