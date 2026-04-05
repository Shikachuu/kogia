package handlers

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/Shikachuu/kogia/internal/api/errdefs"
	"github.com/Shikachuu/kogia/internal/image"
	"github.com/Shikachuu/kogia/internal/store"
)

// ImageCommit handles POST /commit — create an image from a container.
func (h *Handlers) ImageCommit(w http.ResponseWriter, r *http.Request) {
	containerParam := r.URL.Query().Get("container")
	if containerParam == "" {
		respondError(w, errdefs.InvalidParameter("container is required", nil))

		return
	}

	repo := r.URL.Query().Get("repo")
	tag := r.URL.Query().Get("tag")
	comment := r.URL.Query().Get("comment")
	author := r.URL.Query().Get("author")

	// Resolve the container to get its ID and base image.
	ctr, err := h.store.GetContainer(containerParam)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			respondError(w, errdefs.NotFound("no such container: "+containerParam, err))

			return
		}

		respondError(w, err)

		return
	}

	// Parse optional config overrides from the request body.
	var commitCfg *image.CommitConfig

	if r.Body != nil && r.ContentLength != 0 {
		commitCfg, err = parseCommitConfig(r)
		if err != nil {
			respondError(w, errdefs.InvalidParameter("invalid commit config: "+err.Error(), err))

			return
		}
	}

	// ctr.Image is the base image ID set during container creation.
	newID, err := h.images.Commit(ctr.ID, ctr.Image, repo, tag, comment, author, commitCfg)
	if err != nil {
		respondError(w, err)

		return
	}

	respondJSON(w, http.StatusCreated, map[string]string{"Id": newID})
}

// parseCommitConfig reads a Docker container.Config from the request body and
// maps it to an image.CommitConfig with the fields relevant for commit.
func parseCommitConfig(r *http.Request) (*image.CommitConfig, error) {
	var body struct {
		ExposedPorts map[string]struct{} `json:"ExposedPorts"`
		Volumes      map[string]struct{} `json:"Volumes"`
		Labels       map[string]string   `json:"Labels"`
		WorkingDir   string              `json:"WorkingDir"`
		User         string              `json:"User"`
		Cmd          []string            `json:"Cmd"`
		Entrypoint   []string            `json:"Entrypoint"`
		Env          []string            `json:"Env"`
	}

	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("decode commit config: %w", err)
	}

	return &image.CommitConfig{
		Cmd:          body.Cmd,
		Entrypoint:   body.Entrypoint,
		Env:          body.Env,
		ExposedPorts: body.ExposedPorts,
		Volumes:      body.Volumes,
		WorkingDir:   body.WorkingDir,
		Labels:       body.Labels,
		User:         body.User,
	}, nil
}
