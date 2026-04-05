// Package image manages container images via containers/image and containers/storage.
package image

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/containers/image/v5/docker/reference"
	"github.com/containers/image/v5/signature"
	"github.com/containers/storage"
	storagetypes "github.com/containers/storage/types"
	dockerspec "github.com/moby/docker-image-spec/specs-go/v1"
	imagetypes "github.com/moby/moby/api/types/image"
)

// ErrNotFound is returned when an image cannot be found.
var ErrNotFound = errors.New("image: not found")

// ErrInvalidDriver is returned when an unsupported storage driver is specified.
var ErrInvalidDriver = errors.New("image: invalid storage driver")

// StorageDriver represents a containers/storage graph driver.
type StorageDriver string

const (
	// DriverOverlay uses the kernel overlayfs driver. Default and most efficient.
	DriverOverlay StorageDriver = "overlay"
	// DriverVFS uses simple directory copies. Works everywhere, uses more disk.
	DriverVFS StorageDriver = "vfs"
	// DriverFuseOverlayfs uses fuse-overlayfs. Useful for rootless or unsupported kernels.
	DriverFuseOverlayfs StorageDriver = "fuse-overlayfs"
)

// ParseStorageDriver validates and returns a StorageDriver from a string.
// An empty string defaults to DriverOverlay.
func ParseStorageDriver(s string) (StorageDriver, error) {
	if s == "" {
		return DriverOverlay, nil
	}

	switch StorageDriver(s) {
	case DriverOverlay, DriverVFS, DriverFuseOverlayfs:
		return StorageDriver(s), nil
	default:
		return "", fmt.Errorf("%w: %q (valid: overlay, vfs, fuse-overlayfs)", ErrInvalidDriver, s)
	}
}

// Store wraps containers/storage for image management.
type Store struct {
	store storage.Store
}

// StoreOptions configures the image store paths.
type StoreOptions struct {
	GraphRoot string        // persistent storage (layers, images)
	RunRoot   string        // ephemeral runtime state
	Driver    StorageDriver // storage driver
}

// NewStore initializes containers/storage with the configured driver.
func NewStore(opts StoreOptions) (*Store, error) {
	driver := opts.Driver
	if driver == "" {
		driver = DriverOverlay
	}

	storeOpts := storagetypes.StoreOptions{
		GraphRoot:       opts.GraphRoot,
		RunRoot:         opts.RunRoot,
		GraphDriverName: string(driver),
	}

	s, err := storage.GetStore(storeOpts)
	if err != nil {
		return nil, fmt.Errorf("image: init storage: %w", err)
	}

	return &Store{store: s}, nil
}

// Close shuts down the storage backend.
func (s *Store) Close() error {
	_, err := s.store.Shutdown(true)
	if err != nil {
		return fmt.Errorf("image: shutdown storage: %w", err)
	}

	return nil
}

// RawStore returns the underlying containers/storage.Store for use by pull operations.
func (s *Store) RawStore() storage.Store {
	return s.store
}

// List returns all images as Docker API summaries.
func (s *Store) List() ([]imagetypes.Summary, error) {
	images, err := s.store.Images()
	if err != nil {
		return nil, fmt.Errorf("image: list: %w", err)
	}

	summaries := make([]imagetypes.Summary, 0, len(images))

	for i := range images {
		img := &images[i]
		size, _ := s.store.ImageSize(img.ID)
		tags, digests := splitNames(img.Names)

		summaries = append(summaries, imagetypes.Summary{
			ID:          img.ID,
			Created:     img.Created.Unix(),
			Size:        size,
			SharedSize:  -1,
			Containers:  -1,
			RepoTags:    tags,
			RepoDigests: digests,
			Labels:      s.imageLabels(img.ID),
			ParentID:    "",
		})
	}

	return summaries, nil
}

