// Package embed provides embedded crun static binaries for linux/amd64 and linux/arm64.
package embed

import (
	_ "embed"
	"errors"
	"runtime"
)

// ErrUnsupportedArch is returned when no crun binary is available for the current architecture.
var ErrUnsupportedArch = errors.New("embed: unsupported architecture")

//go:embed crun_linux_amd64
var crunAmd64 []byte

//go:embed crun_linux_arm64
var crunArm64 []byte

// Crun returns the embedded static crun binary for the current architecture.
func Crun() ([]byte, error) {
	switch runtime.GOARCH {
	case "amd64":
		return crunAmd64, nil
	case "arm64":
		return crunArm64, nil
	default:
		return nil, ErrUnsupportedArch
	}
}
