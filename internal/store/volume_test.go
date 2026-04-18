package store

import (
	"testing"
	"time"

	"github.com/Shikachuu/kogia/internal/volume"
)

func TestStore_VolumeCreateGet(t *testing.T) {
	t.Parallel()

	s := newTestStore(t)

	rec := &volume.Record{
		Name:       "testvol",
		Driver:     "local",
		Mountpoint: "/var/lib/kogia/volumes/testvol/_data",
		CreatedAt:  time.Now().UTC(),
	}

	if err := s.CreateVolume(rec); err != nil {
		t.Fatalf("create: %v", err)
	}

	got, err := s.GetVolume("testvol")
	if err != nil {
		t.Fatalf("get: %v", err)
	}

	if got.Name != rec.Name {
		t.Errorf("name = %q, want %q", got.Name, rec.Name)
	}

	if got.Driver != rec.Driver {
		t.Errorf("driver = %q, want %q", got.Driver, rec.Driver)
	}

	if got.Mountpoint != rec.Mountpoint {
		t.Errorf("mountpoint = %q, want %q", got.Mountpoint, rec.Mountpoint)
	}
}

func TestStore_VolumeCreateDuplicate(t *testing.T) {
	t.Parallel()

	s := newTestStore(t)

	rec := &volume.Record{
		Name:       "dupvol",
		Driver:     "local",
		Mountpoint: "/var/lib/kogia/volumes/dupvol/_data",
		CreatedAt:  time.Now().UTC(),
	}

	if err := s.CreateVolume(rec); err != nil {
		t.Fatalf("create first: %v", err)
	}

	err := s.CreateVolume(rec)
	if err == nil {
		t.Fatal("expected ErrVolumeNameInUse, got nil")
	}
}

func TestStore_VolumeDelete(t *testing.T) {
	t.Parallel()

	s := newTestStore(t)

	rec := &volume.Record{
		Name:       "delvol",
		Driver:     "local",
		Mountpoint: "/var/lib/kogia/volumes/delvol/_data",
		CreatedAt:  time.Now().UTC(),
	}

	if err := s.CreateVolume(rec); err != nil {
		t.Fatalf("create: %v", err)
	}

	if err := s.DeleteVolume("delvol"); err != nil {
		t.Fatalf("delete: %v", err)
	}

	_, err := s.GetVolume("delvol")
	if err == nil {
		t.Error("expected error after delete, got nil")
	}
}

func TestStore_VolumeList(t *testing.T) {
	t.Parallel()

	s := newTestStore(t)

	// Empty list.
	vols, err := s.ListVolumes()
	if err != nil {
		t.Fatalf("list empty: %v", err)
	}

	if len(vols) != 0 {
		t.Errorf("expected 0 volumes, got %d", len(vols))
	}

	// Add three volumes.
	for _, name := range []string{"vol1", "vol2", "vol3"} {
		rec := &volume.Record{
			Name:       name,
			Driver:     "local",
			Mountpoint: "/var/lib/kogia/volumes/" + name + "/_data",
			CreatedAt:  time.Now().UTC(),
		}

		if createErr := s.CreateVolume(rec); createErr != nil {
			t.Fatalf("create %s: %v", name, createErr)
		}
	}

	vols, err = s.ListVolumes()
	if err != nil {
		t.Fatalf("list: %v", err)
	}

	if len(vols) != 3 {
		t.Errorf("expected 3 volumes, got %d", len(vols))
	}
}

func TestStore_VolumeGetNotFound(t *testing.T) {
	t.Parallel()

	s := newTestStore(t)

	_, err := s.GetVolume("nonexistent")
	if err == nil {
		t.Error("expected error for nonexistent volume")
	}
}
