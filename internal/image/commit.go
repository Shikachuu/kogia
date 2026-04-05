package image

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/containers/image/v5/docker/reference"
	"github.com/containers/image/v5/manifest"
	"github.com/containers/storage"
	dockerspec "github.com/moby/docker-image-spec/specs-go/v1"
	digest "github.com/opencontainers/go-digest"
	imgspec "github.com/opencontainers/image-spec/specs-go"
	imgspecv1 "github.com/opencontainers/image-spec/specs-go/v1"
)

// ErrNoDiffDigest is returned when a container layer has no diff digest.
var ErrNoDiffDigest = errors.New("image: commit: no diff digest for layer")

// CommitConfig holds optional config overrides for committing a container as an image.
type CommitConfig struct {
	ExposedPorts map[string]struct{}
	Volumes      map[string]struct{}
	Labels       map[string]string
	WorkingDir   string
	User         string
	Cmd          []string
	Entrypoint   []string
	Env          []string
}

// Commit creates a new image from a container's read-write layer.
// The containerID must be the containers/storage container ID (same as the
// kogia container ID). imageID is the base image ID.
func (s *Store) Commit(containerID, imageID, repo, tag, comment, author string, cfg *CommitConfig) (string, error) {
	ctr, err := s.store.Container(containerID)
	if err != nil {
		return "", fmt.Errorf("image: commit: lookup container: %w", err)
	}

	baseImg, err := s.store.Image(imageID)
	if err != nil {
		return "", fmt.Errorf("image: commit: lookup base image: %w", err)
	}

	baseCfg, err := s.readOCIConfig(baseImg)
	if err != nil {
		return "", fmt.Errorf("image: commit: read base config: %w", err)
	}

	// Get the diff ID for the container's RW layer.
	layer, err := s.store.Layer(ctr.LayerID)
	if err != nil {
		return "", fmt.Errorf("image: commit: lookup layer: %w", err)
	}

	diffID := layer.UncompressedDigest
	if diffID == "" {
		diffID = layer.TOCDigest
	}

	if diffID == "" {
		return "", fmt.Errorf("%w: %s", ErrNoDiffDigest, ctr.LayerID)
	}

	// Build new OCI image config from base + overrides.
	now := time.Now().UTC()
	newCfg := buildCommitImageConfig(baseCfg, cfg, comment, author, diffID, now)

	cfgJSON, err := json.Marshal(newCfg)
	if err != nil {
		return "", fmt.Errorf("image: commit: marshal config: %w", err)
	}

	cfgDigest := digest.FromBytes(cfgJSON)

	// Build OCI manifest.
	ociManifest := imgspecv1.Manifest{
		Versioned: imgspec.Versioned{SchemaVersion: 2},
		MediaType: imgspecv1.MediaTypeImageManifest,
		Config: imgspecv1.Descriptor{
			MediaType: imgspecv1.MediaTypeImageConfig,
			Digest:    cfgDigest,
			Size:      int64(len(cfgJSON)),
		},
	}

	manifestJSON, err := json.Marshal(ociManifest)
	if err != nil {
		return "", fmt.Errorf("image: commit: marshal manifest: %w", err)
	}

	// Build image name(s).
	var names []string

	if repo != "" {
		if tag == "" {
			tag = "latest"
		}

		parsed, parseErr := reference.ParseNormalizedNamed(repo + ":" + tag)
		if parseErr != nil {
			return "", fmt.Errorf("image: commit: invalid tag %q: %w", repo+":"+tag, parseErr)
		}

		tagged := reference.TagNameOnly(parsed)
		names = append(names, tagged.String())
	}

	// Create the image referencing the container's RW layer.
	img, err := s.store.CreateImage("", names, ctr.LayerID, "", nil)
	if err != nil {
		return "", fmt.Errorf("image: commit: create image: %w", err)
	}

	// Store manifest and config as big data so inspect/history work.
	if err = s.store.SetImageBigData(img.ID, storage.ImageDigestBigDataKey, manifestJSON, manifest.Digest); err != nil {
		return "", fmt.Errorf("image: commit: store manifest: %w", err)
	}

	if err = s.store.SetImageBigData(img.ID, cfgDigest.String(), cfgJSON, manifest.Digest); err != nil {
		return "", fmt.Errorf("image: commit: store config: %w", err)
	}

	return img.ID, nil
}

// buildCommitImageConfig creates a new DockerOCIImage by cloning the base config
// and applying commit overrides.
func buildCommitImageConfig(
	base *dockerspec.DockerOCIImage,
	cfg *CommitConfig,
	comment, author string,
	diffID digest.Digest,
	now time.Time,
) *dockerspec.DockerOCIImage {
	newImg := &dockerspec.DockerOCIImage{
		Image: imgspecv1.Image{
			Created: &now,
			Platform: imgspecv1.Platform{
				Architecture: base.Architecture,
				OS:           base.OS,
			},
			RootFS:  base.RootFS,
			History: append([]imgspecv1.History{}, base.History...),
		},
		Config: base.Config,
	}

	if author != "" {
		newImg.Author = author
	} else {
		newImg.Author = base.Author
	}

	// Append the new layer's diff ID.
	newImg.RootFS.DiffIDs = append(newImg.RootFS.DiffIDs, diffID)

	// Add history entry.
	newImg.History = append(newImg.History, imgspecv1.History{
		Created:   &now,
		CreatedBy: comment,
		Comment:   comment,
		Author:    newImg.Author,
	})

	// Apply config overrides.
	if cfg != nil {
		applyCommitOverrides(&newImg.Config, cfg)
	}

	return newImg
}

// applyCommitOverrides merges CommitConfig fields into the Docker OCI image config.
func applyCommitOverrides(c *dockerspec.DockerOCIImageConfig, cfg *CommitConfig) {
	if cfg.Cmd != nil {
		c.Cmd = cfg.Cmd
	}

	if cfg.Entrypoint != nil {
		c.Entrypoint = cfg.Entrypoint
	}

	if cfg.Env != nil {
		c.Env = cfg.Env
	}

	if cfg.ExposedPorts != nil {
		c.ExposedPorts = cfg.ExposedPorts
	}

	if cfg.Volumes != nil {
		c.Volumes = cfg.Volumes
	}

	if cfg.WorkingDir != "" {
		c.WorkingDir = cfg.WorkingDir
	}

	if cfg.Labels != nil {
		c.Labels = cfg.Labels
	}

	if cfg.User != "" {
		c.User = cfg.User
	}
}
