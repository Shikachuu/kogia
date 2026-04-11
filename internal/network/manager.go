package network

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	// DefaultBridgeName is the Linux bridge interface for the default network.
	DefaultBridgeName = "kogia0"
	// DefaultNetworkName matches Docker's default bridge network.
	DefaultNetworkName = "bridge"
	// DriverBridge is the bridge network driver name.
	DriverBridge = "bridge"

	// Use 172.20.0.0/16 to avoid conflicts with Docker's default 172.17.0.0/16.
	defaultSubnet  = "172.20.0.0/16"
	defaultGateway = "172.20.0.1"
)

// ManagerStore is the persistence interface required by the network Manager.
type ManagerStore interface {
	IPAMStore
	CreateNetwork(rec *Record) error
	GetNetwork(idOrName string) (*Record, error)
	UpdateNetwork(rec *Record) error
	DeleteNetwork(id, name string) error
	ListNetworks() ([]*Record, error)
	CreateEndpoint(ep *EndpointRecord) error
	GetEndpoint(networkID, containerID string) (*EndpointRecord, error)
	DeleteEndpoint(networkID, containerID string) error
	ListEndpoints(networkID string) ([]*EndpointRecord, error)
	ListContainerEndpoints(containerID string) ([]*EndpointRecord, error)
}

// ErrNetworkNotFound is returned when a network does not exist.
var ErrNetworkNotFound = errors.New("network not found")

// ErrNetworkHasContainers is returned when trying to remove a network with connected containers.
var ErrNetworkHasContainers = errors.New("network has active endpoints")

// ErrPredefinedNetwork is returned when trying to remove a predefined network (bridge, host, none).
var ErrPredefinedNetwork = errors.New("predefined networks cannot be removed")

// ErrNoAvailableSubnets is returned when all subnets in the pool are exhausted.
var ErrNoAvailableSubnets = errors.New("no available subnets")

// ErrUnexpectedAddrType is returned when a net.Listener returns an unexpected address type.
var ErrUnexpectedAddrType = errors.New("unexpected address type")

// Manager orchestrates container networking: bridge management, IPAM,
// NAT/port mapping, and DNS.
type Manager struct {
	store   ManagerStore
	bridge  *Bridge
	ipam    *IPAM
	nat     *NAT
	dns     *DNS
	rootDir string
	mu      sync.Mutex
}

// NewManager creates a new network Manager.
func NewManager(store ManagerStore, rootDir string) *Manager {
	return &Manager{
		store:   store,
		bridge:  NewBridge(),
		ipam:    NewIPAM(store),
		nat:     NewNAT(),
		dns:     NewDNS(),
		rootDir: rootDir,
	}
}

// Init restores network state from bbolt, recreates bridges, and creates
// the default "bridge" network if it doesn't exist.
func (m *Manager) Init() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Initialize nftables table and chains.
	if err := m.nat.Init(); err != nil {
		return fmt.Errorf("network: init nat: %w", err)
	}

	// Restore existing networks.
	networks, err := m.store.ListNetworks()
	if err != nil {
		return fmt.Errorf("network: list networks: %w", err)
	}

	for _, rec := range networks {
		if rec.Driver != DriverBridge {
			continue
		}

		// Recreate bridge (idempotent).
		if bridgeErr := m.bridge.CreateBridge(rec.BridgeName, rec.Gateway, rec.Subnet); bridgeErr != nil {
			slog.Warn("failed to recreate bridge", "name", rec.BridgeName, "err", bridgeErr)

			continue
		}

		// Restore masquerade rule.
		if masqErr := m.nat.AddMasquerade(rec.Subnet); masqErr != nil {
			slog.Warn("failed to restore masquerade", "subnet", rec.Subnet, "err", masqErr)
		}

		// Restore DNS entries from endpoints (user-defined networks only).
		if rec.Name != DefaultNetworkName {
			m.restoreDNSEntries(rec)

			if listenerErr := m.dns.AddListener(rec.Gateway); listenerErr != nil {
				slog.Warn("failed to start dns listener", "gateway", rec.Gateway, "err", listenerErr)
			}
		}
	}

	// Create predefined networks if they don't exist.
	if predefErr := m.ensurePredefinedNetworks(); predefErr != nil {
		return fmt.Errorf("network: create predefined networks: %w", predefErr)
	}

	slog.Info("network manager initialized")

	return nil
}

