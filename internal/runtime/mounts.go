package runtime

import (
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/Shikachuu/kogia/internal/volume"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	ocispec "github.com/opencontainers/runtime-spec/specs-go"
)

var (
	// ErrInvalidBind is returned when a bind string has an invalid format.
	ErrInvalidBind = errors.New("mounts: invalid bind format")
	// ErrNoVolumeResolver is returned when a named volume is used without a volume resolver.
	ErrNoVolumeResolver = errors.New("mounts: named volume requires a volume resolver")
	// ErrUnsupportedMountType is returned for unrecognized mount types.
	ErrUnsupportedMountType = errors.New("mounts: unsupported mount type")
)

// VolumeResolver creates or retrieves volumes by name.
type VolumeResolver interface {
	EnsureVolume(name string) (*volume.Record, error)
}

// mountResult holds an OCI mount and the corresponding Docker inspect MountPoint.
type mountResult struct {
	mountPoint container.MountPoint
	oci        ocispec.Mount
}

// buildVolumeMounts processes HostConfig.Binds, HostConfig.Mounts, and Config.Volumes
// into OCI mount specs and Docker inspect MountPoint entries.
func buildVolumeMounts(
	cfg *container.Config,
	hostCfg *container.HostConfig,
	vr VolumeResolver,
) ([]ocispec.Mount, []container.MountPoint, error) {
	seen := make(map[string]*mountResult) // destination → result

	// 1. Parse HostConfig.Binds.
	if hostCfg != nil {
		for _, bind := range hostCfg.Binds {
			mr, err := parseBind(bind, vr)
			if err != nil {
				return nil, nil, fmt.Errorf("mounts: parse bind %q: %w", bind, err)
			}

			seen[mr.oci.Destination] = mr
		}
	}

	// 2. Parse HostConfig.Mounts.
	if hostCfg != nil {
		for i := range hostCfg.Mounts {
			mr, err := parseMount(&hostCfg.Mounts[i], vr)
			if err != nil {
				return nil, nil, fmt.Errorf("mounts: parse mount %q: %w", hostCfg.Mounts[i].Target, err)
			}

			seen[mr.oci.Destination] = mr
		}
	}

	// 3. Handle Config.Volumes (anonymous volumes for uncovered paths).
	if cfg != nil && vr != nil {
		for dest := range cfg.Volumes {
			dest = filepath.Clean(dest)
			if _, exists := seen[dest]; exists {
				continue
			}

			rec, err := vr.EnsureVolume("")
			if err != nil {
				return nil, nil, fmt.Errorf("mounts: anonymous volume for %q: %w", dest, err)
			}

			seen[dest] = &mountResult{
				oci: ocispec.Mount{
					Destination: dest,
					Type:        "bind",
					Source:      rec.Mountpoint,
					Options:     []string{"rbind", "rprivate", "rw"},
				},
				mountPoint: container.MountPoint{
					Type:        mount.TypeVolume,
					Name:        rec.Name,
					Source:      rec.Mountpoint,
					Destination: dest,
					Driver:      "local",
					RW:          true,
				},
			}
		}
	}

	var (
		ociMounts   []ocispec.Mount
		mountPoints []container.MountPoint
	)

	for _, mr := range seen {
		ociMounts = append(ociMounts, mr.oci)
		mountPoints = append(mountPoints, mr.mountPoint)
	}

	return ociMounts, mountPoints, nil
}

// parseBind parses a Docker bind string: "source:dest[:opts]".
func parseBind(bind string, vr VolumeResolver) (*mountResult, error) {
	parts := strings.SplitN(bind, ":", 3)
	if len(parts) < 2 {
		return nil, fmt.Errorf("%w: %q", ErrInvalidBind, bind)
	}

	source := parts[0]
	dest := filepath.Clean(parts[1])

	var optsStr string

	if len(parts) == 3 {
		optsStr = parts[2]
	}

	rw := true
	propagation := "rprivate"

	for _, opt := range strings.Split(optsStr, ",") {
		switch opt {
		case "ro":
			rw = false
		case "rw":
			rw = true
		case "rprivate", "private", "rshared", "shared", "rslave", "slave":
			propagation = opt
		}
	}

	ociOpts := []string{"rbind", propagation}
	if rw {
		ociOpts = append(ociOpts, "rw")
	} else {
		ociOpts = append(ociOpts, "ro")
	}

	mr := &mountResult{
		oci: ocispec.Mount{
			Destination: dest,
			Type:        "bind",
			Options:     ociOpts,
		},
	}

	if strings.HasPrefix(source, "/") || strings.HasPrefix(source, ".") {
		// Host path bind mount.
		mr.oci.Source = source
		mr.mountPoint = container.MountPoint{
			Type:        mount.TypeBind,
			Source:      source,
			Destination: dest,
			RW:          rw,
			Propagation: mount.Propagation(propagation),
		}
	} else {
		// Named volume.
		if vr == nil {
			return nil, fmt.Errorf("%w: %q", ErrNoVolumeResolver, source)
		}

		rec, err := vr.EnsureVolume(source)
		if err != nil {
			return nil, fmt.Errorf("ensure volume %q: %w", source, err)
		}

		mr.oci.Source = rec.Mountpoint
		mr.mountPoint = container.MountPoint{
			Type:        mount.TypeVolume,
			Name:        rec.Name,
			Source:      rec.Mountpoint,
			Destination: dest,
			Driver:      "local",
			RW:          rw,
		}
	}

	return mr, nil
}

