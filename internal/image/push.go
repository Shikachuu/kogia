package image

import (
	"context"
	"fmt"
	"io"
	"net/http"

	"github.com/containers/image/v5/copy"
	"github.com/containers/image/v5/docker"
	"github.com/containers/image/v5/docker/reference"
	istorage "github.com/containers/image/v5/storage"
	imagetypes "github.com/containers/image/v5/types"
)

// Push uploads an image to a registry.
// Progress is streamed as NDJSON to the provided writer.
func (s *Store) Push(ctx context.Context, nameOrID, tag string, auth *AuthConfig, w io.Writer, flusher http.Flusher) error {
	// Try resolving with tag appended first — Docker CLI sends name and tag separately.
	img, err := s.resolveWithTag(nameOrID, tag)
	if err != nil {
		return err
	}

	// Find a suitable reference for the push destination.
	ref, err := pushRef(img.Names, nameOrID, tag)
	if err != nil {
		return err
	}

	// Source: local storage.
	srcRef, err := istorage.Transport.ParseStoreReference(s.store, img.ID)
	if err != nil {
		return fmt.Errorf("push source: %w", err)
	}

	// Destination: docker registry.
	dstRef, err := docker.NewReference(ref)
	if err != nil {
		return fmt.Errorf("push destination: %w", err)
	}

	policyCtx, err := newInsecurePolicyContext()
	if err != nil {
		return err
	}

	defer func() { _ = policyCtx.Destroy() }()

	sysCtx := &imagetypes.SystemContext{}
	if auth != nil && !auth.IsEmpty() {
		sysCtx.DockerAuthConfig = &imagetypes.DockerAuthConfig{
			Username:      auth.Username,
			Password:      auth.Password,
			IdentityToken: auth.IdentityToken,
		}
	}

	pw := &progressWriter{w: w, flusher: flusher}

	writeProgress(pw, &progressMsg{Status: "The push refers to repository [" + reference.FamiliarName(ref) + "]"})

	_, err = copy.Image(ctx, policyCtx, dstRef, srcRef, &copy.Options{
		ReportWriter:   pw,
		SourceCtx:      &imagetypes.SystemContext{},
		DestinationCtx: sysCtx,
	})
	if err != nil {
		return fmt.Errorf("push image: %w", err)
	}

	writeProgress(pw, &progressMsg{Status: "Pushed " + reference.FamiliarString(ref)})

	return nil
}

// pushRef determines the registry reference to push to from the image's names.
func pushRef(names []string, nameOrID, tag string) (reference.Named, error) {
	// Try the provided nameOrID first.
	parsed, err := reference.ParseNormalizedNamed(nameOrID)
	if err == nil {
		if tag != "" {
			if _, hasTag := parsed.(reference.Tagged); !hasTag {
				tagged, tagErr := reference.WithTag(parsed, tag)
				if tagErr == nil {
					return tagged, nil
				}
			}
		}

		return reference.TagNameOnly(parsed), nil
	}

	// Fall back to the first tagged name on the image.
	for _, n := range names {
		named, nameErr := reference.ParseNormalizedNamed(n)
		if nameErr != nil {
			continue
		}

		if _, isTagged := named.(reference.Tagged); isTagged {
			return named, nil
		}

		return reference.TagNameOnly(named), nil
	}

	return nil, fmt.Errorf("%w: no pushable reference for %s", ErrNotFound, nameOrID)
}