// Close shuts down all networking: flush nftables, stop DNS.
func (m *Manager) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if err := m.dns.Close(); err != nil {
		slog.Error("failed to stop dns", "err", err)
	}

	if err := m.nat.Cleanup(); err != nil {
		slog.Error("failed to cleanup nat", "err", err)
	}

	return nil
}

// CreateNetwork creates a new user-defined bridge network.
func (m *Manager) CreateNetwork(name, driver string, subnet netip.Prefix, gateway netip.Addr, internal bool, options, labels map[string]string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if driver == "" {
		driver = DriverBridge
	}

	if driver != DriverBridge {
		return "", fmt.Errorf("network: %w: unsupported driver %q (only \"bridge\" is supported)", errors.ErrUnsupported, driver)
	}

	// Auto-assign subnet if not provided.
	if !subnet.IsValid() {
		var assignErr error

		subnet, gateway, assignErr = m.autoAssignSubnet()
		if assignErr != nil {
			return "", fmt.Errorf("network: %w", assignErr)
		}
	} else if !gateway.IsValid() {
		// Derive gateway from subnet (first usable IP).
		gateway = offsetToIP(subnet, 1)
	}

	id, err := generateNetworkID()
	if err != nil {
		return "", fmt.Errorf("network: %w", err)
	}

	bridgeName := "br-" + id[:12]

	rec := &Record{
		ID:         id,
		Name:       name,
		Created:    time.Now().UTC(),
		Driver:     driver,
		Subnet:     subnet,
		Gateway:    gateway,
		BridgeName: bridgeName,
		Options:    options,
		Labels:     labels,
		Internal:   internal,
	}

	// Create the Linux bridge.
	if bridgeErr := m.bridge.CreateBridge(bridgeName, gateway, subnet); bridgeErr != nil {
		return "", fmt.Errorf("network: create bridge: %w", bridgeErr)
	}

	// Add masquerade rule for outbound NAT.
	if !internal {
		if masqErr := m.nat.AddMasquerade(subnet); masqErr != nil {
			_ = m.bridge.DeleteBridge(bridgeName)

			return "", fmt.Errorf("network: add masquerade: %w", masqErr)
		}
	}

	// Persist to store.
	if storeErr := m.store.CreateNetwork(rec); storeErr != nil {
		_ = m.nat.RemoveMasquerade(subnet)
		_ = m.bridge.DeleteBridge(bridgeName)

		return "", fmt.Errorf("network: persist: %w", storeErr)
	}

	// Start DNS listener on gateway.
	if listenerErr := m.dns.AddListener(gateway); listenerErr != nil {
		slog.Warn("failed to start dns listener for new network", "gateway", gateway, "err", listenerErr)
	}

	slog.Info("network created", "name", name, "id", id[:12], "subnet", subnet, "bridge", bridgeName)

	return id, nil
}

