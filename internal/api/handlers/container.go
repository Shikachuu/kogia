package handlers

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Shikachuu/kogia/internal/api/errdefs"
	"github.com/Shikachuu/kogia/internal/image"
	clog "github.com/Shikachuu/kogia/internal/log"
	"github.com/Shikachuu/kogia/internal/log/jsonfile"
	"github.com/Shikachuu/kogia/internal/runtime"
	"github.com/Shikachuu/kogia/internal/store"
	"github.com/moby/moby/api/types/container"
)

// isNotFound returns true if the error chain contains store.ErrNotFound.
func isNotFound(err error) bool {
	return errors.Is(err, store.ErrNotFound)
}

// ErrHijackUnsupported is returned when the HTTP server does not support connection hijacking.
var ErrHijackUnsupported = errors.New("webserver does not support hijacking")

const queryValueTrue = "true"

// ContainerCreate handles POST /containers/create.
func (h *Handlers) ContainerCreate(w http.ResponseWriter, r *http.Request) {
	var req container.CreateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, errdefs.InvalidParameter("invalid request body", err))

		return
	}

	name := r.URL.Query().Get("name")

	if name != "" {
		if err := validateContainerName(name); err != nil {
			respondError(w, err)

			return
		}
	}

	if err := validateContainerConfig(req.Config); err != nil {
		respondError(w, err)

		return
	}

	if req.HostConfig != nil {
		if err := validateHostConfig(req.HostConfig); err != nil {
			respondError(w, err)

			return
		}
	}

	id, err := h.runtime.Create(r.Context(), req.Config, req.HostConfig, name)
	if err != nil {
		switch {
		case errors.Is(err, store.ErrNameInUse):
			respondError(w, errdefs.Conflict("container name already in use", err))
		case errors.Is(err, store.ErrNotFound), errors.Is(err, image.ErrNotFound):
			respondError(w, errdefs.NotFound("no such image: "+req.Image, err))
		default:
			respondError(w, err)
		}

		return
	}

	respondJSON(w, http.StatusCreated, container.CreateResponse{
		ID:       id,
		Warnings: []string{},
	})
}

