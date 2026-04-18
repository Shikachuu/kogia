// Package volume manages named and anonymous Docker volumes.
package volume

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// ErrUnsupportedDriver is returned for non-local volume drivers.
var ErrUnsupportedDriver = errors.New("volume: only \"local\" driver is supported")

// Record is the persistent volume state stored in bbolt.
type Record struct {
	CreatedAt  time.Time         `json:"createdAt"`
	Labels     map[string]string `json:"labels,omitempty"`
	Options    map[string]string `json:"options,omitempty"`
	Name       string            `json:"name"`
	Driver     string            `json:"driver"`
	Mountpoint string            `json:"mountpoint"`
	Anonymous  bool              `json:"anonymous,omitempty"`
}

// Store is the persistence interface required by the volume Manager.
type Store interface {
	CreateVolume(rec *Record) error
	GetVolume(name string) (*Record, error)
	DeleteVolume(name string) error
	ListVolumes() ([]*Record, error)
}

// Manager orchestrates named and anonymous volumes.
type Manager struct {
	store   Store
	rootDir string
	mu      sync.Mutex
}

// NewManager creates a volume manager rooted at rootDir.
func NewManager(store Store, rootDir string) *Manager {
	return &Manager{
		store:   store,
		rootDir: rootDir,
	}
}

// Create creates a new named or anonymous volume.
// If name is empty, a random 64-char hex name is generated (anonymous volume).
func (m *Manager) Create(name, driver string, labels, opts map[string]string) (*Record, error) {
	if driver == "" {
		driver = "local"
	}

	if driver != "local" {
		return nil, fmt.Errorf("%w: %q", ErrUnsupportedDriver, driver)
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	anonymous := false

	if name == "" {
		n, err := randomHex(32)
		if err != nil {
			return nil, fmt.Errorf("volume: generate name: %w", err)
		}

		name = n
		anonymous = true
	}

	// Check if already exists.
	if existing, _ := m.store.GetVolume(name); existing != nil {
		return existing, nil
	}

	mountpoint := filepath.Join(m.rootDir, name, "_data")

	if err := os.MkdirAll(mountpoint, 0o750); err != nil {
		return nil, fmt.Errorf("volume: mkdir %s: %w", mountpoint, err)
	}

	rec := &Record{
		Name:       name,
		Driver:     driver,
		Labels:     labels,
		Options:    opts,
		Mountpoint: mountpoint,
		CreatedAt:  time.Now().UTC(),
		Anonymous:  anonymous,
	}

	if err := m.store.CreateVolume(rec); err != nil {
		return nil, fmt.Errorf("volume: create: %w", err)
	}

	return rec, nil
}

// Get retrieves a volume by name.
func (m *Manager) Get(name string) (*Record, error) {
	rec, err := m.store.GetVolume(name)
	if err != nil {
		return nil, fmt.Errorf("volume: get %q: %w", name, err)
	}

	return rec, nil
}

// Remove deletes a volume by name.
// If force is false an error is returned when the volume is in use.
func (m *Manager) Remove(name string, _ bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	rec, err := m.store.GetVolume(name)
	if err != nil {
		return fmt.Errorf("volume: remove %q: %w", name, err)
	}

	if err = m.store.DeleteVolume(name); err != nil {
		return fmt.Errorf("volume: remove %q: %w", name, err)
	}

	// Best-effort cleanup of data directory.
	_ = os.RemoveAll(filepath.Dir(rec.Mountpoint))

	return nil
}

// List returns all volumes.
func (m *Manager) List() ([]*Record, error) {
	recs, err := m.store.ListVolumes()
	if err != nil {
		return nil, fmt.Errorf("volume: list: %w", err)
	}

	return recs, nil
}

// ContainerLister retrieves volume references from containers.
type ContainerLister interface {
	// AllVolumeRefs returns the set of volume names referenced by any container.
	AllVolumeRefs() (map[string]struct{}, error)
}

// Prune removes volumes not referenced by any container.
// Returns deleted names and total reclaimed bytes.
func (m *Manager) Prune(cl ContainerLister) ([]string, uint64, error) {
	refs, err := cl.AllVolumeRefs()
	if err != nil {
		return nil, 0, fmt.Errorf("volume: prune: list refs: %w", err)
	}

	all, err := m.store.ListVolumes()
	if err != nil {
		return nil, 0, fmt.Errorf("volume: prune: list: %w", err)
	}

	var (
		deleted  []string
		recBytes uint64
	)

	for _, rec := range all {
		if _, inUse := refs[rec.Name]; inUse {
			continue
		}

		size := dirSize(filepath.Dir(rec.Mountpoint))

		if rmErr := m.Remove(rec.Name, true); rmErr != nil {
			continue
		}

		deleted = append(deleted, rec.Name)
		recBytes += size
	}

	return deleted, recBytes, nil
}

// EnsureVolume returns an existing volume or creates a new one.
// If name is empty an anonymous volume is created.
func (m *Manager) EnsureVolume(name string) (*Record, error) {
	if name != "" {
		if rec, err := m.store.GetVolume(name); err == nil {
			return rec, nil
		}
	}

	return m.Create(name, "local", nil, nil)
}

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("volume: random hex: %w", err)
	}

	return hex.EncodeToString(b), nil
}

func dirSize(path string) uint64 {
	var size uint64

	_ = filepath.Walk(path, func(_ string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		if !info.IsDir() && info.Size() > 0 {
			size += uint64(info.Size()) //nolint:gosec // Size is always non-negative.
		}

		return nil
	})

	return size
}