// RemoveNetwork removes a user-defined network.
func (m *Manager) RemoveNetwork(idOrName string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	rec, err := m.store.GetNetwork(idOrName)
	if err != nil {
		return fmt.Errorf("%w: %s", ErrNetworkNotFound, idOrName)
	}

	// Prevent removal of predefined networks.
	if rec.Name == DefaultNetworkName || rec.Driver == "host" || rec.Driver == "none" {
		return fmt.Errorf("%w: %s", ErrPredefinedNetwork, rec.Name)
	}

	// Check for connected containers.
	endpoints, listErr := m.store.ListEndpoints(rec.ID)
	if listErr != nil {
		return fmt.Errorf("network: list endpoints: %w", listErr)
	}

	if len(endpoints) > 0 {
		return fmt.Errorf("%w: %s", ErrNetworkHasContainers, rec.Name)
	}

	// Stop DNS listener.
	if removeErr := m.dns.RemoveListener(rec.Gateway); removeErr != nil {
		slog.Warn("failed to remove dns listener", "gateway", rec.Gateway, "err", removeErr)
	}

	m.dns.DeregisterNetwork(rec.ID)

	// Remove masquerade rule.
	if masqErr := m.nat.RemoveMasquerade(rec.Subnet); masqErr != nil {
		slog.Warn("failed to remove masquerade", "subnet", rec.Subnet, "err", masqErr)
	}

	// Delete the bridge.
	if bridgeErr := m.bridge.DeleteBridge(rec.BridgeName); bridgeErr != nil {
		slog.Warn("failed to delete bridge", "name", rec.BridgeName, "err", bridgeErr)
	}

	// Release IPAM bitmap.
	if ipamErr := m.ipam.ReleaseSubnet(rec.Subnet); ipamErr != nil {
		slog.Warn("failed to release ipam subnet", "subnet", rec.Subnet, "err", ipamErr)
	}

	// Delete from store.
	if delErr := m.store.DeleteNetwork(rec.ID, rec.Name); delErr != nil {
		return fmt.Errorf("network: delete from store: %w", delErr)
	}

	slog.Info("network removed", "name", rec.Name, "id", rec.ID[:12])

	return nil
}

// Connect attaches a container to a network. This is called from runtime.Start()
// after crun create (PID is known) but before crun start.
//
// The operation is transactional: on any failure, all completed steps are rolled back.
func (m *Manager) Connect(networkID, containerID, containerName string, pid int, portMappings []PortMapping) (*EndpointRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	rec, err := m.store.GetNetwork(networkID)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", ErrNetworkNotFound, networkID)
	}

	if rec.Driver != DriverBridge {
		// host/none modes don't need connect — return empty endpoint.
		return &EndpointRecord{NetworkID: rec.ID, ContainerID: containerID, ContainerName: containerName}, nil
	}

	// Rollback stack.
	var cleanups []func()

	defer func() {
		if err != nil {
			for i := len(cleanups) - 1; i >= 0; i-- {
				cleanups[i]()
			}
		}
	}()

	// 1. Allocate IP.
	ip, err := m.ipam.Allocate(rec.Subnet)
	if err != nil {
		return nil, fmt.Errorf("network: allocate ip: %w", err)
	}

	cleanups = append(cleanups, func() {
		_ = m.ipam.Release(rec.Subnet, ip)
	})

	// 2. Create veth pair and configure container networking.
	vethHost, mac, err := m.bridge.ConnectContainer(rec.BridgeName, pid, ip, rec.Gateway, rec.Subnet)
	if err != nil {
		return nil, fmt.Errorf("network: connect container: %w", err)
	}

	cleanups = append(cleanups, func() {
		_ = m.bridge.DisconnectContainer(vethHost)
	})

	// 3. Add port mappings (nftables DNAT).
	for i, pm := range portMappings {
		if natErr := m.nat.AddPortMapping(pm.HostIP, pm.HostPort, ip, pm.ContainerPort, pm.Protocol); natErr != nil {
			err = fmt.Errorf("network: add port mapping %d: %w", i, natErr)

			return nil, err
		}

		pmCopy := pm

		cleanups = append(cleanups, func() {
			_ = m.nat.RemovePortMapping(pmCopy.HostIP, pmCopy.HostPort, ip, pmCopy.ContainerPort, pmCopy.Protocol)
		})
	}

	// 4. Register DNS (user-defined networks only).
	// Strip leading "/" from container name for DNS.
	dnsName := strings.TrimPrefix(containerName, "/")

	if rec.Name != DefaultNetworkName {
		m.dns.Register(rec.ID, dnsName, ip)

		cleanups = append(cleanups, func() {
			m.dns.Deregister(rec.ID, dnsName)
		})
	}

	// 5. Persist endpoint.
	ep := &EndpointRecord{
		NetworkID:     rec.ID,
		ContainerID:   containerID,
		ContainerName: containerName,
		IPAddress:     ip,
		MacAddress:    mac,
		VethHost:      vethHost,
		PortMappings:  portMappings,
		DNSNames:      []string{dnsName},
	}

	if storeErr := m.store.CreateEndpoint(ep); storeErr != nil {
		err = fmt.Errorf("network: persist endpoint: %w", storeErr)

		return nil, err
	}

	slog.Info("container connected to network",
		"container", containerID[:12],
		"network", rec.Name,
		"ip", ip,
		"veth", vethHost,
	)

	return ep, nil
}

