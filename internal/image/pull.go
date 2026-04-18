package image

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/Shikachuu/kogia/internal/api/stream"
	"github.com/containers/image/v5/copy"
	"github.com/containers/image/v5/docker"
	"github.com/containers/image/v5/docker/reference"
	istorage "github.com/containers/image/v5/storage"
	imagetypes "github.com/containers/image/v5/types"
)

// Pull downloads an image from a registry and stores it locally.
// Progress is streamed as NDJSON to the provided writer.
// The context is intentionally detached from the HTTP request so that a
// client disconnect does not cancel an in-flight layer copy.
func (s *Store) Pull(_ context.Context, fromImage, tag string, auth *AuthConfig, nw *stream.NDJSONWriter) error {
	ref, err := buildRef(fromImage, tag)
	if err != nil {
		return fmt.Errorf("invalid reference: %w", err)
	}

	refStr := ref.String()

	// Source: docker registry.
	srcRef, err := docker.NewReference(ref)
	if err != nil {
		return fmt.Errorf("source reference: %w", err)
	}

	// Destination: local containers/storage.
	dstRef, err := istorage.Transport.ParseStoreReference(s.store, refStr)
	if err != nil {
		return fmt.Errorf("destination reference: %w", err)
	}

	policyCtx, err := newInsecurePolicyContext()
	if err != nil {
		return err
	}

	defer func() { _ = policyCtx.Destroy() }()

	// System context with optional auth.
	sysCtx := &imagetypes.SystemContext{}
	if auth != nil && !auth.IsEmpty() {
		sysCtx.DockerAuthConfig = &imagetypes.DockerAuthConfig{
			Username:      auth.Username,
			Password:      auth.Password,
			IdentityToken: auth.IdentityToken,
		}
	}

	pw := &progressWriter{nw: nw}

	_ = nw.Encode(&stream.ProgressMsg{Status: "Pulling from " + reference.FamiliarName(ref), ID: tag})

	_, err = copy.Image(context.Background(), policyCtx, dstRef, srcRef, &copy.Options{ //nolint:contextcheck // Detach from HTTP request so client disconnect does not cancel the pull.
		ReportWriter:   pw,
		SourceCtx:      sysCtx,
		DestinationCtx: &imagetypes.SystemContext{},
	})
	if err != nil {
		return fmt.Errorf("copy image: %w", err)
	}

	_ = nw.Encode(&stream.ProgressMsg{Status: "Status: Downloaded newer image for " + reference.FamiliarString(ref)})

	return nil
}

// buildRef parses and normalizes fromImage and tag into a docker reference.
func buildRef(fromImage, tag string) (reference.Named, error) {
	// If fromImage already has a tag or digest, use as-is.
	parsed, err := reference.ParseNormalizedNamed(fromImage)
	if err != nil {
		return nil, fmt.Errorf("parse reference %q: %w", fromImage, err)
	}

	// If a tag was provided and the reference doesn't already have one, add it.
	if tag != "" {
		if _, hasTag := parsed.(reference.Tagged); !hasTag {
			if _, hasDigest := parsed.(reference.Digested); !hasDigest {
				withTag, tagErr := reference.WithTag(parsed, tag)
				if tagErr != nil {
					return nil, fmt.Errorf("add tag %q: %w", tag, tagErr)
				}

				return withTag, nil
			}
		}
	}

	// Default to :latest if no tag or digest.
	return reference.TagNameOnly(parsed), nil
}

// progressWriter wraps containers/image progress text into NDJSON objects.
// It is safe for concurrent use because containers/image copies layers in
// parallel goroutines that all write to the same ReportWriter.
type progressWriter struct {
	nw  *stream.NDJSONWriter
	buf []byte
	mu  sync.Mutex
}

func (pw *progressWriter) Write(p []byte) (int, error) {
	pw.mu.Lock()
	defer pw.mu.Unlock()

	pw.buf = append(pw.buf, p...)

	for {
		idx := strings.IndexByte(string(pw.buf), '\n')
		if idx < 0 {
			break
		}

		line := strings.TrimSpace(string(pw.buf[:idx]))
		pw.buf = pw.buf[idx+1:]

		if line == "" {
			continue
		}

		_ = pw.nw.Encode(&stream.ProgressMsg{Status: line})
	}

	return len(p), nil
}
