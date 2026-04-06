package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/moby/moby/api/types/container"
	bolt "go.etcd.io/bbolt"
)

// ErrNameInUse is returned when a container name is already taken.
var ErrNameInUse = errors.New("store: container name already in use")

// ErrAmbiguousPrefix is returned when a container ID prefix matches multiple containers.
var ErrAmbiguousPrefix = errors.New("store: multiple containers match prefix")

const (
	bucketContainers  = "containers"
	bucketContNames   = "container-names"
	bucketContBundles = "container-bundles"
)

// CreateContainer persists a new container record.
// It stores the InspectResponse in the containers bucket, the name→ID mapping
// in the container-names bucket, and returns an error if the name already exists.
func (s *Store) CreateContainer(c *container.InspectResponse) error {
	data, marshalErr := json.Marshal(c)
	if marshalErr != nil {
		return fmt.Errorf("store: marshal container: %w", marshalErr)
	}

	if err := s.db.Update(func(tx *bolt.Tx) error {
		namesBucket := tx.Bucket([]byte(bucketContNames))
		if v := namesBucket.Get([]byte(c.Name)); v != nil {
			return fmt.Errorf("%w: %s", ErrNameInUse, c.Name)
		}

		if putErr := tx.Bucket([]byte(bucketContainers)).Put([]byte(c.ID), data); putErr != nil {
			return fmt.Errorf("put container: %w", putErr)
		}

		if putErr := namesBucket.Put([]byte(c.Name), []byte(c.ID)); putErr != nil {
			return fmt.Errorf("put container name: %w", putErr)
		}

		return nil
	}); err != nil {
		return fmt.Errorf("store: create container: %w", err)
	}

	return nil
}

// GetContainer retrieves a container by full ID, ID prefix, or name.
func (s *Store) GetContainer(idOrName string) (*container.InspectResponse, error) {
	var data []byte

	err := s.db.View(func(tx *bolt.Tx) error {
		containers := tx.Bucket([]byte(bucketContainers))

		// Try exact ID match first.
		if v := containers.Get([]byte(idOrName)); v != nil {
			data = make([]byte, len(v))
			copy(data, v)

			return nil
		}

		// Try name→ID lookup. Names are stored with "/" prefix (Docker convention)
		// but CLI sends them without the prefix.
		names := tx.Bucket([]byte(bucketContNames))

		nameVariants := []string{idOrName, "/" + idOrName}
		for _, name := range nameVariants {
			if id := names.Get([]byte(name)); id != nil {
				v := containers.Get(id)
				if v != nil {
					data = make([]byte, len(v))
					copy(data, v)

					return nil
				}
			}
		}

		// Try ID prefix match.
		var match []byte

		c := containers.Cursor()
		prefix := []byte(idOrName)

		for k, v := c.Seek(prefix); k != nil && strings.HasPrefix(string(k), idOrName); k, v = c.Next() {
			if match != nil {
				return fmt.Errorf("%w: %s", ErrAmbiguousPrefix, idOrName)
			}

			match = make([]byte, len(v))
			copy(match, v)
		}

		if match == nil {
			return fmt.Errorf("%w: container %q", ErrNotFound, idOrName)
		}

		data = match

		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("store: get container: %w", err)
	}

	var result container.InspectResponse

	if unmarshalErr := json.Unmarshal(data, &result); unmarshalErr != nil {
		return nil, fmt.Errorf("store: unmarshal container: %w", unmarshalErr)
	}

	return &result, nil
}

// UpdateContainer overwrites an existing container record in bbolt.
func (s *Store) UpdateContainer(c *container.InspectResponse) error {
	data, marshalErr := json.Marshal(c)
	if marshalErr != nil {
		return fmt.Errorf("store: marshal container: %w", marshalErr)
	}

	if err := s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(bucketContainers))
		if b.Get([]byte(c.ID)) == nil {
			return fmt.Errorf("%w: container %q", ErrNotFound, c.ID)
		}

		return b.Put([]byte(c.ID), data)
	}); err != nil {
		return fmt.Errorf("store: update container: %w", err)
	}

	return nil
}

