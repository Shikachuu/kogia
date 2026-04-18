package handlers

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/Shikachuu/kogia/internal/api/errdefs"
	"github.com/Shikachuu/kogia/internal/store"
	vol "github.com/Shikachuu/kogia/internal/volume"
	"github.com/moby/moby/api/types/events"
	"github.com/moby/moby/api/types/volume"
)

// VolumeCreate handles POST /volumes/create.
func (h *Handlers) VolumeCreate(w http.ResponseWriter, r *http.Request) {
	var req volume.CreateRequest

	if decErr := json.NewDecoder(r.Body).Decode(&req); decErr != nil {
		respondError(w, errdefs.InvalidParameter("invalid volume create request", decErr))

		return
	}

	rec, err := h.volumes.Create(req.Name, req.Driver, req.Labels, req.DriverOpts)
	if err != nil {
		if errors.Is(err, vol.ErrUnsupportedDriver) {
			respondError(w, errdefs.InvalidParameter(err.Error(), err))

			return
		}

		respondError(w, err)

		return
	}

	h.publishEvent(events.VolumeEventType, events.ActionCreate, rec.Name, nil)

	respondJSON(w, http.StatusCreated, recordToVolume(rec))
}

// VolumeInspect handles GET /volumes/{name}.
func (h *Handlers) VolumeInspect(w http.ResponseWriter, r *http.Request) {
	name := pathValue(r, "name")

	rec, err := h.volumes.Get(name)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			respondError(w, errdefs.NotFound("volume "+name+" not found", err))

			return
		}

		respondError(w, err)

		return
	}

	respondJSON(w, http.StatusOK, recordToVolume(rec))
}

// VolumeList handles GET /volumes.
func (h *Handlers) VolumeList(w http.ResponseWriter, _ *http.Request) {
	recs, err := h.volumes.List()
	if err != nil {
		respondError(w, err)

		return
	}

	vols := make([]volume.Volume, 0, len(recs))

	for _, rec := range recs {
		vols = append(vols, recordToVolume(rec))
	}

	respondJSON(w, http.StatusOK, volume.ListResponse{
		Volumes:  vols,
		Warnings: []string{},
	})
}

// VolumeDelete handles DELETE /volumes/{name}.
func (h *Handlers) VolumeDelete(w http.ResponseWriter, r *http.Request) {
	name := pathValue(r, "name")
	force := r.URL.Query().Get("force") == queryValueTrue || r.URL.Query().Get("force") == "1"

	if err := h.volumes.Remove(name, force); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			respondError(w, errdefs.NotFound("volume "+name+" not found", err))

			return
		}

		respondError(w, err)

		return
	}

	h.publishEvent(events.VolumeEventType, events.ActionDestroy, name, nil)

	w.WriteHeader(http.StatusNoContent)
}

// VolumePrune handles POST /volumes/prune.
func (h *Handlers) VolumePrune(w http.ResponseWriter, _ *http.Request) {
	deleted, reclaimed, err := h.volumes.Prune(h.store)
	if err != nil {
		respondError(w, err)

		return
	}

	respondJSON(w, http.StatusOK, volume.PruneReport{
		VolumesDeleted: deleted,
		SpaceReclaimed: reclaimed,
	})
}

func recordToVolume(rec *vol.Record) volume.Volume {
	labels := rec.Labels
	if labels == nil {
		labels = map[string]string{}
	}

	opts := rec.Options
	if opts == nil {
		opts = map[string]string{}
	}

	return volume.Volume{
		Name:       rec.Name,
		Driver:     rec.Driver,
		Mountpoint: rec.Mountpoint,
		Labels:     labels,
		Options:    opts,
		Scope:      "local",
		CreatedAt:  rec.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
	}
}