// Disconnect removes a container from a network.
func (m *Manager) Disconnect(networkID, containerID string, force bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.disconnectLocked(networkID, containerID, force)
}

// DisconnectAll removes a container from all networks. Called during stop/remove.
func (m *Manager) DisconnectAll(containerID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	endpoints, err := m.store.ListContainerEndpoints(containerID)
	if err != nil {
		return fmt.Errorf("network: list container endpoints: %w", err)
	}

	var lastErr error

	for _, ep := range endpoints {
		if disconnErr := m.disconnectLocked(ep.NetworkID, containerID, true); disconnErr != nil {
			slog.Warn("failed to disconnect container from network",
				"container", containerID[:12],
				"network", ep.NetworkID[:12],
				"err", disconnErr,
			)

			lastErr = disconnErr
		}
	}

	return lastErr
}

// disconnectLocked performs the actual disconnect. Caller must hold m.mu.
func (m *Manager) disconnectLocked(networkID, containerID string, force bool) error {
	ep, err := m.store.GetEndpoint(networkID, containerID)
	if err != nil {
		if force {
			return nil // Endpoint already gone, that's fine for force disconnect.
		}

		return fmt.Errorf("network: endpoint not found: %w", err)
	}

	// Remove port mappings.
	for _, pm := range ep.PortMappings {
		if natErr := m.nat.RemovePortMapping(pm.HostIP, pm.HostPort, ep.IPAddress, pm.ContainerPort, pm.Protocol); natErr != nil {
			slog.Warn("failed to remove port mapping", "err", natErr)
		}
	}

	// Deregister DNS.
	for _, name := range ep.DNSNames {
		m.dns.Deregister(networkID, name)
	}

	// Delete host-side veth.
	if disconnErr := m.bridge.DisconnectContainer(ep.VethHost); disconnErr != nil {
		slog.Warn("failed to disconnect veth", "veth", ep.VethHost, "err", disconnErr)
	}

	// Release IP.
	rec, recErr := m.store.GetNetwork(networkID)
	if recErr == nil {
		if releaseErr := m.ipam.Release(rec.Subnet, ep.IPAddress); releaseErr != nil {
			slog.Warn("failed to release ip", "ip", ep.IPAddress, "err", releaseErr)
		}
	}

	// Delete endpoint from store.
	if delErr := m.store.DeleteEndpoint(networkID, containerID); delErr != nil {
		return fmt.Errorf("network: delete endpoint: %w", delErr)
	}

	slog.Info("container disconnected from network",
		"container", containerID[:12],
		"network", networkID[:12],
	)

	return nil
}

// GenerateNetworkFiles writes /etc/hostname, /etc/hosts, and /etc/resolv.conf
// into the container's bundle directory for bind-mounting.
func (m *Manager) GenerateNetworkFiles(bundleDir, containerID, hostname string, endpoints []*EndpointRecord) error {
	// /etc/hostname
	hostnamePath := filepath.Join(bundleDir, "hostname")
	if writeErr := os.WriteFile(hostnamePath, []byte(hostname+"\n"), 0o644); writeErr != nil { //nolint:gosec // Container config file.
		return fmt.Errorf("network: write hostname: %w", writeErr)
	}

	// /etc/hosts
	if writeErr := m.writeHostsFile(filepath.Join(bundleDir, "hosts"), hostname, containerID, endpoints); writeErr != nil {
		return fmt.Errorf("network: write hosts: %w", writeErr)
	}

	// /etc/resolv.conf
	if writeErr := m.writeResolvConf(filepath.Join(bundleDir, "resolv.conf"), endpoints); writeErr != nil {
		return fmt.Errorf("network: write resolv.conf: %w", writeErr)
	}

	return nil
}

