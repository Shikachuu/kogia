package handlers

import (
	"net/http"

	"github.com/Shikachuu/kogia/internal/api/errdefs"
)

const buildMsg = "not supported, use 'docker buildx build' with the docker-container driver"

// ImageBuild handles POST /build.
func (h *Handlers) ImageBuild(w http.ResponseWriter, _ *http.Request) {
	respondError(w, errdefs.InvalidParameter(buildMsg, nil))
}

// BuildPrune handles POST /build/prune.
func (h *Handlers) BuildPrune(w http.ResponseWriter, _ *http.Request) {
	respondError(w, errdefs.InvalidParameter(buildMsg, nil))
}

// Session handles POST /session.
func (h *Handlers) Session(w http.ResponseWriter, _ *http.Request) {
	respondError(w, errdefs.InvalidParameter(buildMsg, nil))
}
