package handlers

import (
	"net/http"

	"github.com/Shikachuu/kogia/internal/api/errdefs"
	"github.com/Shikachuu/kogia/internal/image"
)

// DistributionInspect handles GET /distribution/{name}/json.
func (h *Handlers) DistributionInspect(w http.ResponseWriter, r *http.Request) {
	name := pathValue(r, "name")
	if name == "" {
		respondError(w, errdefs.InvalidParameter("image name is required", nil))

		return
	}

	auth := image.ResolveAuth(r.Header.Get("X-Registry-Auth"), registryFromRef(name))

	result, err := h.images.DistributionInspect(r.Context(), name, auth)
	if err != nil {
		respondError(w, err)

		return
	}

	respondJSON(w, http.StatusOK, result)
}
