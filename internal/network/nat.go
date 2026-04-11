package network

import (
	"fmt"
	"net"
	"net/netip"
	"sync"

	"github.com/google/nftables"
	"github.com/google/nftables/expr"
)

const (
	nftTableName = "kogia"
	nftFamily    = nftables.TableFamilyIPv4
)

// portMappingKey uniquely identifies a port mapping rule for removal.
type portMappingKey struct {
	HostIP        netip.Addr
	ContainerIP   netip.Addr
	Protocol      string
	HostPort      uint16
	ContainerPort uint16
}

// NAT manages nftables rules for container networking:
// masquerade (outbound SNAT), DNAT (port mapping), and forwarding.
type NAT struct {
	conn  *nftables.Conn
	table *nftables.Table

	chainPostrouting *nftables.Chain
	chainPrerouting  *nftables.Chain
	chainForward     *nftables.Chain

	// Track rules for targeted removal.
	masqRules    map[string]*nftables.Rule // subnet CIDR -> rule
	portMapRules map[portMappingKey]*nftables.Rule
	mu           sync.Mutex
}

// NewNAT creates a new NAT manager.
func NewNAT() *NAT {
	return &NAT{
		masqRules:    make(map[string]*nftables.Rule),
		portMapRules: make(map[portMappingKey]*nftables.Rule),
	}
}

// Init creates the nftables table and chains if they don't exist.
func (n *NAT) Init() error {
	n.mu.Lock()
	defer n.mu.Unlock()

	conn, err := nftables.New()
	if err != nil {
		return fmt.Errorf("nat: open nftables: %w", err)
	}

	n.conn = conn

	// Create or get the kogia table.
	n.table = n.conn.AddTable(&nftables.Table{
		Family: nftFamily,
		Name:   nftTableName,
	})

	// Create chains.
	n.chainPostrouting = n.conn.AddChain(&nftables.Chain{
		Name:     "postrouting",
		Table:    n.table,
		Type:     nftables.ChainTypeNAT,
		Hooknum:  nftables.ChainHookPostrouting,
		Priority: nftables.ChainPriorityNATSource,
	})

	n.chainPrerouting = n.conn.AddChain(&nftables.Chain{
		Name:     "prerouting",
		Table:    n.table,
		Type:     nftables.ChainTypeNAT,
		Hooknum:  nftables.ChainHookPrerouting,
		Priority: nftables.ChainPriorityNATDest,
	})

	n.chainForward = n.conn.AddChain(&nftables.Chain{
		Name:     "forward",
		Table:    n.table,
		Type:     nftables.ChainTypeFilter,
		Hooknum:  nftables.ChainHookForward,
		Priority: nftables.ChainPriorityFilter,
	})

	if flushErr := n.conn.Flush(); flushErr != nil {
		return fmt.Errorf("nat: flush init: %w", flushErr)
	}

	return nil
}

// AddMasquerade adds a masquerade rule for a subnet so containers can
// reach external networks.
func (n *NAT) AddMasquerade(subnet netip.Prefix) error {
	n.mu.Lock()
	defer n.mu.Unlock()

	key := subnet.String()
	if _, exists := n.masqRules[key]; exists {
		return nil // Already exists.
	}

	subnetIP := subnet.Addr().As4()
	ones := subnet.Bits()
	mask := net.CIDRMask(ones, 32)

	rule := n.conn.AddRule(&nftables.Rule{
		Table: n.table,
		Chain: n.chainPostrouting,
		Exprs: []expr.Any{
			// Load source IP (offset 12 in IPv4 header, 4 bytes).
			&expr.Payload{
				DestRegister: 1,
				Base:         expr.PayloadBaseNetworkHeader,
				Offset:       12,
				Len:          4,
			},
			// Bitwise AND with subnet mask.
			&expr.Bitwise{
				SourceRegister: 1,
				DestRegister:   1,
				Len:            4,
				Mask:           mask,
				Xor:            []byte{0, 0, 0, 0},
			},
			// Compare result with subnet network address.
			&expr.Cmp{
				Op:       expr.CmpOpEq,
				Register: 1,
				Data:     subnetIP[:],
			},
			// Masquerade the packet.
			&expr.Masq{},
		},
	})

	if err := n.conn.Flush(); err != nil {
		return fmt.Errorf("nat: add masquerade for %s: %w", subnet, err)
	}

	n.masqRules[key] = rule

	return nil
}

// RemoveMasquerade removes the masquerade rule for a subnet.
func (n *NAT) RemoveMasquerade(subnet netip.Prefix) error {
	n.mu.Lock()
	defer n.mu.Unlock()

	key := subnet.String()

	rule, exists := n.masqRules[key]
	if !exists {
		return nil
	}

	if err := n.conn.DelRule(rule); err != nil {
		return fmt.Errorf("nat: remove masquerade for %s: %w", subnet, err)
	}

	if err := n.conn.Flush(); err != nil {
		return fmt.Errorf("nat: flush remove masquerade: %w", err)
	}

	delete(n.masqRules, key)

	return nil
}

