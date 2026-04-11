package network

import (
	"errors"
	"fmt"
	"math/bits"
	"net/netip"
	"sync"
)

// IPAMStore is the persistence interface required by the IPAM allocator.
type IPAMStore interface {
	GetIPAMBitmap(subnet string) ([]byte, error)
	PutIPAMBitmap(subnet string, bitmap []byte) error
	DeleteIPAMBitmap(subnet string) error
}

// ErrNoAvailableIP is returned when all IPs in a subnet are allocated.
var ErrNoAvailableIP = errors.New("ipam: no available IP addresses in subnet")

// ErrIPAlreadyAllocated is returned when a specific IP is already in use.
var ErrIPAlreadyAllocated = errors.New("ipam: IP address already allocated")

// ErrIPOutOfRange is returned when a requested IP is outside the subnet range.
var ErrIPOutOfRange = errors.New("ipam: IP address out of subnet range")

// IPAM manages per-subnet IP allocation using a bitmap persisted in bbolt.
// Bit N in the bitmap represents IP at offset N in the subnet.
// Offset 0 (network address) and offset 1 (gateway) are always reserved.
type IPAM struct {
	store IPAMStore
	mu    sync.Mutex
}

// NewIPAM creates a new IPAM allocator backed by the given store.
func NewIPAM(store IPAMStore) *IPAM {
	return &IPAM{store: store}
}

// Allocate assigns the next available IP from the subnet.
func (a *IPAM) Allocate(subnet netip.Prefix) (netip.Addr, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	totalIPs := subnetSize(subnet)

	bitmap, err := a.loadOrCreateBitmap(subnet, totalIPs)
	if err != nil {
		return netip.Addr{}, err
	}

	// Find first unset bit starting at offset 2 (skip .0 network and .1 gateway).
	offset := findFirstUnset(bitmap, 2, totalIPs)
	if offset < 0 {
		return netip.Addr{}, fmt.Errorf("%w: %s", ErrNoAvailableIP, subnet)
	}

	setBit(bitmap, offset)

	if putErr := a.store.PutIPAMBitmap(subnet.String(), bitmap); putErr != nil {
		return netip.Addr{}, fmt.Errorf("ipam: persist bitmap: %w", putErr)
	}

	ip := offsetToIP(subnet, offset)

	return ip, nil
}

// AllocateSpecific marks a specific IP as allocated in the subnet.
func (a *IPAM) AllocateSpecific(subnet netip.Prefix, ip netip.Addr) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	offset, err := ipToOffset(subnet, ip)
	if err != nil {
		return err
	}

	totalIPs := subnetSize(subnet)

	bitmap, loadErr := a.loadOrCreateBitmap(subnet, totalIPs)
	if loadErr != nil {
		return loadErr
	}

	if getBit(bitmap, offset) {
		return fmt.Errorf("%w: %s in %s", ErrIPAlreadyAllocated, ip, subnet)
	}

	setBit(bitmap, offset)

	if putErr := a.store.PutIPAMBitmap(subnet.String(), bitmap); putErr != nil {
		return fmt.Errorf("ipam: persist bitmap: %w", putErr)
	}

	return nil
}

// Release frees a previously allocated IP back to the pool.
func (a *IPAM) Release(subnet netip.Prefix, ip netip.Addr) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	offset, err := ipToOffset(subnet, ip)
	if err != nil {
		return err
	}

	totalIPs := subnetSize(subnet)

	bitmap, loadErr := a.loadOrCreateBitmap(subnet, totalIPs)
	if loadErr != nil {
		return loadErr
	}

	clearBit(bitmap, offset)

	if putErr := a.store.PutIPAMBitmap(subnet.String(), bitmap); putErr != nil {
		return fmt.Errorf("ipam: persist bitmap: %w", putErr)
	}

	return nil
}

// Count returns the number of allocated and total usable IPs in a subnet.
// Usable excludes the network (.0) and gateway (.1) addresses.
func (a *IPAM) Count(subnet netip.Prefix) (allocated, total int) {
	a.mu.Lock()
	defer a.mu.Unlock()

	totalIPs := subnetSize(subnet)

	bitmap, err := a.loadOrCreateBitmap(subnet, totalIPs)
	if err != nil {
		return 0, totalIPs - 2
	}

	// Count set bits starting from offset 2.
	for i := 2; i < totalIPs; i++ {
		if getBit(bitmap, i) {
			allocated++
		}
	}

	return allocated, totalIPs - 2
}