// Get returns a Docker API InspectResponse for the given image name or ID.
func (s *Store) Get(nameOrID string) (*imagetypes.InspectResponse, error) {
	img, err := s.resolve(nameOrID)
	if err != nil {
		return nil, err
	}

	size, _ := s.store.ImageSize(img.ID)
	tags, digests := splitNames(img.Names)

	resp := &imagetypes.InspectResponse{
		ID:          img.ID,
		RepoTags:    tags,
		RepoDigests: digests,
		Created:     img.Created.Format("2006-01-02T15:04:05.999999999Z"),
		Size:        size,
	}

	// Read OCI config from storage big data to populate Config, Architecture, Os, RootFS.
	s.populateFromOCIConfig(img, resp)

	return resp, nil
}

// Remove deletes an image. Returns the list of deleted/untagged entries.
func (s *Store) Remove(nameOrID string, _, _ bool) ([]imagetypes.DeleteResponse, error) {
	img, err := s.resolve(nameOrID)
	if err != nil {
		return nil, err
	}

	var results []imagetypes.DeleteResponse

	// If nameOrID is a specific tag and image has other names, just untag.
	if isTagReference(nameOrID) && len(img.Names) > 1 {
		fullRef := normalizeRef(nameOrID)

		removeErr := s.store.RemoveNames(img.ID, []string{fullRef})
		if removeErr != nil {
			return nil, fmt.Errorf("image: untag: %w", removeErr)
		}

		results = append(results, imagetypes.DeleteResponse{Untagged: fullRef})

		return results, nil
	}

	// Untag all names first.
	for _, name := range img.Names {
		results = append(results, imagetypes.DeleteResponse{Untagged: name})
	}

	// Delete the image.
	_, deleteErr := s.store.DeleteImage(img.ID, true)
	if deleteErr != nil {
		return nil, fmt.Errorf("image: delete: %w", deleteErr)
	}

	results = append(results, imagetypes.DeleteResponse{Deleted: img.ID})

	return results, nil
}

// Tag adds a new name to an existing image.
func (s *Store) Tag(nameOrID, repo, tag string) error {
	img, err := s.resolve(nameOrID)
	if err != nil {
		return err
	}

	if tag == "" {
		tag = "latest"
	}

	newName := repo + ":" + tag

	// Normalize to full reference.
	parsed, err := reference.ParseNormalizedNamed(newName)
	if err != nil {
		return fmt.Errorf("image: invalid tag %q: %w", newName, err)
	}

	tagged := reference.TagNameOnly(parsed)
	fullRef := tagged.String()

	addErr := s.store.AddNames(img.ID, []string{fullRef})
	if addErr != nil {
		return fmt.Errorf("image: add tag: %w", addErr)
	}

	return nil
}

// History returns the layer history for an image.
func (s *Store) History(nameOrID string) ([]imagetypes.HistoryResponseItem, error) {
	img, err := s.resolve(nameOrID)
	if err != nil {
		return nil, err
	}

	ociCfg, _ := s.readOCIConfig(img)
	if ociCfg == nil {
		// Degrade gracefully: return minimal history when OCI config is unavailable.
		return []imagetypes.HistoryResponseItem{{
			ID:      img.ID,
			Created: img.Created.Unix(),
			Tags:    img.Names,
		}}, nil
	}

	items := make([]imagetypes.HistoryResponseItem, 0, len(ociCfg.History))
	layerIdx := len(ociCfg.RootFS.DiffIDs) - 1

	// OCI history is ordered oldest-first; Docker returns newest-first.
	for i := len(ociCfg.History) - 1; i >= 0; i-- {
		h := ociCfg.History[i]

		item := imagetypes.HistoryResponseItem{
			ID:        "<missing>",
			CreatedBy: h.CreatedBy,
			Comment:   h.Comment,
		}

		if h.Created != nil {
			item.Created = h.Created.Unix()
		}

		if !h.EmptyLayer && layerIdx >= 0 {
			layerIdx--
		}

		if h.EmptyLayer {
			item.Size = 0
		}

		items = append(items, item)
	}

	// The top entry gets the image ID and tags.
	if len(items) > 0 {
		items[0].ID = img.ID
		items[0].Tags = img.Names
	}

	return items, nil
}

