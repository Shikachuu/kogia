package image

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/Shikachuu/kogia/internal/api/stream"
	"github.com/containers/image/v5/copy"
	"github.com/containers/image/v5/docker/archive"
	istorage "github.com/containers/image/v5/storage"
	imagetypes "github.com/containers/image/v5/types"
)

// Load imports images from a Docker archive tar stream.
// Progress is streamed as NDJSON to the provided writer.
func (s *Store) Load(ctx context.Context, input io.Reader, nw *stream.NDJSONWriter) error {
	tmp, err := os.CreateTemp("", "kogia-load-*.tar")
	if err != nil {
		return fmt.Errorf("image load: create temp: %w", err)
	}

	tmpPath := tmp.Name()

	defer func() { _ = os.Remove(tmpPath) }()

	_, copyErr := io.Copy(tmp, input)
	if copyErr != nil {
		_ = tmp.Close()

		return fmt.Errorf("image load: buffer input: %w", copyErr)
	}

	closeErr := tmp.Close()
	if closeErr != nil {
		return fmt.Errorf("image load: close temp: %w", closeErr)
	}

	reader, err := archive.NewReader(nil, tmpPath)
	if err != nil {
		return fmt.Errorf("image load: open archive: %w", err)
	}

	defer func() { _ = reader.Close() }()

	imageList, err := reader.List()
	if err != nil {
		return fmt.Errorf("image load: list images: %w", err)
	}

	policyCtx, err := newInsecurePolicyContext()
	if err != nil {
		return err
	}

	defer func() { _ = policyCtx.Destroy() }()

	pw := &progressWriter{nw: nw}

	for _, refs := range imageList {
		if len(refs) == 0 {
			continue
		}

		srcRef := refs[0]
		refName := srcRef.DockerReference()

		var refStr string
		if refName != nil {
			refStr = refName.String()
		} else {
			refStr = srcRef.StringWithinTransport()
		}

		_ = nw.Encode(&stream.ProgressMsg{Status: "Loading layer for " + refStr})

		dstRef, dstErr := istorage.Transport.ParseStoreReference(s.store, refStr)
		if dstErr != nil {
			return fmt.Errorf("image load: destination for %s: %w", refStr, dstErr)
		}

		_, imgCopyErr := copy.Image(ctx, policyCtx, dstRef, srcRef, &copy.Options{
			ReportWriter:   pw,
			SourceCtx:      &imagetypes.SystemContext{},
			DestinationCtx: &imagetypes.SystemContext{},
		})
		if imgCopyErr != nil {
			return fmt.Errorf("image load: copy %s: %w", refStr, imgCopyErr)
		}

		_ = nw.Encode(&stream.ProgressMsg{Status: "Loaded image: " + refStr})
	}

	return nil
}
