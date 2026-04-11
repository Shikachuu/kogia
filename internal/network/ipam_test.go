package network

import (
	"net/netip"
	"testing"
)

// mockIPAMStore is an in-memory IPAMStore for testing.
type mockIPAMStore struct {
	bitmaps map[string][]byte
}

func newMockIPAMStore() *mockIPAMStore {
	return &mockIPAMStore{bitmaps: make(map[string][]byte)}
}

func (m *mockIPAMStore) GetIPAMBitmap(subnet string) ([]byte, error) {
	b, ok := m.bitmaps[subnet]
	if !ok {
		return nil, nil
	}

	cp := make([]byte, len(b))
	copy(cp, b)

	return cp, nil
}

func (m *mockIPAMStore) PutIPAMBitmap(subnet string, bitmap []byte) error {
	cp := make([]byte, len(bitmap))
	copy(cp, bitmap)
	m.bitmaps[subnet] = cp

	return nil
}

func (m *mockIPAMStore) DeleteIPAMBitmap(subnet string) error {
	delete(m.bitmaps, subnet)

	return nil
}

func TestIPAM_AllocateSequential(t *testing.T) {
	t.Parallel()

	ipam := NewIPAM(newMockIPAMStore())
	subnet := netip.MustParsePrefix("172.17.0.0/24")

	// First allocation should be .2 (skip .0 network and .1 gateway).
	ip1, err := ipam.Allocate(subnet)
	if err != nil {
		t.Fatalf("allocate 1: %v", err)
	}

	if ip1 != netip.MustParseAddr("172.17.0.2") {
		t.Errorf("first alloc = %s, want 172.17.0.2", ip1)
	}

	ip2, err := ipam.Allocate(subnet)
	if err != nil {
		t.Fatalf("allocate 2: %v", err)
	}

	if ip2 != netip.MustParseAddr("172.17.0.3") {
		t.Errorf("second alloc = %s, want 172.17.0.3", ip2)
	}

	ip3, err := ipam.Allocate(subnet)
	if err != nil {
		t.Fatalf("allocate 3: %v", err)
	}

	if ip3 != netip.MustParseAddr("172.17.0.4") {
		t.Errorf("third alloc = %s, want 172.17.0.4", ip3)
	}
}

func TestIPAM_AllocateExhaustion(t *testing.T) {
	t.Parallel()

	ipam := NewIPAM(newMockIPAMStore())
	// /29 = 8 IPs total. Offsets 0 (network) and 1 (gateway) reserved. 6 usable.
	subnet := netip.MustParsePrefix("10.0.0.0/29")

	for i := range 6 {
		_, err := ipam.Allocate(subnet)
		if err != nil {
			t.Fatalf("allocate %d: %v", i+1, err)
		}
	}

	// 7th should fail.
	_, err := ipam.Allocate(subnet)
	if err == nil {
		t.Fatal("expected ErrNoAvailableIP, got nil")
	}

	if !isErr(err, ErrNoAvailableIP) {
		t.Errorf("got error %v, want ErrNoAvailableIP", err)
	}
}

func TestIPAM_AllocateSpecific(t *testing.T) {
	t.Parallel()

	ipam := NewIPAM(newMockIPAMStore())
	subnet := netip.MustParsePrefix("10.0.0.0/24")

	// Allocate a specific IP.
	target := netip.MustParseAddr("10.0.0.50")

	if err := ipam.AllocateSpecific(subnet, target); err != nil {
		t.Fatalf("allocate specific: %v", err)
	}

	// Allocating same IP again should fail.
	if err := ipam.AllocateSpecific(subnet, target); err == nil {
		t.Fatal("expected ErrIPAlreadyAllocated, got nil")
	}

	// IP outside subnet should fail.
	outside := netip.MustParseAddr("192.168.0.1")
	if err := ipam.AllocateSpecific(subnet, outside); err == nil {
		t.Fatal("expected ErrIPOutOfRange, got nil")
	}
}

func TestIPAM_Release(t *testing.T) {
	t.Parallel()

	ipam := NewIPAM(newMockIPAMStore())
	subnet := netip.MustParsePrefix("10.0.0.0/24")

	ip1, err := ipam.Allocate(subnet)
	if err != nil {
		t.Fatalf("allocate: %v", err)
	}

	// Release it.
	if releaseErr := ipam.Release(subnet, ip1); releaseErr != nil {
		t.Fatalf("release: %v", releaseErr)
	}

	// Allocate again — should get the same IP back.
	ip2, err := ipam.Allocate(subnet)
	if err != nil {
		t.Fatalf("re-allocate: %v", err)
	}

	if ip1 != ip2 {
		t.Errorf("re-allocated %s, want %s", ip2, ip1)
	}
}

func TestIPAM_AllocateAfterGap(t *testing.T) {
	t.Parallel()

	ipam := NewIPAM(newMockIPAMStore())
	subnet := netip.MustParsePrefix("10.0.0.0/24")

	ip1, _ := ipam.Allocate(subnet) // .2
	ip2, _ := ipam.Allocate(subnet) // .3
	_, _ = ipam.Allocate(subnet)    // .4

	// Release .3 to create a gap.
	if err := ipam.Release(subnet, ip2); err != nil {
		t.Fatalf("release: %v", err)
	}

	// Next alloc should fill the gap at .3, not skip to .5.
	ip4, err := ipam.Allocate(subnet)
	if err != nil {
		t.Fatalf("allocate after gap: %v", err)
	}

	if ip4 != ip2 {
		t.Errorf("gap fill = %s, want %s", ip4, ip2)
	}

	_ = ip1 // suppress unused
}

