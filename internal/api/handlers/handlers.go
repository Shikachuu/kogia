// Package handlers implements the Docker Engine API handler interfaces.
package handlers

import (
	"github.com/Shikachuu/kogia/internal/api/gen"
	"github.com/Shikachuu/kogia/internal/image"
	"github.com/Shikachuu/kogia/internal/runtime"
	"github.com/Shikachuu/kogia/internal/store"
)

// Handlers implements gen.Handler with moby types.
// Unimplemented endpoints fall through to gen.NotImplemented (501).
type Handlers struct {
	gen.NotImplemented
	store            *store.Store
	images           *image.Store
	runtime          *runtime.Manager
	version          string
	commit           string
	date             string
	dockerAPIVersion string
}

// New creates a new Handlers instance.
func New(s *store.Store, images *image.Store, rt *runtime.Manager, version, commit, date, dockerAPIVersion string) *Handlers {
	return &Handlers{
		store:            s,
		images:           images,
		runtime:          rt,
		version:          version,
		commit:           commit,
		date:             date,
		dockerAPIVersion: dockerAPIVersion,
	}
}
