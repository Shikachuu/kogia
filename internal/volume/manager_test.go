package volume

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// mockStore is an in-memory implementation of the Store interface.
type mockStore struct {
	volumes map[string]*Record
	mu      sync.Mutex
}

func newMockStore() *mockStore {
	return &mockStore{volumes: make(map[string]*Record)}
}

func (m *mockStore) CreateVolume(rec *Record) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, ok := m.volumes[rec.Name]; ok {
		return errors.New("volume name in use")
	}

	m.volumes[rec.Name] = rec

	return nil
}

func (m *mockStore) GetVolume(name string) (*Record, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	rec, ok := m.volumes[name]
	if !ok {
		return nil, errors.New("not found")
	}

	return rec, nil
}

func (m *mockStore) DeleteVolume(name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	delete(m.volumes, name)

	return nil
}

func (m *mockStore) ListVolumes() ([]*Record, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	result := make([]*Record, 0, len(m.volumes))
	for _, rec := range m.volumes {
		result = append(result, rec)
	}

	return result, nil
}

func TestManager_Create(t *testing.T) {
	t.Parallel()

	rootDir := t.TempDir()
	mgr := NewManager(newMockStore(), rootDir)

	rec, err := mgr.Create("myvol", "local", nil, nil)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	if rec.Name != "myvol" {
		t.Errorf("name = %q, want %q", rec.Name, "myvol")
	}

	if rec.Anonymous {
		t.Error("expected non-anonymous volume")
	}

	// Verify directory exists.
	expected := filepath.Join(rootDir, "myvol", "_data")
	if _, statErr := os.Stat(expected); statErr != nil {
		t.Errorf("mountpoint dir not created: %v", statErr)
	}
}

func TestManager_CreateAnonymous(t *testing.T) {
	t.Parallel()

	rootDir := t.TempDir()
	mgr := NewManager(newMockStore(), rootDir)

	rec, err := mgr.Create("", "local", nil, nil)
	if err != nil {
		t.Fatalf("create anonymous: %v", err)
	}

	if len(rec.Name) != 64 {
		t.Errorf("anonymous name length = %d, want 64", len(rec.Name))
	}

	if !rec.Anonymous {
		t.Error("expected anonymous = true")
	}
}

func TestManager_CreateUnsupportedDriver(t *testing.T) {
	t.Parallel()

	rootDir := t.TempDir()
	mgr := NewManager(newMockStore(), rootDir)

	_, err := mgr.Create("vol", "nfs", nil, nil)
	if err == nil {
		t.Fatal("expected error for unsupported driver")
	}
}

func TestManager_Get(t *testing.T) {
	t.Parallel()

	rootDir := t.TempDir()
	mgr := NewManager(newMockStore(), rootDir)

	_, err := mgr.Create("getvol", "local", nil, nil)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	got, err := mgr.Get("getvol")
	if err != nil {
		t.Fatalf("get: %v", err)
	}

	if got.Name != "getvol" {
		t.Errorf("name = %q, want %q", got.Name, "getvol")
	}
}

func TestManager_Remove(t *testing.T) {
	t.Parallel()

	rootDir := t.TempDir()
	mgr := NewManager(newMockStore(), rootDir)

	rec, err := mgr.Create("rmvol", "local", nil, nil)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	if rmErr := mgr.Remove("rmvol", false); rmErr != nil {
		t.Fatalf("remove: %v", rmErr)
	}

	// Verify directory removed.
	if _, statErr := os.Stat(filepath.Dir(rec.Mountpoint)); !os.IsNotExist(statErr) {
		t.Error("expected volume directory to be removed")
	}

	// Verify store entry gone.
	_, err = mgr.Get("rmvol")
	if err == nil {
		t.Error("expected error after removal")
	}
}

func TestManager_EnsureVolume(t *testing.T) {
	t.Parallel()

	rootDir := t.TempDir()
	mgr := NewManager(newMockStore(), rootDir)

	// First call creates.
	rec1, err := mgr.EnsureVolume("ensured")
	if err != nil {
		t.Fatalf("ensure first: %v", err)
	}

	// Second call returns existing.
	rec2, err := mgr.EnsureVolume("ensured")
	if err != nil {
		t.Fatalf("ensure second: %v", err)
	}

	if rec1.Name != rec2.Name {
		t.Errorf("names differ: %q vs %q", rec1.Name, rec2.Name)
	}

	if rec1.CreatedAt != rec2.CreatedAt {
		t.Error("expected same creation time (idempotent)")
	}
}

func TestManager_EnsureVolumeAnonymous(t *testing.T) {
	t.Parallel()

	rootDir := t.TempDir()
	mgr := NewManager(newMockStore(), rootDir)

	rec, err := mgr.EnsureVolume("")
	if err != nil {
		t.Fatalf("ensure anonymous: %v", err)
	}

	if !rec.Anonymous {
		t.Error("expected anonymous volume")
	}

	if len(rec.Name) != 64 {
		t.Errorf("name length = %d, want 64", len(rec.Name))
	}
}

type mockContainerLister struct {
	refs map[string]struct{}
}

func (m *mockContainerLister) AllVolumeRefs() (map[string]struct{}, error) {
	return m.refs, nil
}

func TestManager_Prune(t *testing.T) {
	t.Parallel()

	rootDir := t.TempDir()
	store := newMockStore()
	mgr := NewManager(store, rootDir)

	// Create 3 volumes.
	for _, name := range []string{"keep", "prune1", "prune2"} {
		if _, err := mgr.Create(name, "local", nil, nil); err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
	}

	// "keep" is referenced by a container.
	cl := &mockContainerLister{refs: map[string]struct{}{"keep": {}}}

	deleted, _, err := mgr.Prune(cl)
	if err != nil {
		t.Fatalf("prune: %v", err)
	}

	if len(deleted) != 2 {
		t.Errorf("pruned count = %d, want 2", len(deleted))
	}

	// "keep" should still exist.
	if _, getErr := mgr.Get("keep"); getErr != nil {
		t.Error("expected 'keep' to survive prune")
	}
}