// UpdateHostsFile rewrites /etc/hosts for all containers on a network.
// Called when a container joins or leaves the network.
func (m *Manager) UpdateHostsFile(networkID string) {
	endpoints, err := m.store.ListEndpoints(networkID)
	if err != nil {
		slog.Warn("failed to list endpoints for hosts update", "network", networkID[:12], "err", err)

		return
	}

	for _, ep := range endpoints {
		bundleDir := m.containerBundleDir(ep.ContainerID)
		hostsPath := filepath.Join(bundleDir, "hosts")

		// Only rewrite if the file exists (container is running).
		if _, statErr := os.Stat(hostsPath); statErr != nil {
			continue
		}

		if writeErr := m.writeHostsFile(hostsPath, "", ep.ContainerID, endpoints); writeErr != nil {
			slog.Warn("failed to update hosts file",
				"container", ep.ContainerID[:12],
				"err", writeErr,
			)
		}
	}
}

// GetNetwork retrieves a network record by ID, name, or prefix.
func (m *Manager) GetNetwork(idOrName string) (*Record, error) {
	rec, err := m.store.GetNetwork(idOrName)
	if err != nil {
		return nil, fmt.Errorf("network: get %q: %w", idOrName, err)
	}

	return rec, nil
}

// ListNetworks returns all network records.
func (m *Manager) ListNetworks() ([]*Record, error) {
	recs, err := m.store.ListNetworks()
	if err != nil {
		return nil, fmt.Errorf("network: list: %w", err)
	}

	return recs, nil
}

// ListEndpoints returns all endpoints for a network.
func (m *Manager) ListEndpoints(networkID string) ([]*EndpointRecord, error) {
	eps, err := m.store.ListEndpoints(networkID)
	if err != nil {
		return nil, fmt.Errorf("network: list endpoints: %w", err)
	}

	return eps, nil
}

// writeHostsFile generates the /etc/hosts content.
func (m *Manager) writeHostsFile(path, hostname, containerID string, allEndpoints []*EndpointRecord) error {
	var b strings.Builder

	b.WriteString("127.0.0.1\tlocalhost\n")
	b.WriteString("::1\tlocalhost ip6-localhost ip6-loopback\n")

	// Find this container's own endpoint for the self-entry.
	for _, ep := range allEndpoints {
		if ep.ContainerID == containerID {
			name := strings.TrimPrefix(ep.ContainerName, "/")

			if hostname == "" {
				hostname = containerID[:12]
			}

			b.WriteString(ep.IPAddress.String() + "\t" + hostname + " " + name + "\n")

			break
		}
	}

	// Add entries for other containers on the same network.
	for _, ep := range allEndpoints {
		if ep.ContainerID == containerID {
			continue
		}

		name := strings.TrimPrefix(ep.ContainerName, "/")
		b.WriteString(ep.IPAddress.String() + "\t" + name + "\n")
	}

	if writeErr := os.WriteFile(path, []byte(b.String()), 0o644); writeErr != nil { //nolint:gosec // Container config file.
		return fmt.Errorf("write hosts: %w", writeErr)
	}

	return nil
}

// writeResolvConf generates /etc/resolv.conf content.
func (m *Manager) writeResolvConf(path string, endpoints []*EndpointRecord) error {
	var b strings.Builder

	// For user-defined networks, point to our DNS server (gateway IP).
	// For default bridge, copy the host's resolv.conf.
	usedGateway := false

	for _, ep := range endpoints {
		rec, recErr := m.store.GetNetwork(ep.NetworkID)
		if recErr != nil {
			continue
		}

		if rec.Name != DefaultNetworkName && rec.Gateway.IsValid() {
			b.WriteString("nameserver " + rec.Gateway.String() + "\n")

			usedGateway = true

			break
		}
	}

	if !usedGateway {
		// Fall back to host resolv.conf content.
		hostResolv, readErr := os.ReadFile("/etc/resolv.conf")
		if readErr == nil {
			b.Write(hostResolv)
		} else {
			b.WriteString("nameserver 8.8.8.8\n")
		}
	}

	if writeErr := os.WriteFile(path, []byte(b.String()), 0o644); writeErr != nil { //nolint:gosec // Container config file.
		return fmt.Errorf("write resolv.conf: %w", writeErr)
	}

	return nil
}

