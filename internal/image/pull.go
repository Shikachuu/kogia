package image

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/containers/image/v5/copy"
	"github.com/containers/image/v5/docker"
	"github.com/containers/image/v5/docker/reference"
	istorage "github.com/containers/image/v5/storage"
	imagetypes "github.com/containers/image/v5/types"
)

// Pull downloads an image from a registry and stores it locally.
// Progress is streamed as NDJSON to the provided writer.
func (s *Store) Pull(ctx context.Context, fromImage, tag string, auth *AuthConfig, w io.Writer, flusher http.Flusher) error {
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

	pw := &progressWriter{w: w, flusher: flusher}

	writeProgress(pw, &progressMsg{Status: "Pulling from " + reference.FamiliarName(ref), ID: tag})

	_, err = copy.Image(ctx, policyCtx, dstRef, srcRef, &copy.Options{
		ReportWriter:   pw,
		SourceCtx:      sysCtx,
		DestinationCtx: &imagetypes.SystemContext{},
	})
	if err != nil {
		return fmt.Errorf("copy image: %w", err)
	}

	writeProgress(pw, &progressMsg{Status: "Status: Downloaded newer image for " + reference.FamiliarString(ref)})

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

// progressMsg is a single NDJSON progress message sent to the Docker client.
type progressMsg struct {
	Status         string       `json:"status"`
	ID             string       `json:"id,omitempty"`
	Progress       string       `json:"progress,omitempty"`
	ProgressDetail interface{}  `json:"progressDetail,omitempty"`
	ErrorDetail    *errorDetail `json:"errorDetail,omitempty"`
	Error          string       `json:"error,omitempty"`
}

type errorDetail struct {
	Message string `json:"message"`
}

// progressWriter wraps containers/image progress text into NDJSON objects.
type progressWriter struct {
	w       io.Writer
	flusher http.Flusher
	buf     []byte
}

func (pw *progressWriter) Write(p []byte) (int, error) {
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

		writeProgress(pw, &progressMsg{Status: line})
	}

	return len(p), nil
}

func writeProgress(pw *progressWriter, msg *progressMsg) {
	data, err := json.Marshal(msg)
	if err != nil {
		return
	}

	_, _ = pw.w.Write(data)
	_, _ = pw.w.Write([]byte("\n"))

	if pw.flusher != nil {
		pw.flusher.Flush()
	}
}

// WriteError writes an error as an NDJSON progress message.
func WriteError(w io.Writer, flusher http.Flusher, err error) {
	pw := &progressWriter{w: w, flusher: flusher}
	writeProgress(pw, &progressMsg{
		ErrorDetail: &errorDetail{Message: err.Error()},
		Error:       err.Error(),
	})
}