// parseMount converts a Docker mount.Mount into an OCI mount and MountPoint.
func parseMount(m *mount.Mount, vr VolumeResolver) (*mountResult, error) {
	dest := filepath.Clean(m.Target)
	rw := !m.ReadOnly

	switch m.Type {
	case mount.TypeBind:
		propagation := mount.PropagationRPrivate
		if m.BindOptions != nil && m.BindOptions.Propagation != "" {
			propagation = m.BindOptions.Propagation
		}

		ociOpts := []string{"rbind", string(propagation)}
		if rw {
			ociOpts = append(ociOpts, "rw")
		} else {
			ociOpts = append(ociOpts, "ro")
		}

		return &mountResult{
			oci: ocispec.Mount{
				Destination: dest,
				Type:        "bind",
				Source:      m.Source,
				Options:     ociOpts,
			},
			mountPoint: container.MountPoint{
				Type:        mount.TypeBind,
				Source:      m.Source,
				Destination: dest,
				RW:          rw,
				Propagation: propagation,
			},
		}, nil

	case mount.TypeVolume:
		if vr == nil {
			return nil, fmt.Errorf("%w: %q", ErrNoVolumeResolver, m.Source)
		}

		rec, err := vr.EnsureVolume(m.Source)
		if err != nil {
			return nil, fmt.Errorf("ensure volume %q: %w", m.Source, err)
		}

		ociOpts := []string{"rbind", "rprivate"}
		if rw {
			ociOpts = append(ociOpts, "rw")
		} else {
			ociOpts = append(ociOpts, "ro")
		}

		return &mountResult{
			oci: ocispec.Mount{
				Destination: dest,
				Type:        "bind",
				Source:      rec.Mountpoint,
				Options:     ociOpts,
			},
			mountPoint: container.MountPoint{
				Type:        mount.TypeVolume,
				Name:        rec.Name,
				Source:      rec.Mountpoint,
				Destination: dest,
				Driver:      "local",
				RW:          rw,
			},
		}, nil

	case mount.TypeTmpfs:
		ociOpts := []string{"nosuid", "nodev", "noexec"}

		if m.TmpfsOptions != nil && m.TmpfsOptions.SizeBytes > 0 {
			ociOpts = append(ociOpts, "size="+strconv.FormatInt(m.TmpfsOptions.SizeBytes, 10))
		}

		if m.TmpfsOptions != nil && m.TmpfsOptions.Mode != 0 {
			ociOpts = append(ociOpts, "mode="+strconv.FormatUint(uint64(m.TmpfsOptions.Mode), 8))
		}

		return &mountResult{
			oci: ocispec.Mount{
				Destination: dest,
				Type:        "tmpfs",
				Source:      "tmpfs",
				Options:     ociOpts,
			},
			mountPoint: container.MountPoint{
				Type:        mount.TypeTmpfs,
				Destination: dest,
				RW:          rw,
			},
		}, nil

	default:
		return nil, fmt.Errorf("%w: %q", ErrUnsupportedMountType, m.Type)
	}
}

// resolveVolumesFrom copies mounts from source containers into the target container's config.
func resolveVolumesFrom(
	volumesFrom []string,
	store containerGetter,
) ([]string, error) {
	var extraBinds []string

	for _, vf := range volumesFrom {
		name, modeStr, _ := strings.Cut(vf, ":")

		record, err := store.GetContainer(name)
		if err != nil {
			return nil, fmt.Errorf("volumes-from %q: %w", name, err)
		}

		if record.HostConfig != nil {
			for _, bind := range record.HostConfig.Binds {
				if modeStr == "ro" {
					// Strip existing rw/ro and force ro.
					bind = forceReadOnly(bind)
				}

				extraBinds = append(extraBinds, bind)
			}
		}
	}

	return extraBinds, nil
}

// containerGetter abstracts container lookup for VolumesFrom resolution.
type containerGetter interface {
	GetContainer(idOrName string) (*container.InspectResponse, error)
}

// forceReadOnly replaces any rw option with ro in a bind string.
func forceReadOnly(bind string) string {
	parts := strings.SplitN(bind, ":", 3)
	if len(parts) < 2 {
		return bind
	}

	if len(parts) == 2 {
		return bind + ":ro"
	}

	// Replace rw with ro in options, or append ro.
	opts := strings.Split(parts[2], ",")
	hasRO := false

	for i, opt := range opts {
		switch opt {
		case "rw":
			opts[i] = "ro"
			hasRO = true
		case "ro":
			hasRO = true
		}
	}

	if !hasRO {
		opts = append(opts, "ro")
	}

	return parts[0] + ":" + parts[1] + ":" + strings.Join(opts, ",")
}