// Prune removes all untagged (dangling) images. Returns deleted items and reclaimed bytes.
func (s *Store) Prune() ([]imagetypes.DeleteResponse, uint64, error) {
	images, err := s.store.Images()
	if err != nil {
		return nil, 0, fmt.Errorf("image: prune list: %w", err)
	}

	var (
		deleted   []imagetypes.DeleteResponse
		reclaimed uint64
	)

	for i := range images {
		img := &images[i]

		// Only prune dangling images (no tags).
		if len(img.Names) > 0 {
			continue
		}

		size, _ := s.store.ImageSize(img.ID)

		_, deleteErr := s.store.DeleteImage(img.ID, true)
		if deleteErr != nil {
			continue
		}

		deleted = append(deleted, imagetypes.DeleteResponse{Deleted: img.ID})

		if size > 0 {
			reclaimed += uint64(size)
		}
	}

	return deleted, reclaimed, nil
}

// resolveWithTag tries to resolve an image by name+tag, falling back to name alone.
// Docker CLI sends name and tag separately for push (e.g., name="ttl.sh/img", tag="v1").
func (s *Store) resolveWithTag(nameOrID, tag string) (*storage.Image, error) {
	if tag != "" {
		withTag := nameOrID + ":" + tag
		if img, err := s.resolve(withTag); err == nil {
			return img, nil
		}
	}

	return s.resolve(nameOrID)
}

// resolve looks up an image by name or ID, trying normalization if direct lookup fails.
func (s *Store) resolve(nameOrID string) (*storage.Image, error) {
	// Direct lookup by ID or full name.
	img, err := s.store.Image(nameOrID)
	if err == nil {
		return img, nil
	}

	// Try normalizing short name (e.g., "alpine" → "docker.io/library/alpine:latest").
	normalized := normalizeRef(nameOrID)
	if normalized != nameOrID {
		img, err = s.store.Image(normalized)
		if err == nil {
			return img, nil
		}
	}

	return nil, fmt.Errorf("%w: %s", ErrNotFound, nameOrID)
}

// readOCIConfig reads and parses the OCI image config from storage big data.
func (s *Store) readOCIConfig(img *storage.Image) (*dockerspec.DockerOCIImage, error) {
	manifestData, err := s.store.ImageBigData(img.ID, storage.ImageDigestBigDataKey)
	if err != nil {
		return nil, fmt.Errorf("read manifest: %w", err)
	}

	cfgDigest := manifestConfigDigest(manifestData)

	if cfgDigest == "" {
		// May be a manifest list; try finding the config by probing big data keys.
		return s.probeOCIConfig(img)
	}

	configData, err := s.store.ImageBigData(img.ID, cfgDigest)
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", cfgDigest, err)
	}

	return parseOCIConfig(configData)
}

// probeOCIConfig tries to find and parse the OCI config from any big data key
// that looks like a config digest (for manifest list images).
func (s *Store) probeOCIConfig(img *storage.Image) (*dockerspec.DockerOCIImage, error) {
	keys, err := s.store.ListImageBigData(img.ID)
	if err != nil {
		return nil, fmt.Errorf("list big data: %w", err)
	}

	for _, key := range keys {
		if !strings.HasPrefix(key, "sha256:") || key == storage.ImageDigestBigDataKey {
			continue
		}

		data, bdErr := s.store.ImageBigData(img.ID, key)
		if bdErr != nil {
			continue
		}

		var probe struct {
			Architecture string `json:"architecture"`
		}

		if json.Unmarshal(data, &probe) == nil && probe.Architecture != "" {
			return parseOCIConfig(data)
		}
	}

	return nil, fmt.Errorf("%w: no OCI config found for %s", ErrNotFound, img.ID)
}

// populateFromOCIConfig fills Config, Architecture, Os, and RootFS on the InspectResponse.
func (s *Store) populateFromOCIConfig(img *storage.Image, resp *imagetypes.InspectResponse) {
	ociCfg, err := s.readOCIConfig(img)
	if err != nil {
		return
	}

	resp.Architecture = ociCfg.Architecture
	resp.Os = ociCfg.OS
	resp.Author = ociCfg.Author
	resp.Config = &ociCfg.Config

	if ociCfg.RootFS.DiffIDs != nil {
		layers := make([]string, len(ociCfg.RootFS.DiffIDs))
		for i, d := range ociCfg.RootFS.DiffIDs {
			layers[i] = d.String()
		}

		resp.RootFS = imagetypes.RootFS{
			Type:   ociCfg.RootFS.Type,
			Layers: layers,
		}
	}
}