// DeleteContainer removes a container from all buckets.
func (s *Store) DeleteContainer(id, name string) error {
	if err := s.db.Update(func(tx *bolt.Tx) error {
		if delErr := tx.Bucket([]byte(bucketContainers)).Delete([]byte(id)); delErr != nil {
			return fmt.Errorf("delete container: %w", delErr)
		}

		if name != "" {
			if delErr := tx.Bucket([]byte(bucketContNames)).Delete([]byte(name)); delErr != nil {
				return fmt.Errorf("delete container name: %w", delErr)
			}
		}

		_ = tx.Bucket([]byte(bucketContBundles)).Delete([]byte(id))

		return nil
	}); err != nil {
		return fmt.Errorf("store: delete container: %w", err)
	}

	return nil
}

// ContainerFilters holds filter criteria for listing containers.
type ContainerFilters struct {
	ID       []string
	Name     []string
	Status   []string
	Label    []string
	Ancestor []string
	Limit    int
	All      bool
}

// ListContainers returns containers matching the given filters.
func (s *Store) ListContainers(f *ContainerFilters) ([]*container.InspectResponse, error) {
	var results []*container.InspectResponse

	err := s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(bucketContainers)).ForEach(func(_, v []byte) error {
			var c container.InspectResponse
			if err := json.Unmarshal(v, &c); err != nil {
				return fmt.Errorf("unmarshal container: %w", err)
			}

			if !f.All && (c.State == nil || c.State.Status != "running") {
				return nil
			}

			if !matchesFilters(&c, f) {
				return nil
			}

			results = append(results, &c)

			return nil
		})
	})
	if err != nil {
		return nil, fmt.Errorf("store: list containers: %w", err)
	}

	if f.Limit > 0 && len(results) > f.Limit {
		results = results[:f.Limit]
	}

	return results, nil
}

// ContainerNameExists checks whether a container name is already taken.
func (s *Store) ContainerNameExists(name string) (bool, error) {
	exists := false

	err := s.db.View(func(tx *bolt.Tx) error {
		if tx.Bucket([]byte(bucketContNames)).Get([]byte(name)) != nil {
			exists = true
		}

		return nil
	})
	if err != nil {
		return false, fmt.Errorf("store: check container name: %w", err)
	}

	return exists, nil
}

// SetContainerBundle stores the OCI bundle directory path for a container.
func (s *Store) SetContainerBundle(id, bundlePath string) error {
	if err := s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(bucketContBundles)).Put([]byte(id), []byte(bundlePath))
	}); err != nil {
		return fmt.Errorf("store: set container bundle: %w", err)
	}

	return nil
}

// GetContainerBundle returns the OCI bundle directory path for a container.
func (s *Store) GetContainerBundle(id string) (string, error) {
	var path string

	err := s.db.View(func(tx *bolt.Tx) error {
		v := tx.Bucket([]byte(bucketContBundles)).Get([]byte(id))
		if v == nil {
			return fmt.Errorf("%w: bundle for container %q", ErrNotFound, id)
		}

		path = string(v)

		return nil
	})
	if err != nil {
		return "", fmt.Errorf("store: get container bundle: %w", err)
	}

	return path, nil
}

func matchesFilters(c *container.InspectResponse, f *ContainerFilters) bool {
	if len(f.ID) > 0 && !matchesAnyPrefix(c.ID, f.ID) {
		return false
	}

	if len(f.Name) > 0 && !matchesAny(c.Name, f.Name) {
		return false
	}

	if len(f.Status) > 0 {
		if c.State == nil || !matchesAny(string(c.State.Status), f.Status) {
			return false
		}
	}

	if len(f.Label) > 0 && !matchesLabels(c.Config, f.Label) {
		return false
	}

	if len(f.Ancestor) > 0 {
		if c.Config == nil || !matchesAnyAncestor(c.Config.Image, c.Image, f.Ancestor) {
			return false
		}
	}

	return true
}

func matchesAnyPrefix(value string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(value, p) {
			return true
		}
	}

	return false
}

func matchesAny(value string, candidates []string) bool {
	for _, c := range candidates {
		if value == c {
			return true
		}
	}

	return false
}

func matchesLabels(cfg *container.Config, labels []string) bool {
	if cfg == nil || cfg.Labels == nil {
		return false
	}

	for _, l := range labels {
		k, v, hasValue := strings.Cut(l, "=")
		val, exists := cfg.Labels[k]

		if !exists {
			return false
		}

		if hasValue && val != v {
			return false
		}
	}

	return true
}

func matchesAnyAncestor(imageName, imageID string, ancestors []string) bool {
	for _, a := range ancestors {
		if imageName == a || imageID == a || strings.HasPrefix(imageID, a) {
			return true
		}
	}

	return false
}
