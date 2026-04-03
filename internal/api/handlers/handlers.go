// Package handlers implements the Docker Engine API handler interfaces.
package handlers

import (
	"github.com/Shikachuu/kogia/internal/api/gen"
	"github.com/Shikachuu/kogia/internal/store"
)

// Handlers implements gen.Handler with moby types.
// Unimplemented endpoints fall through to gen.NotImplemented (501).
type Handlers struct {
	gen.NotImplemented
	store            *store.Store
	version          string
	commit           string
	date             string
	dockerAPIVersion string
}

// New creates a new Handlers instance.
func New(s *store.Store, version, commit, date, dockerAPIVersion string) *Handlers {
	return &Handlers{
		store:            s,
		version:          version,
		commit:           commit,
		date:             date,
		dockerAPIVersion: dockerAPIVersion,
	}
}
