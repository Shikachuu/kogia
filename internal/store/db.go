// Package store manages persistent daemon state via bbolt.
package store

import (
	"errors"
	"fmt"
	"time"

	bolt "go.etcd.io/bbolt"
)

// ErrNotFound is returned when a key does not exist in the store.
var ErrNotFound = errors.New("store: key not found")

// ErrBucketNotFound is returned when a bucket does not exist.
var ErrBucketNotFound = errors.New("store: bucket not found")

const bucketMeta = "meta"

// Store wraps a bbolt database.
type Store struct {
	db *bolt.DB
}

// New opens the bbolt database at dbPath and ensures required buckets exist.
func New(dbPath string) (*Store, error) {
	db, err := bolt.Open(dbPath, 0o600, &bolt.Options{Timeout: time.Second})
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", dbPath, err)
	}

	if err = db.Update(func(tx *bolt.Tx) error {
		if _, berr := tx.CreateBucketIfNotExists([]byte(bucketMeta)); berr != nil {
			return fmt.Errorf("create bucket %q: %w", bucketMeta, berr)
		}

		return nil
	}); err != nil {
		_ = db.Close()

		return nil, fmt.Errorf("store: init buckets: %w", err)
	}

	return &Store{db: db}, nil
}

// Close closes the underlying bbolt database.
func (s *Store) Close() error {
	if err := s.db.Close(); err != nil {
		return fmt.Errorf("store: close: %w", err)
	}

	return nil
}

// Put stores value under key in the named bucket.
func (s *Store) Put(bucket, key string, value []byte) error {
	err := s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(bucket))
		if b == nil {
			return fmt.Errorf("%w: %s", ErrBucketNotFound, bucket)
		}

		return b.Put([]byte(key), value)
	})
	if err != nil {
		return fmt.Errorf("store: put %s/%s: %w", bucket, key, err)
	}

	return nil
}

// Get retrieves the value for key from the named bucket.
// Returns ErrNotFound if the key does not exist.
func (s *Store) Get(bucket, key string) ([]byte, error) {
	var val []byte

	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(bucket))
		if b == nil {
			return fmt.Errorf("%w: %s", ErrBucketNotFound, bucket)
		}

		v := b.Get([]byte(key))
		if v == nil {
			return ErrNotFound
		}

		val = make([]byte, len(v))
		copy(val, v)

		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("store: get %s/%s: %w", bucket, key, err)
	}

	return val, nil
}

// Delete removes key from the named bucket.
func (s *Store) Delete(bucket, key string) error {
	err := s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(bucket))
		if b == nil {
			return fmt.Errorf("%w: %s", ErrBucketNotFound, bucket)
		}

		return b.Delete([]byte(key))
	})
	if err != nil {
		return fmt.Errorf("store: delete %s/%s: %w", bucket, key, err)
	}

	return nil
}