// AddPortMapping adds a DNAT rule to forward traffic from hostIP:hostPort
// to containerIP:containerPort.
func (n *NAT) AddPortMapping(hostIP netip.Addr, hostPort uint16, containerIP netip.Addr, containerPort uint16, proto string) error {
	n.mu.Lock()
	defer n.mu.Unlock()

	key := portMappingKey{
		HostIP:        hostIP,
		HostPort:      hostPort,
		ContainerIP:   containerIP,
		ContainerPort: containerPort,
		Protocol:      proto,
	}

	if _, exists := n.portMapRules[key]; exists {
		return nil
	}

	protoNum := protoToNum(proto)
	containerIPBytes := containerIP.As4()

	exprs := []expr.Any{
		// Match protocol (offset 9 in IPv4 header, 1 byte).
		&expr.Payload{
			DestRegister: 1,
			Base:         expr.PayloadBaseNetworkHeader,
			Offset:       9,
			Len:          1,
		},
		&expr.Cmp{
			Op:       expr.CmpOpEq,
			Register: 1,
			Data:     []byte{protoNum},
		},
		// Match destination port (offset 2 in transport header, 2 bytes).
		&expr.Payload{
			DestRegister: 1,
			Base:         expr.PayloadBaseTransportHeader,
			Offset:       2,
			Len:          2,
		},
		&expr.Cmp{
			Op:       expr.CmpOpEq,
			Register: 1,
			Data:     portToBytes(hostPort),
		},
	}

	// If hostIP is specified and not 0.0.0.0, also match destination IP.
	if hostIP.IsValid() && !hostIP.IsUnspecified() {
		hostIPBytes := hostIP.As4()

		exprs = append([]expr.Any{
			&expr.Payload{
				DestRegister: 1,
				Base:         expr.PayloadBaseNetworkHeader,
				Offset:       16,
				Len:          4,
			},
			&expr.Cmp{
				Op:       expr.CmpOpEq,
				Register: 1,
				Data:     hostIPBytes[:],
			},
		}, exprs...)
	}

	// DNAT to container IP:port.
	exprs = append(exprs,
		&expr.Immediate{
			Register: 1,
			Data:     append(containerIPBytes[:], portToBytes(containerPort)...),
		},
		&expr.NAT{
			Type:        expr.NATTypeDestNAT,
			Family:      uint32(nftables.TableFamilyIPv4),
			RegAddrMin:  1,
			RegProtoMin: 1,
		},
	)

	rule := n.conn.AddRule(&nftables.Rule{
		Table: n.table,
		Chain: n.chainPrerouting,
		Exprs: exprs,
	})

	if err := n.conn.Flush(); err != nil {
		return fmt.Errorf("nat: add port mapping %s:%d->%s:%d/%s: %w",
			hostIP, hostPort, containerIP, containerPort, proto, err)
	}

	n.portMapRules[key] = rule

	return nil
}

// RemovePortMapping removes a DNAT rule for a port mapping.
func (n *NAT) RemovePortMapping(hostIP netip.Addr, hostPort uint16, containerIP netip.Addr, containerPort uint16, proto string) error {
	n.mu.Lock()
	defer n.mu.Unlock()

	key := portMappingKey{
		HostIP:        hostIP,
		HostPort:      hostPort,
		ContainerIP:   containerIP,
		ContainerPort: containerPort,
		Protocol:      proto,
	}

	rule, exists := n.portMapRules[key]
	if !exists {
		return nil
	}

	if err := n.conn.DelRule(rule); err != nil {
		return fmt.Errorf("nat: remove port mapping: %w", err)
	}

	if err := n.conn.Flush(); err != nil {
		return fmt.Errorf("nat: flush remove port mapping: %w", err)
	}

	delete(n.portMapRules, key)

	return nil
}

// Cleanup flushes the entire kogia nftables table. Called on daemon shutdown.
func (n *NAT) Cleanup() error {
	n.mu.Lock()
	defer n.mu.Unlock()

	if n.conn == nil {
		return nil
	}

	n.conn.DelTable(n.table)

	if err := n.conn.Flush(); err != nil {
		return fmt.Errorf("nat: cleanup table: %w", err)
	}

	n.masqRules = make(map[string]*nftables.Rule)
	n.portMapRules = make(map[portMappingKey]*nftables.Rule)

	return nil
}

// protoToNum converts a protocol string to its IP protocol number.
func protoToNum(proto string) byte {
	switch proto {
	case "udp":
		return 17
	default: // "tcp"
		return 6
	}
}

// portToBytes converts a uint16 port to big-endian bytes (network byte order).
func portToBytes(port uint16) []byte {
	return []byte{byte(port >> 8), byte(port)} //nolint:gosec // Deliberate byte extraction from uint16.
}
