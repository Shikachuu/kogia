package runtime

import (
	"testing"

	"github.com/Shikachuu/kogia/internal/volume"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
)

// mockVolumeResolver returns predictable volume records.
type mockVolumeResolver struct {
	vols map[string]*volume.Record
	next int
}

func newMockVolumeResolver() *mockVolumeResolver {
	return &mockVolumeResolver{vols: make(map[string]*volume.Record)}
}

func (m *mockVolumeResolver) EnsureVolume(name string) (*volume.Record, error) {
	if name == "" {
		m.next++
		name = "anon_" + string(rune('0'+m.next))
	}

	if rec, ok := m.vols[name]; ok {
		return rec, nil
	}

	rec := &volume.Record{
		Name:       name,
		Driver:     "local",
		Mountpoint: "/var/lib/kogia/volumes/" + name + "/_data",
	}
	m.vols[name] = rec

	return rec, nil
}

func TestBuildVolumeMounts_Binds(t *testing.T) {
	t.Parallel()

	vr := newMockVolumeResolver()

	tests := []struct {
		name     string
		bind     string
		wantDest string
		wantType mount.Type
		wantRW   bool
	}{
		{
			name:     "host path bind",
			bind:     "/host/data:/container/data",
			wantDest: "/container/data",
			wantRW:   true,
			wantType: mount.TypeBind,
		},
		{
			name:     "host path read-only",
			bind:     "/host/data:/container/data:ro",
			wantDest: "/container/data",
			wantRW:   false,
			wantType: mount.TypeBind,
		},
		{
			name:     "named volume",
			bind:     "myvol:/data",
			wantDest: "/data",
			wantRW:   true,
			wantType: mount.TypeVolume,
		},
		{
			name:     "named volume read-only",
			bind:     "myvol:/data:ro",
			wantDest: "/data",
			wantRW:   false,
			wantType: mount.TypeVolume,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			localVR := newMockVolumeResolver()

			ociMounts, mps, err := buildVolumeMounts(
				nil,
				&container.HostConfig{Binds: []string{tt.bind}},
				localVR,
			)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if len(ociMounts) != 1 {
				t.Fatalf("expected 1 OCI mount, got %d", len(ociMounts))
			}

			if ociMounts[0].Destination != tt.wantDest {
				t.Errorf("dest = %q, want %q", ociMounts[0].Destination, tt.wantDest)
			}

			if len(mps) != 1 {
				t.Fatalf("expected 1 mount point, got %d", len(mps))
			}

			if mps[0].RW != tt.wantRW {
				t.Errorf("rw = %v, want %v", mps[0].RW, tt.wantRW)
			}

			if mps[0].Type != tt.wantType {
				t.Errorf("type = %q, want %q", mps[0].Type, tt.wantType)
			}
		})
	}

	_ = vr // parent resolver not used in subtests
}

func TestBuildVolumeMounts_Mounts(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		mnt      mount.Mount
		wantDest string
		wantType mount.Type
	}{
		{
			name: "bind mount",
			mnt: mount.Mount{
				Type:   mount.TypeBind,
				Source: "/host/path",
				Target: "/container/path",
			},
			wantDest: "/container/path",
			wantType: mount.TypeBind,
		},
		{
			name: "volume mount",
			mnt: mount.Mount{
				Type:   mount.TypeVolume,
				Source: "namedvol",
				Target: "/data",
			},
			wantDest: "/data",
			wantType: mount.TypeVolume,
		},
		{
			name: "tmpfs mount",
			mnt: mount.Mount{
				Type:   mount.TypeTmpfs,
				Target: "/tmp",
			},
			wantDest: "/tmp",
			wantType: mount.TypeTmpfs,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			vr := newMockVolumeResolver()

			ociMounts, mps, err := buildVolumeMounts(
				nil,
				&container.HostConfig{Mounts: []mount.Mount{tt.mnt}},
				vr,
			)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if len(ociMounts) != 1 {
				t.Fatalf("expected 1 OCI mount, got %d", len(ociMounts))
			}

			if ociMounts[0].Destination != tt.wantDest {
				t.Errorf("dest = %q, want %q", ociMounts[0].Destination, tt.wantDest)
			}

			if len(mps) != 1 {
				t.Fatalf("expected 1 mount point, got %d", len(mps))
			}

			if mps[0].Type != tt.wantType {
				t.Errorf("type = %q, want %q", mps[0].Type, tt.wantType)
			}
		})
	}
}

func TestBuildVolumeMounts_ConfigVolumes(t *testing.T) {
	t.Parallel()

	vr := newMockVolumeResolver()

	cfg := &container.Config{
		Volumes: map[string]struct{}{
			"/data":  {},
			"/cache": {},
		},
	}

	ociMounts, mps, err := buildVolumeMounts(cfg, nil, vr)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(ociMounts) != 2 {
		t.Fatalf("expected 2 OCI mounts (anonymous volumes), got %d", len(ociMounts))
	}

	if len(mps) != 2 {
		t.Fatalf("expected 2 mount points, got %d", len(mps))
	}

	for _, mp := range mps {
		if mp.Type != mount.TypeVolume {
			t.Errorf("expected volume type, got %q", mp.Type)
		}

		if mp.Driver != "local" {
			t.Errorf("expected local driver, got %q", mp.Driver)
		}
	}
}

func TestBuildVolumeMounts_MergeDedup(t *testing.T) {
	t.Parallel()

	vr := newMockVolumeResolver()

	cfg := &container.Config{
		Volumes: map[string]struct{}{
			"/data": {}, // This should NOT create an anonymous volume.
		},
	}

	hostCfg := &container.HostConfig{
		Binds: []string{"myvol:/data"}, // Explicit bind takes priority.
	}

	ociMounts, mps, err := buildVolumeMounts(cfg, hostCfg, vr)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(ociMounts) != 1 {
		t.Fatalf("expected 1 OCI mount (deduped), got %d", len(ociMounts))
	}

	if len(mps) != 1 {
		t.Fatalf("expected 1 mount point, got %d", len(mps))
	}

	if mps[0].Name != "myvol" {
		t.Errorf("expected named volume 'myvol', got %q", mps[0].Name)
	}
}

func TestForceReadOnly(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		bind string
		want string
	}{
		{
			name: "add ro to bare bind",
			bind: "/host:/container",
			want: "/host:/container:ro",
		},
		{
			name: "replace rw with ro",
			bind: "/host:/container:rw",
			want: "/host:/container:ro",
		},
		{
			name: "keep existing ro",
			bind: "/host:/container:ro",
			want: "/host:/container:ro",
		},
		{
			name: "replace rw in multi-opts",
			bind: "/host:/container:rw,rprivate",
			want: "/host:/container:ro,rprivate",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := forceReadOnly(tt.bind)
			if got != tt.want {
				t.Errorf("forceReadOnly(%q) = %q, want %q", tt.bind, got, tt.want)
			}
		})
	}
}
