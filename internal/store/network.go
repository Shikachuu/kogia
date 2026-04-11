package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/Shikachuu/kogia/internal/network"
	bolt "go.etcd.io/bbolt"
)

// ErrNetworkNameInUse is returned when a network name is already taken.
var ErrNetworkNameInUse = errors.New("store: network name already in use")

// ErrAmbiguousNetworkPrefix is returned when a network ID prefix matches multiple networks.
var ErrAmbiguousNetworkPrefix = errors.New("store: multiple networks match prefix")

const (
	bucketNetworks     = "networks"
	bucketNetworkNames = "network-names"
	bucketEndpoints    = "endpoints"
	bucketIPAM         = "ipam"
)

// CreateNetwork persists a new network record.
func (s *Store) CreateNetwork(rec *network.Record) error {
	data, marshalErr := json.Marshal(rec)
	if marshalErr != nil {
		return fmt.Errorf("store: marshal network: %w", marshalErr)
	}

	if err := s.db.Update(func(tx *bolt.Tx) error {
		namesBucket := tx.Bucket([]byte(bucketNetworkNames))
		if v := namesBucket.Get([]byte(rec.Name)); v != nil {
			return fmt.Errorf("%w: %s", ErrNetworkNameInUse, rec.Name)
		}

		if putErr := tx.Bucket([]byte(bucketNetworks)).Put([]byte(rec.ID), data); putErr != nil {
			return fmt.Errorf("put network: %w", putErr)
		}

		if putErr := namesBucket.Put([]byte(rec.Name), []byte(rec.ID)); putErr != nil {
			return fmt.Errorf("put network name: %w", putErr)
		}

		return nil
	}); err != nil {
		return fmt.Errorf("store: create network: %w", err)
	}

	return nil
}

// GetNetwork retrieves a network by full ID, ID prefix, or name.
func (s *Store) GetNetwork(idOrName string) (*network.Record, error) {
	var data []byte

	err := s.db.View(func(tx *bolt.Tx) error {
		networks := tx.Bucket([]byte(bucketNetworks))

		// Try exact ID match first.
		if v := networks.Get([]byte(idOrName)); v != nil {
			data = make([]byte, len(v))
			copy(data, v)

			return nil
		}

		// Try name→ID lookup.
		names := tx.Bucket([]byte(bucketNetworkNames))
		if id := names.Get([]byte(idOrName)); id != nil {
			v := networks.Get(id)
			if v != nil {
				data = make([]byte, len(v))
				copy(data, v)

				return nil
			}
		}

		// Try ID prefix match.
		var match []byte

		c := networks.Cursor()
		prefix := []byte(idOrName)

		for k, v := c.Seek(prefix); k != nil && strings.HasPrefix(string(k), idOrName); k, v = c.Next() {
			if match != nil {
				return fmt.Errorf("%w: %s", ErrAmbiguousNetworkPrefix, idOrName)
			}

			match = make([]byte, len(v))
			copy(match, v)
		}

		if match == nil {
			return fmt.Errorf("%w: network %q", ErrNotFound, idOrName)
		}

		data = match

		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("store: get network: %w", err)
	}

	var result network.Record

	if unmarshalErr := json.Unmarshal(data, &result); unmarshalErr != nil {
		return nil, fmt.Errorf("store: unmarshal network: %w", unmarshalErr)
	}

	return &result, nil
}

// UpdateNetwork overwrites an existing network record in bbolt.
func (s *Store) UpdateNetwork(rec *network.Record) error {
	data, marshalErr := json.Marshal(rec)
	if marshalErr != nil {
		return fmt.Errorf("store: marshal network: %w", marshalErr)
	}

	if err := s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(bucketNetworks))
		if b.Get([]byte(rec.ID)) == nil {
			return fmt.Errorf("%w: network %q", ErrNotFound, rec.ID)
		}

		return b.Put([]byte(rec.ID), data)
	}); err != nil {
		return fmt.Errorf("store: update network: %w", err)
	}

	return nil
}

// DeleteNetwork removes a network from both the networks and network-names buckets.
func (s *Store) DeleteNetwork(id, name string) error {
	if err := s.db.Update(func(tx *bolt.Tx) error {
		if delErr := tx.Bucket([]byte(bucketNetworks)).Delete([]byte(id)); delErr != nil {
			return fmt.Errorf("delete network: %w", delErr)
		}

		if name != "" {
			if delErr := tx.Bucket([]byte(bucketNetworkNames)).Delete([]byte(name)); delErr != nil {
				return fmt.Errorf("delete network name: %w", delErr)
			}
		}

		return nil
	}); err != nil {
		return fmt.Errorf("store: delete network: %w", err)
	}

	return nil
}

// ListNetworks returns all persisted network records.
func (s *Store) ListNetworks() ([]*network.Record, error) {
	var results []*network.Record

	err := s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(bucketNetworks)).ForEach(func(_, v []byte) error {
			var rec network.Record
			if unmarshalErr := json.Unmarshal(v, &rec); unmarshalErr != nil {
				return fmt.Errorf("unmarshal network: %w", unmarshalErr)
			}

			results = append(results, &rec)

			return nil
		})
	})
	if err != nil {
		return nil, fmt.Errorf("store: list networks: %w", err)
	}

	return results, nil
}