// containerBundleDir returns the bundle directory path for a container.
func (m *Manager) containerBundleDir(containerID string) string {
	return filepath.Join(m.rootDir, "containers", containerID)
}

// ensurePredefinedNetworks creates the default bridge, host, and none networks
// if they don't exist in the store.
func (m *Manager) ensurePredefinedNetworks() error {
	// Default bridge network.
	if _, err := m.store.GetNetwork(DefaultNetworkName); err != nil {
		subnet := netip.MustParsePrefix(defaultSubnet)
		gateway := netip.MustParseAddr(defaultGateway)

		id, genErr := generateNetworkID()
		if genErr != nil {
			return genErr
		}

		rec := &Record{
			ID:         id,
			Name:       DefaultNetworkName,
			Created:    time.Now().UTC(),
			Driver:     DriverBridge,
			Subnet:     subnet,
			Gateway:    gateway,
			BridgeName: DefaultBridgeName,
		}

		if bridgeErr := m.bridge.CreateBridge(DefaultBridgeName, gateway, subnet); bridgeErr != nil {
			return fmt.Errorf("create default bridge: %w", bridgeErr)
		}

		if masqErr := m.nat.AddMasquerade(subnet); masqErr != nil {
			return fmt.Errorf("add default masquerade: %w", masqErr)
		}

		if storeErr := m.store.CreateNetwork(rec); storeErr != nil {
			return fmt.Errorf("persist default bridge: %w", storeErr)
		}

		slog.Info("created default bridge network", "subnet", subnet, "gateway", gateway)
	}

	// Host network (virtual — no bridge/IPAM).
	if _, err := m.store.GetNetwork("host"); err != nil {
		id, genErr := generateNetworkID()
		if genErr != nil {
			return genErr
		}

		if storeErr := m.store.CreateNetwork(&Record{
			ID:      id,
			Name:    "host",
			Created: time.Now().UTC(),
			Driver:  "host",
		}); storeErr != nil {
			return fmt.Errorf("persist host network: %w", storeErr)
		}
	}

	// None network (virtual — no bridge/IPAM).
	if _, err := m.store.GetNetwork("none"); err != nil {
		id, genErr := generateNetworkID()
		if genErr != nil {
			return genErr
		}

		if storeErr := m.store.CreateNetwork(&Record{
			ID:      id,
			Name:    "none",
			Created: time.Now().UTC(),
			Driver:  "none",
		}); storeErr != nil {
			return fmt.Errorf("persist none network: %w", storeErr)
		}
	}

	return nil
}

// autoAssignSubnet finds the first unused subnet from the Docker-compatible pool:
// 172.18-31.0.0/16, then 192.168.0-255.0/20.
func (m *Manager) autoAssignSubnet() (netip.Prefix, netip.Addr, error) {
	existing, err := m.store.ListNetworks()
	if err != nil {
		return netip.Prefix{}, netip.Addr{}, fmt.Errorf("list networks for subnet assignment: %w", err)
	}

	usedSubnets := make(map[string]bool, len(existing))
	for _, rec := range existing {
		if rec.Subnet.IsValid() {
			usedSubnets[rec.Subnet.String()] = true
		}
	}

	// Try 172.18.0.0/16 through 172.31.0.0/16.
	// Start at 172.21 to avoid Docker's commonly used 172.17-19 range.
	for second := 21; second <= 31; second++ {
		subnet := netip.MustParsePrefix(fmt.Sprintf("172.%d.0.0/16", second))
		if !usedSubnets[subnet.String()] {
			gateway := netip.MustParseAddr(fmt.Sprintf("172.%d.0.1", second))

			return subnet, gateway, nil
		}
	}

	// Try 192.168.0.0/20 through 192.168.240.0/20.
	for third := 0; third <= 240; third += 16 {
		subnet := netip.MustParsePrefix(fmt.Sprintf("192.168.%d.0/20", third))
		if !usedSubnets[subnet.String()] {
			gateway := netip.MustParseAddr(fmt.Sprintf("192.168.%d.1", third))

			return subnet, gateway, nil
		}
	}

	return netip.Prefix{}, netip.Addr{}, ErrNoAvailableSubnets
}