// Config holds the OCI image configuration fields needed by the runtime.
type Config struct {
	WorkingDir string
	User       string
	Env        []string
	Entrypoint []string
	Cmd        []string
}

// GetConfig returns the OCI config fields needed for container creation.
func (s *Store) GetConfig(nameOrID string) (*Config, error) {
	img, err := s.resolve(nameOrID)
	if err != nil {
		return nil, err
	}

	ociCfg, err := s.readOCIConfig(img)
	if err != nil {
		return nil, fmt.Errorf("image: read config for %s: %w", nameOrID, err)
	}

	return &Config{
		Env:        ociCfg.Config.Env,
		Entrypoint: ociCfg.Config.Entrypoint,
		Cmd:        ociCfg.Config.Cmd,
		WorkingDir: ociCfg.Config.WorkingDir,
		User:       ociCfg.Config.User,
	}, nil
}

// parseOCIConfig unmarshals raw JSON into a DockerOCIImage.
func parseOCIConfig(data []byte) (*dockerspec.DockerOCIImage, error) {
	var cfg dockerspec.DockerOCIImage
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse OCI config: %w", err)
	}

	return &cfg, nil
}

// manifestConfigDigest extracts the config digest from manifest JSON.
func manifestConfigDigest(manifestData []byte) string {
	var m struct {
		Config struct {
			Digest string `json:"digest"`
		} `json:"config"`
	}

	if json.Unmarshal(manifestData, &m) != nil {
		return ""
	}

	return m.Config.Digest
}

// imageLabels reads labels from the OCI config for the given image.
func (s *Store) imageLabels(id string) map[string]string {
	manifestData, err := s.store.ImageBigData(id, storage.ImageDigestBigDataKey)
	if err != nil {
		return nil
	}

	cfgDigest := manifestConfigDigest(manifestData)
	if cfgDigest == "" {
		return nil
	}

	configData, err := s.store.ImageBigData(id, cfgDigest)
	if err != nil {
		return nil
	}

	var ociCfg dockerspec.DockerOCIImage
	if json.Unmarshal(configData, &ociCfg) != nil {
		return nil
	}

	return ociCfg.Config.Labels
}

// newInsecurePolicyContext creates a signature policy context that accepts everything.
func newInsecurePolicyContext() (*signature.PolicyContext, error) {
	policy, err := signature.NewPolicyFromBytes([]byte(`{"default":[{"type":"insecureAcceptAnything"}]}`))
	if err != nil {
		return nil, fmt.Errorf("signature policy: %w", err)
	}

	ctx, err := signature.NewPolicyContext(policy)
	if err != nil {
		return nil, fmt.Errorf("policy context: %w", err)
	}

	return ctx, nil
}

// splitNames separates image names into repo tags and repo digests.
func splitNames(names []string) (tags, digests []string) {
	for _, name := range names {
		if strings.Contains(name, "@sha256:") {
			digests = append(digests, name)
		} else {
			tags = append(tags, name)
		}
	}

	if tags == nil {
		tags = []string{}
	}

	if digests == nil {
		digests = []string{}
	}

	return tags, digests
}

// normalizeRef normalizes a short image reference to its fully qualified form.
func normalizeRef(nameOrID string) string {
	parsed, err := reference.ParseNormalizedNamed(nameOrID)
	if err != nil {
		return nameOrID
	}

	tagged := reference.TagNameOnly(parsed)

	return tagged.String()
}

// isTagReference returns true if the reference looks like it specifies a tag.
func isTagReference(ref string) bool {
	parsed, err := reference.ParseNormalizedNamed(ref)
	if err != nil {
		return false
	}

	_, isTagged := parsed.(reference.Tagged)

	return isTagged
}