// endpointKey builds the composite key for an endpoint: "{networkID}/{containerID}".
func endpointKey(networkID, containerID string) string {
	return networkID + "/" + containerID
}

// CreateEndpoint persists a new endpoint record.
func (s *Store) CreateEndpoint(ep *network.EndpointRecord) error {
	data, marshalErr := json.Marshal(ep)
	if marshalErr != nil {
		return fmt.Errorf("store: marshal endpoint: %w", marshalErr)
	}

	key := endpointKey(ep.NetworkID, ep.ContainerID)

	if err := s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(bucketEndpoints)).Put([]byte(key), data)
	}); err != nil {
		return fmt.Errorf("store: create endpoint: %w", err)
	}

	return nil
}

// GetEndpoint retrieves an endpoint by network and container IDs.
func (s *Store) GetEndpoint(networkID, containerID string) (*network.EndpointRecord, error) {
	var data []byte

	key := endpointKey(networkID, containerID)

	err := s.db.View(func(tx *bolt.Tx) error {
		v := tx.Bucket([]byte(bucketEndpoints)).Get([]byte(key))
		if v == nil {
			return fmt.Errorf("%w: endpoint %q", ErrNotFound, key)
		}

		data = make([]byte, len(v))
		copy(data, v)

		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("store: get endpoint: %w", err)
	}

	var result network.EndpointRecord

	if unmarshalErr := json.Unmarshal(data, &result); unmarshalErr != nil {
		return nil, fmt.Errorf("store: unmarshal endpoint: %w", unmarshalErr)
	}

	return &result, nil
}

// DeleteEndpoint removes an endpoint record.
func (s *Store) DeleteEndpoint(networkID, containerID string) error {
	key := endpointKey(networkID, containerID)

	if err := s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(bucketEndpoints)).Delete([]byte(key))
	}); err != nil {
		return fmt.Errorf("store: delete endpoint: %w", err)
	}

	return nil
}

// ListEndpoints returns all endpoint records for a given network.
func (s *Store) ListEndpoints(networkID string) ([]*network.EndpointRecord, error) {
	var results []*network.EndpointRecord

	prefix := networkID + "/"

	err := s.db.View(func(tx *bolt.Tx) error {
		c := tx.Bucket([]byte(bucketEndpoints)).Cursor()

		for k, v := c.Seek([]byte(prefix)); k != nil && strings.HasPrefix(string(k), prefix); k, v = c.Next() {
			var ep network.EndpointRecord
			if unmarshalErr := json.Unmarshal(v, &ep); unmarshalErr != nil {
				return fmt.Errorf("unmarshal endpoint: %w", unmarshalErr)
			}

			results = append(results, &ep)
		}

		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("store: list endpoints: %w", err)
	}

	return results, nil
}

// ListContainerEndpoints returns all endpoint records for a given container across all networks.
func (s *Store) ListContainerEndpoints(containerID string) ([]*network.EndpointRecord, error) {
	var results []*network.EndpointRecord

	err := s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(bucketEndpoints)).ForEach(func(_, v []byte) error {
			var ep network.EndpointRecord
			if unmarshalErr := json.Unmarshal(v, &ep); unmarshalErr != nil {
				return fmt.Errorf("unmarshal endpoint: %w", unmarshalErr)
			}

			if ep.ContainerID == containerID {
				results = append(results, &ep)
			}

			return nil
		})
	})
	if err != nil {
		return nil, fmt.Errorf("store: list container endpoints: %w", err)
	}

	return results, nil
}

// GetIPAMBitmap retrieves the IPAM bitmap for a subnet.
// Returns nil, nil if no bitmap exists yet (new subnet).
func (s *Store) GetIPAMBitmap(subnet string) ([]byte, error) {
	var data []byte

	err := s.db.View(func(tx *bolt.Tx) error {
		v := tx.Bucket([]byte(bucketIPAM)).Get([]byte(subnet))
		if v == nil {
			return nil // New subnet, no bitmap yet.
		}

		data = make([]byte, len(v))
		copy(data, v)

		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("store: get ipam bitmap: %w", err)
	}

	return data, nil
}

// PutIPAMBitmap stores the IPAM bitmap for a subnet.
func (s *Store) PutIPAMBitmap(subnet string, bitmap []byte) error {
	if err := s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(bucketIPAM)).Put([]byte(subnet), bitmap)
	}); err != nil {
		return fmt.Errorf("store: put ipam bitmap: %w", err)
	}

	return nil
}

// DeleteIPAMBitmap removes the IPAM bitmap for a subnet.
func (s *Store) DeleteIPAMBitmap(subnet string) error {
	if err := s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(bucketIPAM)).Delete([]byte(subnet))
	}); err != nil {
		return fmt.Errorf("store: delete ipam bitmap: %w", err)
	}

	return nil
}
