package handlers

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime"

	"github.com/Shikachuu/kogia/internal/api/stream"
	"github.com/Shikachuu/kogia/internal/events"
	"github.com/Shikachuu/kogia/internal/store"
	"github.com/moby/moby/api/types/build"
	"github.com/moby/moby/api/types/container"
	imagetypes "github.com/moby/moby/api/types/image"
	"github.com/moby/moby/api/types/system"
	volumetypes "github.com/moby/moby/api/types/volume"
	"golang.org/x/sys/unix"
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

// SystemDataUsage handles GET /system/df — returns disk usage by resource type.
func (h *Handlers) SystemDataUsage(w http.ResponseWriter, r *http.Request) {
	types := parseTypeFilter(r)

	resp := system.DiskUsage{
		BuildCacheUsage: &build.DiskUsage{},
	}

	// Load all containers once — needed by both container and image usage.
	var containers []*container.InspectResponse

	if types.includes("container") || types.includes("image") {
		var err error

		containers, err = h.store.ListContainers(&store.ContainerFilters{All: true})
		if err != nil {
			slog.Error("system df: list containers", "err", err)
			respondError(w, err)

			return
		}
	}

	if types.includes("image") {
		du, err := h.imageDiskUsage(containers)
		if err != nil {
			slog.Error("system df: image usage", "err", err)
			respondError(w, err)

			return
		}

		resp.ImageUsage = du
	}

	if types.includes("container") {
		resp.ContainerUsage = h.containerDiskUsage(containers)
	}

	if types.includes("volume") {
		du, err := h.volumeDiskUsage()
		if err != nil {
			slog.Error("system df: volume usage", "err", err)
			respondError(w, err)

			return
		}

		resp.VolumeUsage = du
	}

	respondJSON(w, http.StatusOK, resp)
}

// imageDiskUsage computes disk usage for all images.
func (h *Handlers) imageDiskUsage(containers []*container.InspectResponse) (*imagetypes.DiskUsage, error) {
	images, err := h.images.List()
	if err != nil {
		return nil, fmt.Errorf("list images: %w", err)
	}

	// Build set of image IDs referenced by containers.
	usedImages := make(map[string]int64, len(containers))
	for _, c := range containers {
		usedImages[c.Image]++
	}

	du := &imagetypes.DiskUsage{
		Items:      make([]imagetypes.Summary, 0, len(images)),
		TotalCount: int64(len(images)),
	}

	for i := range images {
		img := images[i]
		img.Containers = usedImages[img.ID]
		du.TotalSize += img.Size

		if img.Containers > 0 {
			du.ActiveCount++
		} else {
			du.Reclaimable += img.Size
		}

		du.Items = append(du.Items, img)
	}

	return du, nil
}

// containerDiskUsage computes disk usage for all containers.
func (h *Handlers) containerDiskUsage(containers []*container.InspectResponse) *container.DiskUsage {
	rawStore := h.images.RawStore()

	du := &container.DiskUsage{
		Items:      make([]container.Summary, 0, len(containers)),
		TotalCount: int64(len(containers)),
	}

	for _, c := range containers {
		s := inspectToSummary(c)

		size, err := rawStore.ContainerSize(c.ID)
		if err != nil {
			slog.Debug("system df: container size", "id", c.ID, "err", err)
		} else {
			s.SizeRw = size
		}

		du.TotalSize += s.SizeRw

		if c.State != nil && c.State.Status == "running" {
			du.ActiveCount++
		} else {
			du.Reclaimable += s.SizeRw
		}

		du.Items = append(du.Items, *s)
	}

	return du
}

// volumeDiskUsage computes disk usage for all volumes.
func (h *Handlers) volumeDiskUsage() (*volumetypes.DiskUsage, error) {
	recs, err := h.volumes.List()
	if err != nil {
		return nil, fmt.Errorf("list volumes: %w", err)
	}

	refs, err := h.store.AllVolumeRefs()
	if err != nil {
		return nil, fmt.Errorf("volume refs: %w", err)
	}

	du := &volumetypes.DiskUsage{
		Items:      make([]volumetypes.Volume, 0, len(recs)),
		TotalCount: int64(len(recs)),
	}

	for _, rec := range recs {
		v := recordToVolume(rec)
		size := dirSizeBytes(rec.Mountpoint)

		var refCount int64
		if _, ok := refs[rec.Name]; ok {
			refCount = 1
			du.ActiveCount++
		}

		v.UsageData = &volumetypes.UsageData{
			Size:     size,
			RefCount: refCount,
		}

		du.TotalSize += size
		du.Items = append(du.Items, v)

		if refCount == 0 {
			du.Reclaimable += size
		}
	}

	return du, nil
}

// typeFilter tracks which resource types to include in system df output.
type typeFilter map[string]bool

// parseTypeFilter extracts the "type" query parameter.
// An empty filter means all types are included.
func parseTypeFilter(r *http.Request) typeFilter {
	vals := r.URL.Query()["type"]
	if len(vals) == 0 {
		return nil
	}

	f := make(typeFilter, len(vals))
	for _, v := range vals {
		f[v] = true
	}

	return f
}

func (f typeFilter) includes(t string) bool {
	return len(f) == 0 || f[t]
}

// dirSizeBytes walks a directory and sums file sizes.
func dirSizeBytes(path string) int64 {
	var size int64

	_ = filepath.Walk(path, func(_ string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		if !info.IsDir() && info.Size() > 0 {
			size += info.Size()
		}

		return nil
	})

	return size
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
