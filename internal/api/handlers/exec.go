package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"github.com/Shikachuu/kogia/internal/api/errdefs"
	"github.com/Shikachuu/kogia/internal/runtime"
	"github.com/moby/moby/api/types/container"
)

func isExecNotFound(err error) bool {
	return errors.Is(err, runtime.ErrExecNotFound)
}

// ContainerExec handles POST /containers/{id}/exec — creates an exec instance.
func (h *Handlers) ContainerExec(w http.ResponseWriter, r *http.Request) {
	id := pathValue(r, "id")

	var req container.ExecCreateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, errdefs.InvalidParameter("invalid exec config", err))

		return
	}

	if len(req.Cmd) == 0 {
		respondError(w, errdefs.InvalidParameter("exec requires at least one command", nil))

		return
	}

	execID, err := h.runtime.ExecCreate(r.Context(), id, req)
	if err != nil {
		if isNotFound(err) {
			respondError(w, errdefs.NotFound("no such container: "+id, err))

			return
		}

		respondError(w, err)

		return
	}

	respondJSON(w, http.StatusCreated, container.ExecCreateResponse{ID: execID})
}

// ExecStart handles POST /exec/{id}/start — starts an exec instance.
func (h *Handlers) ExecStart(w http.ResponseWriter, r *http.Request) {
	execID := pathValue(r, "id")

	var req container.ExecStartRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, errdefs.InvalidParameter("invalid exec start config", err))

		return
	}

	if req.Detach {
		// Detached exec: start in background, respond immediately.
		go func() {
			_ = h.runtime.ExecStart(context.Background(), execID, &runtime.ExecStartOpts{
				Detach: true,
				TTY:    req.Tty,
			})
		}()

		w.WriteHeader(http.StatusOK)

		return
	}

	// Interactive exec: hijack the connection.
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		respondError(w, ErrHijackUnsupported)

		return
	}

	conn, buf, err := hijacker.Hijack()
	if err != nil {
		respondError(w, fmt.Errorf("hijack: %w", err))

		return
	}

	_, _ = buf.WriteString("HTTP/1.1 101 UPGRADED\r\n")
	_, _ = buf.WriteString("Content-Type: application/vnd.docker.raw-stream\r\n")
	_, _ = buf.WriteString("Connection: Upgrade\r\n")
	_, _ = buf.WriteString("Upgrade: tcp\r\n")
	_, _ = buf.WriteString("\r\n")
	_ = buf.Flush()

	// Use detached context — r.Context() is cancelled after hijack.
	_ = h.runtime.ExecStart(context.Background(), execID, &runtime.ExecStartOpts{
		Conn: conn,
		TTY:  req.Tty,
	})

	_ = conn.Close()
}

// ExecInspect handles GET /exec/{id}/json.
func (h *Handlers) ExecInspect(w http.ResponseWriter, r *http.Request) {
	execID := pathValue(r, "id")

	resp, err := h.runtime.ExecInspect(r.Context(), execID)
	if err != nil {
		if isExecNotFound(err) {
			respondError(w, errdefs.NotFound("no such exec instance: "+execID, err))

			return
		}

		respondError(w, err)

		return
	}

	respondJSON(w, http.StatusOK, resp)
}

// ExecResize handles POST /exec/{id}/resize.
func (h *Handlers) ExecResize(w http.ResponseWriter, r *http.Request) {
	execID := pathValue(r, "id")

	height, err := strconv.Atoi(r.URL.Query().Get("h"))
	if err != nil {
		respondError(w, errdefs.InvalidParameter("invalid height", err))

		return
	}

	width, err := strconv.Atoi(r.URL.Query().Get("w"))
	if err != nil {
		respondError(w, errdefs.InvalidParameter("invalid width", err))

		return
	}

	if resizeErr := h.runtime.ExecResize(r.Context(), execID, uint16(height), uint16(width)); resizeErr != nil {
		if isExecNotFound(resizeErr) {
			respondError(w, errdefs.NotFound("no such exec instance: "+execID, resizeErr))

			return
		}

		respondError(w, resizeErr)

		return
	}

	w.WriteHeader(http.StatusOK)
}