// ContainerStart handles POST /containers/{id}/start.
func (h *Handlers) ContainerStart(w http.ResponseWriter, r *http.Request) {
	id := pathValue(r, "id")

	err := h.runtime.Start(r.Context(), id)
	if err != nil {
		if errors.Is(err, runtime.ErrAlreadyRunning) {
			w.WriteHeader(http.StatusNotModified)

			return
		}

		if isNotFound(err) {
			respondError(w, errdefs.NotFound("no such container: "+id, err))

			return
		}

		respondError(w, err)

		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// ContainerStop handles POST /containers/{id}/stop.
func (h *Handlers) ContainerStop(w http.ResponseWriter, r *http.Request) {
	id := pathValue(r, "id")

	timeout, err := validateTimeout(r.URL.Query().Get("t"), runtime.DefaultStopTimeout)
	if err != nil {
		respondError(w, err)

		return
	}

	if stopErr := h.runtime.Stop(r.Context(), id, timeout); stopErr != nil {
		if isNotFound(stopErr) {
			respondError(w, errdefs.NotFound("no such container: "+id, stopErr))

			return
		}

		respondError(w, stopErr)

		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// ContainerKill handles POST /containers/{id}/kill.
func (h *Handlers) ContainerKill(w http.ResponseWriter, r *http.Request) {
	id := pathValue(r, "id")
	signal := r.URL.Query().Get("signal")

	if signal == "" {
		signal = runtime.DefaultKillSignal
	}

	if err := validateSignal(signal); err != nil {
		respondError(w, err)

		return
	}

	if err := h.runtime.Kill(r.Context(), id, signal); err != nil {
		if errors.Is(err, runtime.ErrNotRunning) {
			respondError(w, errdefs.Conflict("container is not running", err))

			return
		}

		if isNotFound(err) {
			respondError(w, errdefs.NotFound("no such container: "+id, err))

			return
		}

		respondError(w, err)

		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// ContainerRestart handles POST /containers/{id}/restart.
func (h *Handlers) ContainerRestart(w http.ResponseWriter, r *http.Request) {
	id := pathValue(r, "id")

	timeout, err := validateTimeout(r.URL.Query().Get("t"), runtime.DefaultStopTimeout)
	if err != nil {
		respondError(w, err)

		return
	}

	if restartErr := h.runtime.Restart(r.Context(), id, timeout); restartErr != nil {
		if isNotFound(restartErr) {
			respondError(w, errdefs.NotFound("no such container: "+id, restartErr))

			return
		}

		respondError(w, restartErr)

		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// ContainerWait handles POST /containers/{id}/wait.
// Docker CLI sends wait before start. We must flush the 200 status immediately
// so the HTTP connection is unblocked, then write the JSON body only when the
// container exits. This allows the CLI to pipeline start on another connection.
func (h *Handlers) ContainerWait(w http.ResponseWriter, r *http.Request) {
	id := pathValue(r, "id")

	// Verify container exists before committing to streaming response.
	if _, err := h.runtime.Inspect(r.Context(), id); err != nil {
		if isNotFound(err) {
			respondError(w, errdefs.NotFound("no such container: "+id, err))

			return
		}

		respondError(w, err)

		return
	}

	// Send 200 OK + headers immediately, then keep connection open.
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)

	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}

	// Block until container exits.
	exitCode, err := h.runtime.Wait(r.Context(), id)
	if err != nil {
		// Can't change status code after headers are sent — write error in body.
		// Use a generic message for non-errdefs errors to prevent leaking internals.
		msg := errdefs.SafeMessage(err)
		if errdefs.StatusCode(err) == http.StatusInternalServerError {
			msg = "internal server error"
		}

		_ = json.NewEncoder(w).Encode(container.WaitResponse{
			StatusCode: -1,
			Error:      &container.WaitExitError{Message: msg},
		})

		return
	}

	_ = json.NewEncoder(w).Encode(container.WaitResponse{
		StatusCode: int64(exitCode),
	})
}

// ContainerDelete handles DELETE /containers/{id}.
func (h *Handlers) ContainerDelete(w http.ResponseWriter, r *http.Request) {
	id := pathValue(r, "id")
	force := r.URL.Query().Get("force") == "1" || r.URL.Query().Get("force") == queryValueTrue

	if err := h.runtime.Remove(r.Context(), id, force); err != nil {
		if isNotFound(err) {
			respondError(w, errdefs.NotFound("no such container: "+id, err))

			return
		}

		if errors.Is(err, runtime.ErrContainerRunning) {
			respondError(w, errdefs.Conflict("container is running, use force to remove", err))

			return
		}

		respondError(w, err)

		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// ContainerList handles GET /containers/json.
func (h *Handlers) ContainerList(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	filters := store.ContainerFilters{
		All: q.Get("all") == "1" || q.Get("all") == queryValueTrue,
	}

	if v := q.Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			filters.Limit = n
		}
	}

	// Parse Docker-style JSON filters.
	if raw := q.Get("filters"); raw != "" {
		var f map[string][]string
		if err := json.Unmarshal([]byte(raw), &f); err != nil {
			respondError(w, errdefs.InvalidParameter("invalid filters JSON", err))

			return
		}

		filters.ID = f["id"]
		filters.Name = f["name"]
		filters.Status = f["status"]
		filters.Label = f["label"]
		filters.Ancestor = f["ancestor"]
	}

	records, err := h.runtime.List(r.Context(), &filters)
	if err != nil {
		respondError(w, err)

		return
	}

	summaries := make([]*container.Summary, 0, len(records))
	for _, rec := range records {
		summaries = append(summaries, inspectToSummary(rec))
	}

	respondJSON(w, http.StatusOK, summaries)
}

// ContainerInspect handles GET /containers/{id}/json.
func (h *Handlers) ContainerInspect(w http.ResponseWriter, r *http.Request) {
	id := pathValue(r, "id")

	resp, err := h.runtime.Inspect(r.Context(), id)
	if err != nil {
		if isNotFound(err) {
			respondError(w, errdefs.NotFound("no such container: "+id, err))

			return
		}

		respondError(w, err)

		return
	}

	respondJSON(w, http.StatusOK, resp)
}

// ContainerLogs handles GET /containers/{id}/logs.
func (h *Handlers) ContainerLogs(w http.ResponseWriter, r *http.Request) {
	id := pathValue(r, "id")

	record, err := h.runtime.Inspect(r.Context(), id)
	if err != nil {
		if isNotFound(err) {
			respondError(w, errdefs.NotFound("no such container: "+id, err))

			return
		}

		respondError(w, err)

		return
	}

	opts, err := parseLogOpts(r)
	if err != nil {
		respondError(w, err)

		return
	}

	maxFiles := logMaxFiles(record)

	reader, err := jsonfile.ReadLogsFrom(record.LogPath, maxFiles, opts)
	if err != nil {
		respondError(w, err)

		return
	}
	defer reader.Close()

	w.Header().Set("Content-Type", "application/vnd.docker.raw-stream")
	w.WriteHeader(http.StatusOK)

	flusher, _ := w.(http.Flusher)

	for msg := range reader.Lines {
		writeStdcopyFrame(w, msg)

		if flusher != nil {
			flusher.Flush()
		}
	}
}

func parseLogOpts(r *http.Request) (clog.ReadOpts, error) {
	q := r.URL.Query()
	opts := clog.ReadOpts{
		Stdout: q.Get("stdout") != "false" && q.Get("stdout") != "0",
		Stderr: q.Get("stderr") != "false" && q.Get("stderr") != "0",
		Tail:   -1,
	}

	if v := q.Get("tail"); v != "" && v != "all" {
		n, parseErr := strconv.Atoi(v)
		if parseErr != nil {
			return opts, fmt.Errorf("parse log opts: %w",
				errdefs.InvalidParameter(fmt.Sprintf("invalid tail value: %q", v), parseErr))
		}

		opts.Tail = n
	}

	if v := q.Get("since"); v != "" {
		t, parseErr := parseSince(v)
		if parseErr != nil {
			return opts, fmt.Errorf("parse log opts: %w",
				errdefs.InvalidParameter(fmt.Sprintf("invalid since value: %q", v), parseErr))
		}

		opts.Since = t
	}

	if v := q.Get("until"); v != "" {
		t, parseErr := parseSince(v)
		if parseErr != nil {
			return opts, fmt.Errorf("parse log opts: %w",
				errdefs.InvalidParameter(fmt.Sprintf("invalid until value: %q", v), parseErr))
		}

		opts.Until = t
	}

	return opts, nil
}

func logMaxFiles(record *container.InspectResponse) int {
	if record.HostConfig != nil && record.HostConfig.LogConfig.Config != nil {
		if v, ok := record.HostConfig.LogConfig.Config["max-file"]; ok {
			n, _ := strconv.Atoi(v)

			return n
		}
	}

	return 0
}

// writeStdcopyFrame writes a Docker stdcopy multiplexed frame.
// Format: [stream_type, 0, 0, 0, size(4 bytes big-endian)] + payload.
func writeStdcopyFrame(w http.ResponseWriter, msg *clog.Message) {
	streamType := byte(1) // stdout
	if msg.Stream == "stderr" {
		streamType = 2
	}

	var header [8]byte

	header[0] = streamType
	binary.BigEndian.PutUint32(header[4:8], uint32(len(msg.Line))) //nolint:gosec // Log line length is bounded by scanner buffer size.

	_, _ = w.Write(header[:])
	_, _ = w.Write(msg.Line) //nolint:gosec // Response writer errors are handled by the HTTP server.
}

// inspectToSummary converts an InspectResponse to a container Summary for listing.
func inspectToSummary(c *container.InspectResponse) *container.Summary {
	s := &container.Summary{
		ID:    c.ID,
		Names: []string{c.Name},
		Image: "",
	}

	if c.Config != nil {
		s.Image = c.Config.Image
		s.Labels = c.Config.Labels

		// Build command string.
		if len(c.Config.Cmd) > 0 {
			s.Command = strings.Join(c.Config.Cmd, " ")
		}
	}

	s.ImageID = c.Image

	// Parse created time.
	if t, err := time.Parse(time.RFC3339Nano, c.Created); err == nil {
		s.Created = t.Unix()
	}

	if c.State != nil {
		s.State = c.State.Status
		s.Status = formatStatus(c.State)
	}

	s.Mounts = c.Mounts

	return s
}

func formatStatus(state *container.State) string {
	switch state.Status {
	case container.StateRunning:
		if t, err := time.Parse(time.RFC3339Nano, state.StartedAt); err == nil {
			return fmt.Sprintf("Up %s", time.Since(t).Truncate(time.Second))
		}

		return "Up"
	case container.StateExited:
		exitStr := fmt.Sprintf("Exited (%d)", state.ExitCode)

		if t, err := time.Parse(time.RFC3339Nano, state.FinishedAt); err == nil {
			return fmt.Sprintf("%s %s ago", exitStr, time.Since(t).Truncate(time.Second))
		}

		return exitStr
	case container.StateCreated:
		return "Created"
	case container.StateRestarting:
		return "Restarting"
	default:
		return string(state.Status)
	}
}

// parseSince parses a Docker "since" parameter, which can be a Unix timestamp
// (integer or float) or an RFC3339 time.
func parseSince(s string) (time.Time, error) {
	// Try as Unix timestamp (integer).
	if ts, err := strconv.ParseInt(s, 10, 64); err == nil {
		return time.Unix(ts, 0), nil
	}

	// Try as Unix timestamp (float).
	if ts, err := strconv.ParseFloat(s, 64); err == nil {
		sec := int64(ts)
		nsec := int64((ts - float64(sec)) * 1e9)

		return time.Unix(sec, nsec), nil
	}

	// Try as duration (e.g., "5m").
	if d, err := time.ParseDuration(s); err == nil {
		return time.Now().Add(-d), nil
	}

	// Try as RFC3339.
	if t, parseErr := time.Parse(time.RFC3339, s); parseErr == nil {
		return t, nil
	}

	// Try as RFC3339Nano (e.g., "2024-01-15T10:30:00.123456789Z").
	t, parseErr := time.Parse(time.RFC3339Nano, s)
	if parseErr != nil {
		return time.Time{}, fmt.Errorf("parse time %q: %w", s, parseErr)
	}

	return t, nil
}

// ContainerAttach handles POST /containers/{id}/attach.
func (h *Handlers) ContainerAttach(w http.ResponseWriter, r *http.Request) {
	id := pathValue(r, "id")

	record, err := h.runtime.Inspect(r.Context(), id)
	if err != nil {
		if isNotFound(err) {
			respondError(w, errdefs.NotFound("no such container: "+id, err))

			return
		}

		respondError(w, err)

		return
	}

	q := r.URL.Query()
	wantStream := q.Get("stream") == "1" || q.Get("stream") == queryValueTrue
	wantStdin := q.Get("stdin") == "1" || q.Get("stdin") == queryValueTrue
	wantStdout := q.Get("stdout") == "1" || q.Get("stdout") == queryValueTrue
	wantStderr := q.Get("stderr") == "1" || q.Get("stderr") == queryValueTrue

	// Upgrade to raw stream.
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

	// Send 101 Switching Protocols — Docker CLI requires this for upgrade.
	_, _ = buf.WriteString("HTTP/1.1 101 UPGRADED\r\n")
	_, _ = buf.WriteString("Content-Type: application/vnd.docker.raw-stream\r\n")
	_, _ = buf.WriteString("Connection: Upgrade\r\n")
	_, _ = buf.WriteString("Upgrade: tcp\r\n")
	_, _ = buf.WriteString("\r\n")
	_ = buf.Flush()

	if !wantStream {
		// Non-streaming attach (docker run -d flow): close immediately.
		_ = conn.Close()

		return
	}

	isTTY := record.Config != nil && record.Config.Tty

	// Use a detached context — r.Context() is canceled after hijack since
	// Go's HTTP server considers the request done once the connection is taken.
	_ = h.runtime.Attach(context.Background(), record.ID, &runtime.AttachOpts{ //nolint:contextcheck // Detached context: r.Context() is canceled after hijack.
		Conn:   conn,
		Stdin:  wantStdin,
		Stdout: wantStdout,
		Stderr: wantStderr,
		TTY:    isTTY,
	})

	_ = conn.Close()
}

// ContainerResize handles POST /containers/{id}/resize.
func (h *Handlers) ContainerResize(w http.ResponseWriter, r *http.Request) {
	id := pathValue(r, "id")

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

	if resizeErr := h.runtime.Resize(r.Context(), id, uint16(height), uint16(width)); resizeErr != nil { //nolint:gosec // Height/width are validated terminal dimensions.
		if isNotFound(resizeErr) {
			respondError(w, errdefs.NotFound("no such container: "+id, resizeErr))

			return
		}

		respondError(w, resizeErr)

		return
	}

	w.WriteHeader(http.StatusOK)
}

// ContainerPause handles POST /containers/{id}/pause.
func (h *Handlers) ContainerPause(w http.ResponseWriter, r *http.Request) {
	id := pathValue(r, "id")

	if err := h.runtime.Pause(r.Context(), id); err != nil {
		if isNotFound(err) {
			respondError(w, errdefs.NotFound("no such container: "+id, err))

			return
		}

		respondError(w, err)

		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// ContainerUnpause handles POST /containers/{id}/unpause.
func (h *Handlers) ContainerUnpause(w http.ResponseWriter, r *http.Request) {
	id := pathValue(r, "id")

	if err := h.runtime.Unpause(r.Context(), id); err != nil {
		if isNotFound(err) {
			respondError(w, errdefs.NotFound("no such container: "+id, err))

			return
		}

		respondError(w, err)

		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// ContainerTop handles GET /containers/{id}/top.
func (h *Handlers) ContainerTop(w http.ResponseWriter, r *http.Request) {
	id := pathValue(r, "id")

	psArgs := r.URL.Query().Get("ps_args")
	if psArgs == "" {
		psArgs = "-ef"
	}

	resp, err := h.runtime.Top(r.Context(), id, psArgs)
	if err != nil {
		if isNotFound(err) {
			respondError(w, errdefs.NotFound("no such container: "+id, err))

			return
		}

		respondError(w, err)

		return
	}

	respondJSON(w, http.StatusOK, resp)
}
