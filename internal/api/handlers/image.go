package handlers

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/Shikachuu/kogia/internal/image"
	imagetypes "github.com/moby/moby/api/types/image"
)

var (
	errFromImageRequired = errors.New("fromImage is required")
	errRepoRequired      = errors.New("repo is required")
	errSearchTermRequired = errors.New("search term is required")
	errNamesRequired      = errors.New("names query parameter is required")
)

// ImageCreate handles POST /images/create (pull).
func (h *Handlers) ImageCreate(w http.ResponseWriter, r *http.Request) {
	fromImage := r.URL.Query().Get("fromImage")
	if fromImage == "" {
		errorJSON(w, http.StatusBadRequest, errFromImageRequired)

		return
	}

	tag := r.URL.Query().Get("tag")

	auth := image.ResolveAuth(r.Header.Get("X-Registry-Auth"), registryFromRef(fromImage))

	// Start streaming response immediately.
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)

	flusher, _ := w.(http.Flusher)

	if err := h.images.Pull(r.Context(), fromImage, tag, auth, w, flusher); err != nil {
		slog.Error("image pull failed", "image", fromImage, "err", err)
		image.WriteError(w, flusher, err)
	}
}

// ImageList handles GET /images/json.
func (h *Handlers) ImageList(w http.ResponseWriter, _ *http.Request) {
	images, err := h.images.List()
	if err != nil {
		errorJSON(w, http.StatusInternalServerError, err)

		return
	}

	respondJSON(w, http.StatusOK, images)
}

// ImageInspect handles GET /images/{name}/json.
func (h *Handlers) ImageInspect(w http.ResponseWriter, r *http.Request) {
	name := pathValue(r, "name")

	info, err := h.images.Get(name)
	if err != nil {
		if errors.Is(err, image.ErrNotFound) {
			errorJSON(w, http.StatusNotFound, fmt.Errorf("no such image: %s: %w", name, err))

			return
		}

		errorJSON(w, http.StatusInternalServerError, err)

		return
	}

	respondJSON(w, http.StatusOK, info)
}

// ImageDelete handles DELETE /images/{name}.
func (h *Handlers) ImageDelete(w http.ResponseWriter, r *http.Request) {
	name := pathValue(r, "name")
	force := r.URL.Query().Get("force") == "true" || r.URL.Query().Get("force") == "1"
	noprune := r.URL.Query().Get("noprune") == "true" || r.URL.Query().Get("noprune") == "1"

	items, err := h.images.Remove(name, force, !noprune)
	if err != nil {
		if errors.Is(err, image.ErrNotFound) {
			errorJSON(w, http.StatusNotFound, fmt.Errorf("no such image: %s: %w", name, err))

			return
		}

		errorJSON(w, http.StatusInternalServerError, err)

		return
	}

	respondJSON(w, http.StatusOK, items)
}

// ImageTag handles POST /images/{name}/tag.
func (h *Handlers) ImageTag(w http.ResponseWriter, r *http.Request) {
	name := pathValue(r, "name")
	repo := r.URL.Query().Get("repo")
	tag := r.URL.Query().Get("tag")

	if repo == "" {
		errorJSON(w, http.StatusBadRequest, errRepoRequired)

		return
	}

	if err := h.images.Tag(name, repo, tag); err != nil {
		if errors.Is(err, image.ErrNotFound) {
			errorJSON(w, http.StatusNotFound, fmt.Errorf("no such image: %s: %w", name, err))

			return
		}

		errorJSON(w, http.StatusInternalServerError, err)

		return
	}

	w.WriteHeader(http.StatusCreated)
}

// ImageHistory handles GET /images/{name}/history.
func (h *Handlers) ImageHistory(w http.ResponseWriter, r *http.Request) {
	name := pathValue(r, "name")

	history, err := h.images.History(name)
	if err != nil {
		if errors.Is(err, image.ErrNotFound) {
			errorJSON(w, http.StatusNotFound, fmt.Errorf("no such image: %s: %w", name, err))

			return
		}

		errorJSON(w, http.StatusInternalServerError, err)

		return
	}

	respondJSON(w, http.StatusOK, history)
}

// ImagePrune handles POST /images/prune.
func (h *Handlers) ImagePrune(w http.ResponseWriter, _ *http.Request) {
	deleted, reclaimed, err := h.images.Prune()
	if err != nil {
		errorJSON(w, http.StatusInternalServerError, err)

		return
	}

	respondJSON(w, http.StatusOK, imagetypes.PruneReport{
		ImagesDeleted:  deleted,
		SpaceReclaimed: reclaimed,
	})
}

// ImageSearch handles GET /images/search.
func (h *Handlers) ImageSearch(w http.ResponseWriter, r *http.Request) {
	term := r.URL.Query().Get("term")
	if term == "" {
		errorJSON(w, http.StatusBadRequest, errSearchTermRequired)

		return
	}

	limit := 25

	if l := r.URL.Query().Get("limit"); l != "" {
		parsed, parseErr := strconv.Atoi(l)
		if parseErr == nil && parsed > 0 {
			limit = parsed
		}
	}

	results, err := image.Search(r.Context(), term, limit)
	if err != nil {
		errorJSON(w, http.StatusInternalServerError, err)

		return
	}

	respondJSON(w, http.StatusOK, results)
}

// ImageGet handles GET /images/{name}/get (export single image as tar).
func (h *Handlers) ImageGet(w http.ResponseWriter, r *http.Request) {
	name := pathValue(r, "name")

	w.Header().Set("Content-Type", "application/x-tar")

	if err := h.images.Export([]string{name}, w); err != nil {
		if errors.Is(err, image.ErrNotFound) {
			errorJSON(w, http.StatusNotFound, fmt.Errorf("no such image: %s: %w", name, err))

			return
		}

		slog.Error("image export failed", "image", name, "err", err)
	}
}

// ImageGetAll handles GET /images/get (export multiple images as tar).
func (h *Handlers) ImageGetAll(w http.ResponseWriter, r *http.Request) {
	names := r.URL.Query()["names"]
	if len(names) == 0 {
		errorJSON(w, http.StatusBadRequest, errNamesRequired)

		return
	}

	w.Header().Set("Content-Type", "application/x-tar")

	if err := h.images.Export(names, w); err != nil {
		slog.Error("image export failed", "images", names, "err", err)
	}
}

// ImageLoad handles POST /images/load (import images from tar).
func (h *Handlers) ImageLoad(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)

	flusher, _ := w.(http.Flusher)

	if err := h.images.Load(r.Context(), r.Body, w, flusher); err != nil {
		slog.Error("image load failed", "err", err)
		image.WriteError(w, flusher, err)
	}
}

// ImagePush handles POST /images/{name}/push.
func (h *Handlers) ImagePush(w http.ResponseWriter, r *http.Request) {
	name := pathValue(r, "name")
	tag := r.URL.Query().Get("tag")

	auth := image.ResolveAuth(r.Header.Get("X-Registry-Auth"), registryFromRef(name))

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)

	flusher, _ := w.(http.Flusher)

	if err := h.images.Push(r.Context(), name, tag, auth, w, flusher); err != nil {
		slog.Error("image push failed", "image", name, "err", err)
		image.WriteError(w, flusher, err)
	}
}

// registryFromRef extracts the registry hostname from a reference string.
func registryFromRef(ref string) string {
	// Simple extraction: take everything before the first /.
	if idx := strings.IndexByte(ref, '/'); idx > 0 {
		candidate := ref[:idx]
		// Must look like a hostname (contains a dot or is localhost).
		if strings.ContainsAny(candidate, ".:") {
			return candidate
		}
	}

	return "docker.io"
}
