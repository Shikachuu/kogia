package handlers

import (
	"encoding/json"
	"errors"
	"net"
	"net/http"

	"github.com/containers/image/v5/docker"

	"github.com/Shikachuu/kogia/internal/api/errdefs"
	"github.com/Shikachuu/kogia/internal/image"
)

// SystemAuth handles POST /auth.
// It validates registry credentials without persisting them — the Docker CLI
// handles writing credentials to config.json on success.
func (h *Handlers) SystemAuth(w http.ResponseWriter, r *http.Request) {
	var auth image.AuthConfig

	if err := json.NewDecoder(r.Body).Decode(&auth); err != nil {
		respondError(w, errdefs.InvalidParameter("invalid auth config", err))

		return
	}

	if auth.IsEmpty() {
		respondError(w, errdefs.InvalidParameter("credentials required", nil))

		return
	}

	if err := h.images.CheckAuth(r.Context(), &auth); err != nil {
		respondAuthError(w, err, auth.ServerAddress)

		return
	}

	respondJSON(w, http.StatusOK, map[string]string{
		"Status": "Login Succeeded",
	})
}

// respondAuthError writes the appropriate error response for a failed auth check.
func respondAuthError(w http.ResponseWriter, err error, server string) {
	var unauthorizedErr docker.ErrUnauthorizedForCredentials
	if errors.As(err, &unauthorizedErr) {
		respondError(w, errdefs.Unauthorized("unauthorized: incorrect username or password", err))

		return
	}

	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		respondError(w, errdefs.InvalidParameter("registry not reachable: "+server, err))

		return
	}

	var opErr *net.OpError
	if errors.As(err, &opErr) {
		respondError(w, errdefs.InvalidParameter("registry not reachable: "+server, err))

		return
	}

	respondError(w, err)
}
