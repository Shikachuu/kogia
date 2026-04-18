package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/Shikachuu/kogia/internal/volume"
	"github.com/moby/moby/api/types/container"
	bolt "go.etcd.io/bbolt"
)

// ErrVolumeNameInUse is returned when a volume name is already taken.
var ErrVolumeNameInUse = errors.New("store: volume name already in use")

const (
	bucketVolumes     = "volumes"
	bucketVolumeNames = "volume-names"
)

// CreateVolume persists a new volume record.
func (s *Store) CreateVolume(rec *volume.Record) error {
	data, marshalErr := json.Marshal(rec)
	if marshalErr != nil {
		return fmt.Errorf("store: marshal volume: %w", marshalErr)
	}

	if err := s.db.Update(func(tx *bolt.Tx) error {
		namesBucket := tx.Bucket([]byte(bucketVolumeNames))
		if v := namesBucket.Get([]byte(rec.Name)); v != nil {
			return fmt.Errorf("%w: %s", ErrVolumeNameInUse, rec.Name)
		}

		if putErr := tx.Bucket([]byte(bucketVolumes)).Put([]byte(rec.Name), data); putErr != nil {
			return fmt.Errorf("put volume: %w", putErr)
		}

		if putErr := namesBucket.Put([]byte(rec.Name), []byte(rec.Name)); putErr != nil {
			return fmt.Errorf("put volume name: %w", putErr)
		}

		return nil
	}); err != nil {
		return fmt.Errorf("store: create volume: %w", err)
	}

	return nil
}

// GetVolume retrieves a volume by name.
func (s *Store) GetVolume(name string) (*volume.Record, error) {
	var rec volume.Record

	err := s.db.View(func(tx *bolt.Tx) error {
		data := tx.Bucket([]byte(bucketVolumes)).Get([]byte(name))
		if data == nil {
			return fmt.Errorf("%w: volume %q", ErrNotFound, name)
		}

		return json.Unmarshal(data, &rec)
	})
	if err != nil {
		return nil, fmt.Errorf("store: get volume: %w", err)
	}

	return &rec, nil
}

// DeleteVolume removes a volume by name.
func (s *Store) DeleteVolume(name string) error {
	if err := s.db.Update(func(tx *bolt.Tx) error {
		if delErr := tx.Bucket([]byte(bucketVolumes)).Delete([]byte(name)); delErr != nil {
			return fmt.Errorf("delete volume: %w", delErr)
		}

		if delErr := tx.Bucket([]byte(bucketVolumeNames)).Delete([]byte(name)); delErr != nil {
			return fmt.Errorf("delete volume name: %w", delErr)
		}

		return nil
	}); err != nil {
		return fmt.Errorf("store: delete volume: %w", err)
	}

	return nil
}

// ListVolumes returns all stored volume records.
func (s *Store) ListVolumes() ([]*volume.Record, error) {
	var result []*volume.Record

	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(bucketVolumes))

		return b.ForEach(func(_, v []byte) error {
			var rec volume.Record
			if unmarshalErr := json.Unmarshal(v, &rec); unmarshalErr != nil {
				return fmt.Errorf("unmarshal volume: %w", unmarshalErr)
			}

			result = append(result, &rec)

			return nil
		})
	})
	if err != nil {
		return nil, fmt.Errorf("store: list volumes: %w", err)
	}

	return result, nil
}

// AllVolumeRefs returns the set of volume names referenced by any container.
// It satisfies the volume.ContainerLister interface.
func (s *Store) AllVolumeRefs() (map[string]struct{}, error) {
	refs := make(map[string]struct{})

	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(bucketContainers))

		return b.ForEach(func(_, v []byte) error {
			var c container.InspectResponse
			if unmarshalErr := json.Unmarshal(v, &c); unmarshalErr != nil {
				return nil //nolint:nilerr // Best-effort: skip corrupt records.
			}

			// Collect from HostConfig.Binds (named volumes are non-absolute sources).
			if c.HostConfig != nil {
				for _, bind := range c.HostConfig.Binds {
					src, _, _ := strings.Cut(bind, ":")
					if src != "" && !strings.HasPrefix(src, "/") && !strings.HasPrefix(src, ".") {
						refs[src] = struct{}{}
					}
				}
			}

			// Collect from Mounts (inspect response includes resolved mounts).
			for _, mp := range c.Mounts {
				if mp.Name != "" {
					refs[mp.Name] = struct{}{}
				}
			}

			return nil
		})
	})
	if err != nil {
		return nil, fmt.Errorf("store: all volume refs: %w", err)
	}

	return refs, nil
}
