package image

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/containers/image/v5/docker"
	"github.com/containers/image/v5/docker/reference"
	"github.com/containers/image/v5/manifest"
	imagetypes "github.com/containers/image/v5/types"
	digest "github.com/opencontainers/go-digest"
	imgspecv1 "github.com/opencontainers/image-spec/specs-go/v1"
)

// DistributionDescriptor describes a manifest in the registry.
type DistributionDescriptor struct {
	MediaType string        `json:"mediaType"`
	Digest    digest.Digest `json:"digest"`
	Size      int64         `json:"size"`
}

// PlatformSpec describes a platform for a multi-arch manifest.
type PlatformSpec struct {
	Architecture string `json:"architecture"`
	OS           string `json:"os"`
	Variant      string `json:"variant,omitempty"`
	OSVersion    string `json:"os.version,omitempty"`
}

// DistributionInspectResponse is the response for GET /distribution/{name}/json.
type DistributionInspectResponse struct {
	Descriptor DistributionDescriptor `json:"Descriptor"`
	Platforms  []PlatformSpec         `json:"Platforms"`
}

// DistributionInspect queries a registry for manifest metadata without pulling layers.
func (s *Store) DistributionInspect(ctx context.Context, name string, auth *AuthConfig) (*DistributionInspectResponse, error) {
	ref, err := reference.ParseNormalizedNamed(name)
	if err != nil {
		return nil, fmt.Errorf("image: distribution inspect: parse %q: %w", name, err)
	}

	ref = reference.TagNameOnly(ref)

	srcRef, err := docker.NewReference(ref)
	if err != nil {
		return nil, fmt.Errorf("image: distribution inspect: docker ref: %w", err)
	}

	sysCtx := &imagetypes.SystemContext{}
	if auth != nil && !auth.IsEmpty() {
		sysCtx.DockerAuthConfig = &imagetypes.DockerAuthConfig{
			Username:      auth.Username,
			Password:      auth.Password,
			IdentityToken: auth.IdentityToken,
		}
	}

	src, err := srcRef.NewImageSource(ctx, sysCtx)
	if err != nil {
		return nil, fmt.Errorf("image: distribution inspect: open source: %w", err)
	}

	defer func() { _ = src.Close() }()

	manifestBytes, mimeType, err := src.GetManifest(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("image: distribution inspect: get manifest: %w", err)
	}

	d := digest.FromBytes(manifestBytes)

	resp := &DistributionInspectResponse{
		Descriptor: DistributionDescriptor{
			MediaType: mimeType,
			Digest:    d,
			Size:      int64(len(manifestBytes)),
		},
	}

	resp.Platforms = extractPlatforms(manifestBytes, mimeType)

	return resp, nil
}

// extractPlatforms extracts platform information from the manifest.
// For manifest lists/indexes, platforms come from the list entries.
// For single manifests, we try to parse the config to get arch/os.
func extractPlatforms(manifestBytes []byte, mimeType string) []PlatformSpec {
	if manifest.MIMETypeIsMultiImage(mimeType) {
		return extractPlatformsFromIndex(manifestBytes)
	}

	return extractPlatformFromSingle(manifestBytes)
}

// extractPlatformsFromIndex parses an OCI index or Docker manifest list.
func extractPlatformsFromIndex(manifestBytes []byte) []PlatformSpec {
	// Try OCI index first.
	var index imgspecv1.Index
	if err := json.Unmarshal(manifestBytes, &index); err == nil && len(index.Manifests) > 0 {
		platforms := make([]PlatformSpec, 0, len(index.Manifests))

		for _, m := range index.Manifests {
			if m.Platform != nil {
				platforms = append(platforms, PlatformSpec{
					Architecture: m.Platform.Architecture,
					OS:           m.Platform.OS,
					Variant:      m.Platform.Variant,
					OSVersion:    m.Platform.OSVersion,
				})
			}
		}

		return platforms
	}

	// Try Docker manifest list.
	var dockerList dockerManifestList

	if err := json.Unmarshal(manifestBytes, &dockerList); err == nil {
		platforms := make([]PlatformSpec, 0, len(dockerList.Manifests))

		for _, m := range dockerList.Manifests {
			platforms = append(platforms, PlatformSpec{
				Architecture: m.Platform.Architecture,
				OS:           m.Platform.OS,
				Variant:      m.Platform.Variant,
				OSVersion:    m.Platform.OSVersion,
			})
		}

		return platforms
	}

	return nil
}

// extractPlatformFromSingle tries to get arch/os from a single manifest's config.
func extractPlatformFromSingle(manifestBytes []byte) []PlatformSpec {
	var m struct {
		Config struct {
			Digest string `json:"digest"`
		} `json:"config"`
	}

	if json.Unmarshal(manifestBytes, &m) != nil || m.Config.Digest == "" {
		return nil
	}

	// We can't fetch the config blob without additional network calls.
	// Return nil — the caller gets the descriptor without platform info.
	return nil
}

// dockerManifestList is the Docker schema2 manifest list format.
type dockerManifestList struct {
	Manifests []dockerManifestEntry `json:"manifests"`
}

type dockerManifestEntry struct {
	Platform dockerManifestPlatform `json:"platform"`
}

type dockerManifestPlatform struct {
	Architecture string `json:"architecture"`
	OS           string `json:"os"`
	Variant      string `json:"variant,omitempty"`
	OSVersion    string `json:"os.version,omitempty"`
}