// ReleaseSubnet deletes the entire bitmap for a subnet.
func (a *IPAM) ReleaseSubnet(subnet netip.Prefix) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	if err := a.store.DeleteIPAMBitmap(subnet.String()); err != nil {
		return fmt.Errorf("ipam: release subnet %s: %w", subnet, err)
	}

	return nil
}

// loadOrCreateBitmap loads the bitmap from store or creates a new one.
// The new bitmap has bits 0 (network) and 1 (gateway) pre-set as reserved.
func (a *IPAM) loadOrCreateBitmap(subnet netip.Prefix, totalIPs int) ([]byte, error) {
	key := subnet.String()

	bitmap, err := a.store.GetIPAMBitmap(key)
	if err != nil {
		return nil, fmt.Errorf("ipam: load bitmap: %w", err)
	}

	bitmapBytes := (totalIPs + 7) / 8

	if bitmap == nil {
		bitmap = make([]byte, bitmapBytes)
		// Reserve offset 0 (network address) and 1 (gateway).
		setBit(bitmap, 0)
		setBit(bitmap, 1)
	}

	// Grow if needed (shouldn't happen in practice).
	if len(bitmap) < bitmapBytes {
		grown := make([]byte, bitmapBytes)
		copy(grown, bitmap)
		bitmap = grown
	}

	return bitmap, nil
}

// subnetSize returns the total number of IPs in a subnet.
func subnetSize(subnet netip.Prefix) int {
	hostBits := 32 - subnet.Bits()
	if hostBits > 20 {
		// Cap at /12 to avoid huge bitmaps (~500KB).
		hostBits = 20
	}

	return 1 << hostBits
}

// offsetToIP converts a bit offset to an IP address within the subnet.
func offsetToIP(subnet netip.Prefix, offset int) netip.Addr {
	base := subnet.Addr().As4()
	ip := uint32(base[0])<<24 | uint32(base[1])<<16 | uint32(base[2])<<8 | uint32(base[3])
	ip += uint32(offset) //nolint:gosec // Offset is bounded by subnet size (max 2^20).

	//nolint:gosec // Deliberate truncation to extract individual bytes from uint32 IP.
	return netip.AddrFrom4([4]byte{
		byte(ip >> 24), byte(ip >> 16), byte(ip >> 8), byte(ip),
	})
}

// ipToOffset converts an IP to its bit offset within the subnet.
func ipToOffset(subnet netip.Prefix, ip netip.Addr) (int, error) {
	if !subnet.Contains(ip) {
		return 0, fmt.Errorf("%w: %s not in %s", ErrIPOutOfRange, ip, subnet)
	}

	base := subnet.Addr().As4()
	addr := ip.As4()

	baseInt := uint32(base[0])<<24 | uint32(base[1])<<16 | uint32(base[2])<<8 | uint32(base[3])
	addrInt := uint32(addr[0])<<24 | uint32(addr[1])<<16 | uint32(addr[2])<<8 | uint32(addr[3])

	return int(addrInt - baseInt), nil
}

// findFirstUnset finds the first unset bit starting from the given offset.
// Returns -1 if no unset bit is found within maxBits.
func findFirstUnset(bitmap []byte, start, maxBits int) int {
	for i := start; i < maxBits; {
		byteIdx := i / 8
		if byteIdx >= len(bitmap) {
			return -1
		}

		b := bitmap[byteIdx]
		if b == 0xFF {
			// Entire byte is full, skip to next byte boundary.
			i = (byteIdx + 1) * 8

			continue
		}

		// Find first zero bit in this byte, starting from bit position within byte.
		bitStart := i % 8
		// Invert byte to find zero positions, mask out bits before bitStart.
		inverted := ^b >> bitStart
		if inverted == 0 {
			i = (byteIdx + 1) * 8

			continue
		}

		bitPos := bitStart + bits.TrailingZeros8(inverted)
		offset := byteIdx*8 + bitPos

		if offset >= maxBits {
			return -1
		}

		return offset
	}

	return -1
}

// setBit sets bit at the given offset in the bitmap.
func setBit(bitmap []byte, offset int) {
	bitmap[offset/8] |= 1 << (offset % 8)
}

// clearBit clears bit at the given offset in the bitmap.
func clearBit(bitmap []byte, offset int) {
	bitmap[offset/8] &^= 1 << (offset % 8)
}

// getBit returns whether the bit at the given offset is set.
func getBit(bitmap []byte, offset int) bool {
	return bitmap[offset/8]&(1<<(offset%8)) != 0
}