// restoreDNSEntries loads endpoints from the store and registers them with DNS.
func (m *Manager) restoreDNSEntries(rec *Record) {
	endpoints, err := m.store.ListEndpoints(rec.ID)
	if err != nil {
		return
	}

	for _, ep := range endpoints {
		for _, name := range ep.DNSNames {
			m.dns.Register(rec.ID, name, ep.IPAddress)
		}
	}
}

// generateNetworkID generates a 64-character hex network ID.
func generateNetworkID() (string, error) {
	buf := make([]byte, 32)

	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate network id: %w", err)
	}

	return hex.EncodeToString(buf), nil
}

// ResolvePortMappings converts Docker PortBindings into our PortMapping slice.
// It also resolves ephemeral ports (host port 0) by finding available ports.
func ResolvePortMappings(portBindings map[string][]PortBinding) ([]PortMapping, error) {
	var mappings []PortMapping

	for containerPortProto, bindings := range portBindings {
		// Parse "80/tcp" format.
		parts := strings.SplitN(containerPortProto, "/", 2)
		port := parts[0]
		proto := "tcp"

		if len(parts) == 2 {
			proto = parts[1]
		}

		var containerPort uint16

		if _, scanErr := fmt.Sscanf(port, "%d", &containerPort); scanErr != nil {
			return nil, fmt.Errorf("invalid container port %q: %w", port, scanErr)
		}

		for _, binding := range bindings {
			hostPort := binding.HostPort

			// Resolve ephemeral port.
			if hostPort == 0 {
				resolved, resolveErr := FindAvailablePort(proto)
				if resolveErr != nil {
					return nil, fmt.Errorf("find available port: %w", resolveErr)
				}

				hostPort = resolved
			}

			hostIP := netip.Addr{}

			if binding.HostIP != "" {
				var parseErr error

				hostIP, parseErr = netip.ParseAddr(binding.HostIP)
				if parseErr != nil {
					return nil, fmt.Errorf("invalid host ip %q: %w", binding.HostIP, parseErr)
				}
			}

			mappings = append(mappings, PortMapping{
				HostIP:        hostIP,
				HostPort:      hostPort,
				ContainerPort: containerPort,
				Protocol:      proto,
			})
		}
	}

	return mappings, nil
}

// PortBinding represents a host port binding from Docker's API.
type PortBinding struct {
	HostIP   string
	HostPort uint16
}

// FindAvailablePort finds an available port by briefly listening and then closing.
// FindAvailablePort finds an available port by briefly listening and then closing.
func FindAvailablePort(proto string) (uint16, error) {
	ln, err := net.Listen(proto, "0.0.0.0:0") //nolint:gosec // Ephemeral port discovery must bind all interfaces.
	if err != nil {
		// TCP listen failed, try raw for UDP.
		if proto == "udp" {
			addr, udpErr := net.ResolveUDPAddr("udp", "0.0.0.0:0")
			if udpErr != nil {
				return 0, fmt.Errorf("resolve udp: %w", udpErr)
			}

			conn, listenErr := net.ListenUDP("udp", addr)
			if listenErr != nil {
				return 0, fmt.Errorf("listen udp: %w", listenErr)
			}

			defer func() { _ = conn.Close() }()

			udpAddr, ok := conn.LocalAddr().(*net.UDPAddr)
			if !ok {
				return 0, ErrUnexpectedAddrType
			}

			return uint16(udpAddr.Port), nil //nolint:gosec // Ephemeral port fits in uint16.
		}

		return 0, fmt.Errorf("listen %s: %w", proto, err)
	}

	defer func() { _ = ln.Close() }()

	tcpAddr, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		return 0, ErrUnexpectedAddrType
	}

	return uint16(tcpAddr.Port), nil //nolint:gosec // Ephemeral port fits in uint16.
}
