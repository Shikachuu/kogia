package handlers

import (
	"net/http"

	"github.com/moby/moby/api/types/container"
)

// ContainerList handles GET /containers/json.
func (h *Handlers) ContainerList(w http.ResponseWriter, _ *http.Request) {
	respondJSON(w, http.StatusOK, []*container.Summary{})
}
