package store

import (
	"net/netip"
	"path/filepath"
	"testing"
	"time"

	"github.com/Shikachuu/kogia/internal/network"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()

	dbPath := filepath.Join(t.TempDir(), "test.db")

	s, err := New(dbPath)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}

	t.Cleanup(func() { _ = s.Close() })

	return s
}

func TestStore_NetworkCRUD(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)

	rec := &network.Record{
		ID:         "aaaa1111bbbb2222cccc3333dddd4444eeee5555ffff6666aaaa1111bbbb2222",
		Name:       "testnet",
		Created:    time.Now().UTC(),
		Driver:     "bridge",
		Subnet:     netip.MustParsePrefix("172.18.0.0/16"),
		Gateway:    netip.MustParseAddr("172.18.0.1"),
		BridgeName: "br-aaaa1111bbbb",
	}

	// Create.
	if err := s.CreateNetwork(rec); err != nil {
		t.Fatalf("create: %v", err)
	}

	// Get by ID.
	got, err := s.GetNetwork(rec.ID)
	if err != nil {
		t.Fatalf("get by id: %v", err)
	}

	if got.Name != rec.Name {
		t.Errorf("name = %q, want %q", got.Name, rec.Name)
	}

	if got.Subnet != rec.Subnet {
		t.Errorf("subnet = %s, want %s", got.Subnet, rec.Subnet)
	}

	// Get by name.
	got, err = s.GetNetwork("testnet")
	if err != nil {
		t.Fatalf("get by name: %v", err)
	}

	if got.ID != rec.ID {
		t.Errorf("id = %q, want %q", got.ID, rec.ID)
	}

	// Get by prefix.
	got, err = s.GetNetwork("aaaa1111")
	if err != nil {
		t.Fatalf("get by prefix: %v", err)
	}

	if got.ID != rec.ID {
		t.Errorf("prefix lookup id = %q, want %q", got.ID, rec.ID)
	}

	// Update.
	rec.Labels = map[string]string{"env": "test"}

	if updateErr := s.UpdateNetwork(rec); updateErr != nil {
		t.Fatalf("update: %v", updateErr)
	}

	got, _ = s.GetNetwork(rec.ID)
	if got.Labels["env"] != "test" {
		t.Errorf("label after update = %q, want %q", got.Labels["env"], "test")
	}

	// Delete.
	if delErr := s.DeleteNetwork(rec.ID, rec.Name); delErr != nil {
		t.Fatalf("delete: %v", delErr)
	}

	_, err = s.GetNetwork(rec.ID)
	if err == nil {
		t.Error("expected ErrNotFound after delete")
	}
}

func TestStore_NetworkNameUniqueness(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)

	rec1 := &network.Record{
		ID:   "1111111111111111111111111111111111111111111111111111111111111111",
		Name: "samename",
	}
	rec2 := &network.Record{
		ID:   "2222222222222222222222222222222222222222222222222222222222222222",
		Name: "samename",
	}

	if err := s.CreateNetwork(rec1); err != nil {
		t.Fatalf("create first: %v", err)
	}

	err := s.CreateNetwork(rec2)
	if err == nil {
		t.Fatal("expected ErrNetworkNameInUse, got nil")
	}
}

func TestStore_EndpointCRUD(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)

	networkID := "aaaa1111bbbb2222cccc3333dddd4444eeee5555ffff6666aaaa1111bbbb2222"
	containerID := "cccc1111dddd2222eeee3333ffff4444aaaa5555bbbb6666cccc1111dddd2222"

	ep := &network.EndpointRecord{
		NetworkID:     networkID,
		ContainerID:   containerID,
		ContainerName: "/mycontainer",
		IPAddress:     netip.MustParseAddr("172.18.0.2"),
		MacAddress:    "02:42:ac:12:00:02",
		VethHost:      "veth1234abcd",
	}

	// Create.
	if err := s.CreateEndpoint(ep); err != nil {
		t.Fatalf("create endpoint: %v", err)
	}

	// Get.
	got, err := s.GetEndpoint(networkID, containerID)
	if err != nil {
		t.Fatalf("get endpoint: %v", err)
	}

	if got.IPAddress != ep.IPAddress {
		t.Errorf("ip = %s, want %s", got.IPAddress, ep.IPAddress)
	}

	if got.ContainerName != ep.ContainerName {
		t.Errorf("name = %q, want %q", got.ContainerName, ep.ContainerName)
	}

	// List by network.
	eps, err := s.ListEndpoints(networkID)
	if err != nil {
		t.Fatalf("list endpoints: %v", err)
	}

	if len(eps) != 1 {
		t.Fatalf("list count = %d, want 1", len(eps))
	}

	// List by container.
	cEps, err := s.ListContainerEndpoints(containerID)
	if err != nil {
		t.Fatalf("list container endpoints: %v", err)
	}

	if len(cEps) != 1 {
		t.Fatalf("container endpoint count = %d, want 1", len(cEps))
	}

	// Delete.
	if delErr := s.DeleteEndpoint(networkID, containerID); delErr != nil {
		t.Fatalf("delete endpoint: %v", delErr)
	}

	_, err = s.GetEndpoint(networkID, containerID)
	if err == nil {
		t.Error("expected error after delete")
	}
}

func TestStore_IPAMBitmap(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)

	subnet := "172.17.0.0/16"

	// Get non-existent returns nil, nil.
	data, err := s.GetIPAMBitmap(subnet)
	if err != nil {
		t.Fatalf("get non-existent: %v", err)
	}

	if data != nil {
		t.Errorf("expected nil for non-existent subnet, got %d bytes", len(data))
	}

	// Put then get.
	bitmap := []byte{0x03, 0x00, 0x00, 0x00}

	if putErr := s.PutIPAMBitmap(subnet, bitmap); putErr != nil {
		t.Fatalf("put: %v", putErr)
	}

	got, err := s.GetIPAMBitmap(subnet)
	if err != nil {
		t.Fatalf("get: %v", err)
	}

	if len(got) != len(bitmap) {
		t.Fatalf("bitmap length = %d, want %d", len(got), len(bitmap))
	}

	if got[0] != 0x03 {
		t.Errorf("bitmap[0] = %02x, want 03", got[0])
	}

	// Delete then get.
	if delErr := s.DeleteIPAMBitmap(subnet); delErr != nil {
		t.Fatalf("delete: %v", delErr)
	}

	got, err = s.GetIPAMBitmap(subnet)
	if err != nil {
		t.Fatalf("get after delete: %v", err)
	}

	if got != nil {
		t.Errorf("expected nil after delete, got %d bytes", len(got))
	}
}

func TestStore_ListNetworks(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)

	// Empty list.
	nets, err := s.ListNetworks()
	if err != nil {
		t.Fatalf("list empty: %v", err)
	}

	if len(nets) != 0 {
		t.Errorf("expected 0 networks, got %d", len(nets))
	}

	// Add two networks.
	for i, name := range []string{"net1", "net2"} {
		rec := &network.Record{
			ID:   "aaa" + string(rune('0'+i)) + "111111111111111111111111111111111111111111111111111111111111",
			Name: name,
		}

		if createErr := s.CreateNetwork(rec); createErr != nil {
			t.Fatalf("create %s: %v", name, createErr)
		}
	}

	nets, err = s.ListNetworks()
	if err != nil {
		t.Fatalf("list: %v", err)
	}

	if len(nets) != 2 {
		t.Errorf("expected 2 networks, got %d", len(nets))
	}
}