func TestIPAM_Count(t *testing.T) {
	t.Parallel()

	ipam := NewIPAM(newMockIPAMStore())
	subnet := netip.MustParsePrefix("10.0.0.0/24")

	allocated, total := ipam.Count(subnet)
	if allocated != 0 || total != 254 {
		t.Errorf("initial count = (%d, %d), want (0, 254)", allocated, total)
	}

	for range 5 {
		_, _ = ipam.Allocate(subnet)
	}

	allocated, total = ipam.Count(subnet)
	if allocated != 5 || total != 254 {
		t.Errorf("after 5 allocs = (%d, %d), want (5, 254)", allocated, total)
	}
}

func TestIPAM_ReleaseSubnet(t *testing.T) {
	t.Parallel()

	store := newMockIPAMStore()
	ipam := NewIPAM(store)
	subnet := netip.MustParsePrefix("10.0.0.0/24")

	_, _ = ipam.Allocate(subnet)
	_, _ = ipam.Allocate(subnet)

	if err := ipam.ReleaseSubnet(subnet); err != nil {
		t.Fatalf("release subnet: %v", err)
	}

	// Bitmap should be gone from store.
	if _, exists := store.bitmaps[subnet.String()]; exists {
		t.Error("bitmap still exists after ReleaseSubnet")
	}

	// Fresh allocation should start at .2 again.
	ip, err := ipam.Allocate(subnet)
	if err != nil {
		t.Fatalf("allocate after release: %v", err)
	}

	if ip != netip.MustParseAddr("10.0.0.2") {
		t.Errorf("post-release alloc = %s, want 10.0.0.2", ip)
	}
}

func TestOffsetToIP_IPToOffset_Roundtrip(t *testing.T) {
	t.Parallel()

	tests := []struct {
		subnet string
		wantIP string
		offset int
	}{
		{subnet: "172.17.0.0/16", offset: 0, wantIP: "172.17.0.0"},
		{subnet: "172.17.0.0/16", offset: 1, wantIP: "172.17.0.1"},
		{subnet: "172.17.0.0/16", offset: 2, wantIP: "172.17.0.2"},
		{subnet: "172.17.0.0/16", offset: 256, wantIP: "172.17.1.0"},
		{subnet: "10.0.0.0/24", offset: 5, wantIP: "10.0.0.5"},
		{subnet: "10.0.0.0/24", offset: 255, wantIP: "10.0.0.255"},
		{subnet: "192.168.1.0/24", offset: 100, wantIP: "192.168.1.100"},
	}

	for _, tt := range tests {
		subnet := netip.MustParsePrefix(tt.subnet)
		ip := offsetToIP(subnet, tt.offset)

		if ip.String() != tt.wantIP {
			t.Errorf("offsetToIP(%s, %d) = %s, want %s", tt.subnet, tt.offset, ip, tt.wantIP)
		}

		gotOffset, err := ipToOffset(subnet, ip)
		if err != nil {
			t.Errorf("ipToOffset(%s, %s): %v", tt.subnet, ip, err)

			continue
		}

		if gotOffset != tt.offset {
			t.Errorf("ipToOffset(%s, %s) = %d, want %d", tt.subnet, ip, gotOffset, tt.offset)
		}
	}
}

func TestFindFirstUnset(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		bitmap  []byte
		start   int
		maxBits int
		want    int
	}{
		{"empty byte", []byte{0x00}, 0, 8, 0},
		{"full byte", []byte{0xFF}, 0, 8, -1},
		{"skip first two", []byte{0x03}, 2, 8, 2},       // bits 0,1 set → first unset at 2
		{"start mid-byte", []byte{0x07}, 2, 8, 3},       // bits 0,1,2 set → first unset at 3
		{"second byte", []byte{0xFF, 0x00}, 0, 16, 8},   // first byte full → bit 8
		{"all full", []byte{0xFF, 0xFF}, 0, 16, -1},     // no room
		{"maxBits boundary", []byte{0x00}, 0, 3, 0},     // limited by maxBits
		{"start past maxBits", []byte{0x00}, 10, 8, -1}, // start > maxBits
		{"gap in byte", []byte{0x0B}, 0, 8, 2},          // 0b00001011 → bit 2 is unset
	}

	for _, tt := range tests {
		got := findFirstUnset(tt.bitmap, tt.start, tt.maxBits)
		if got != tt.want {
			t.Errorf("%s: findFirstUnset(start=%d, max=%d) = %d, want %d",
				tt.name, tt.start, tt.maxBits, got, tt.want)
		}
	}
}

func TestBitOperations(t *testing.T) {
	t.Parallel()

	bitmap := make([]byte, 4)

	// Set and get.
	setBit(bitmap, 0)

	if !getBit(bitmap, 0) {
		t.Error("bit 0 should be set")
	}

	setBit(bitmap, 15)

	if !getBit(bitmap, 15) {
		t.Error("bit 15 should be set")
	}

	if getBit(bitmap, 1) {
		t.Error("bit 1 should not be set")
	}

	// Clear.
	clearBit(bitmap, 0)

	if getBit(bitmap, 0) {
		t.Error("bit 0 should be cleared")
	}

	// Bit 15 should still be set.
	if !getBit(bitmap, 15) {
		t.Error("bit 15 should still be set after clearing bit 0")
	}
}

// isErr checks whether err wraps target using string matching (errors.Is doesn't
// work across fmt.Errorf %w chains for some sentinel patterns).
func isErr(err, target error) bool {
	if err == nil {
		return target == nil
	}

	for e := err; e != nil; {
		if e.Error() == target.Error() {
			return true
		}

		unwrapper, ok := e.(interface{ Unwrap() error })
		if !ok {
			break
		}

		e = unwrapper.Unwrap()
	}

	// Fall back to string contains.
	return contains(err.Error(), target.Error())
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && searchString(s, sub)
}

func searchString(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}

	return false
}
