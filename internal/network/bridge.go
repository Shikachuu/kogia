package network

import (
	"crypto/rand"
	"fmt"
	"net"
	"net/netip"
	"os"
	"runtime"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
)

// Bridge manages Linux bridge interfaces and veth pairs for container networking.
type Bridge struct{}

// NewBridge creates a new Bridge manager.
func NewBridge() *Bridge {
	return &Bridge{}
}

// CreateBridge creates a Linux bridge interface with the given name, assigns
// the gateway IP address from the subnet, and brings it up.
func (b *Bridge) CreateBridge(name string, gateway netip.Addr, subnet netip.Prefix) error {
	br := &netlink.Bridge{
		LinkAttrs: netlink.LinkAttrs{
			Name: name,
		},
	}

	alreadyExists := false

	if addErr := netlink.LinkAdd(br); addErr != nil {
		if os.IsExist(addErr) {
			alreadyExists = true
		} else {
			return fmt.Errorf("bridge: create %s: %w", name, addErr)
		}
	}

	if !alreadyExists {
		// Assign gateway IP to the bridge.
		addr := &netlink.Addr{
			IPNet: &net.IPNet{
				IP:   gateway.AsSlice(),
				Mask: net.CIDRMask(subnet.Bits(), 32),
			},
		}

		if addrErr := netlink.AddrAdd(br, addr); addrErr != nil {
			_ = netlink.LinkDel(br)

			return fmt.Errorf("bridge: add addr %s to %s: %w", gateway, name, addrErr)
		}

		// Bring the bridge up.
		if upErr := netlink.LinkSetUp(br); upErr != nil {
			_ = netlink.LinkDel(br)

			return fmt.Errorf("bridge: set up %s: %w", name, upErr)
		}
	}

	// Always configure sysctls — even for existing bridges (e.g. daemon restart).
	if fwdErr := enableIPForward(); fwdErr != nil {
		fmt.Fprintf(os.Stderr, "bridge: warning: enable ip_forward: %v\n", fwdErr)
	}

	if rlnErr := enableRouteLocalnet(name); rlnErr != nil {
		fmt.Fprintf(os.Stderr, "bridge: warning: enable route_localnet on %s: %v\n", name, rlnErr)
	}

	return nil
}

// DeleteBridge removes a Linux bridge interface.
func (b *Bridge) DeleteBridge(name string) error {
	link, err := netlink.LinkByName(name)
	if err != nil {
		return fmt.Errorf("bridge: find %s: %w", name, err)
	}

	if delErr := netlink.LinkDel(link); delErr != nil {
		return fmt.Errorf("bridge: delete %s: %w", name, delErr)
	}

	return nil
}

// ConnectContainer creates a veth pair, attaches the host end to the bridge,
// moves the container end into the container's network namespace (identified
// by PID), assigns the container IP and default route.
//
// Returns the host-side veth name and generated MAC address.
func (b *Bridge) ConnectContainer(bridgeName string, pid int, containerIP, gateway netip.Addr, subnet netip.Prefix) (vethHost, mac string, err error) {
	// Look up the bridge.
	br, lookupErr := netlink.LinkByName(bridgeName)
	if lookupErr != nil {
		return "", "", fmt.Errorf("bridge: find %s: %w", bridgeName, lookupErr)
	}

	// Generate veth names.
	vethHost, err = generateVethName()
	if err != nil {
		return "", "", err
	}

	// Use a temporary random name for the peer in the host namespace to avoid
	// conflicts (e.g. host already has eth0). It gets renamed to "eth0" inside
	// the container's netns.
	peerTmpName, err := generateVethName()
	if err != nil {
		return "", "", err
	}

	// Create the veth pair.
	veth := &netlink.Veth{
		LinkAttrs: netlink.LinkAttrs{
			Name:        vethHost,
			MasterIndex: br.Attrs().Index,
		},
		PeerName: peerTmpName,
	}

	if addErr := netlink.LinkAdd(veth); addErr != nil {
		return "", "", fmt.Errorf("bridge: create veth pair: %w", addErr)
	}

	// If anything fails after this point, clean up the veth pair.
	defer func() {
		if err != nil {
			_ = netlink.LinkDel(veth)
		}
	}()

	// Get the container-side veth (peer) by its temporary name.
	peer, err := netlink.LinkByName(peerTmpName)
	if err != nil {
		return "", "", fmt.Errorf("bridge: find peer %s: %w", peerTmpName, err)
	}

	// Generate MAC address: 02:42:xx:xx:xx:xx from container IP.
	mac = generateMAC(containerIP)

	if macErr := netlink.LinkSetHardwareAddr(peer, mustParseMAC(mac)); macErr != nil {
		return "", "", fmt.Errorf("bridge: set mac on %s: %w", peerTmpName, macErr)
	}

	// Move peer into the container's network namespace.
	if nsErr := netlink.LinkSetNsPid(peer, pid); nsErr != nil {
		return "", "", fmt.Errorf("bridge: move %s to pid %d: %w", peerTmpName, pid, nsErr)
	}

	// Configure networking inside the container's namespace.
	// The interface is renamed from its temporary name to "eth0" inside the netns.
	if cfgErr := configureContainerNetns(pid, peerTmpName, containerIP, gateway, subnet); cfgErr != nil {
		return "", "", fmt.Errorf("bridge: configure container netns: %w", cfgErr)
	}

	// Bring up host-side veth.
	hostLink, err := netlink.LinkByName(vethHost)
	if err != nil {
		return "", "", fmt.Errorf("bridge: find host veth %s: %w", vethHost, err)
	}

	if upErr := netlink.LinkSetUp(hostLink); upErr != nil {
		return "", "", fmt.Errorf("bridge: set up host veth %s: %w", vethHost, upErr)
	}

	return vethHost, mac, nil
}

// DisconnectContainer removes the host-side veth, which automatically
// destroys the veth pair.
func (b *Bridge) DisconnectContainer(vethHost string) error {
	link, err := netlink.LinkByName(vethHost)
	if err != nil {
		// Already gone (container exited, cleanup race). Not an error.
		return nil //nolint:nilerr // Missing veth is expected during cleanup races.
	}

	if delErr := netlink.LinkDel(link); delErr != nil {
		return fmt.Errorf("bridge: delete veth %s: %w", vethHost, delErr)
	}

	return nil
}

// configureContainerNetns enters the container's network namespace and
// configures the interface: assigns IP, brings up eth0 + lo, adds default route.
func configureContainerNetns(pid int, ifName string, containerIP, gateway netip.Addr, subnet netip.Prefix) error {
	// Lock OS thread — netns changes are per-thread.
	runtime.LockOSThread()

	defer runtime.UnlockOSThread()

	// Save the current (host) namespace.
	hostNS, err := netns.Get()
	if err != nil {
		return fmt.Errorf("get host netns: %w", err)
	}

	defer func() { _ = hostNS.Close() }()

	// Enter the container's namespace.
	containerNS, err := netns.GetFromPid(pid)
	if err != nil {
		return fmt.Errorf("get container netns for pid %d: %w", pid, err)
	}

	defer func() { _ = containerNS.Close() }()

	if setErr := netns.Set(containerNS); setErr != nil {
		return fmt.Errorf("set container netns: %w", setErr)
	}

	// Always restore host namespace when done.
	defer func() { _ = netns.Set(hostNS) }()

	// Find the interface inside the container namespace (still has its temp name).
	link, err := netlink.LinkByName(ifName)
	if err != nil {
		return fmt.Errorf("find %s in container: %w", ifName, err)
	}

	// Rename to eth0.
	if renameErr := netlink.LinkSetName(link, "eth0"); renameErr != nil {
		return fmt.Errorf("rename %s to eth0: %w", ifName, renameErr)
	}

	// Re-fetch after rename.
	link, err = netlink.LinkByName("eth0")
	if err != nil {
		return fmt.Errorf("find eth0 after rename: %w", err)
	}

	// Assign IP address.
	addr := &netlink.Addr{
		IPNet: &net.IPNet{
			IP:   containerIP.AsSlice(),
			Mask: net.CIDRMask(subnet.Bits(), 32),
		},
	}

	if addrErr := netlink.AddrAdd(link, addr); addrErr != nil {
		return fmt.Errorf("add addr %s to %s: %w", containerIP, ifName, addrErr)
	}

	// Bring up eth0.
	if upErr := netlink.LinkSetUp(link); upErr != nil {
		return fmt.Errorf("set up %s: %w", ifName, upErr)
	}

	// Bring up loopback.
	lo, loErr := netlink.LinkByName("lo")
	if loErr == nil {
		_ = netlink.LinkSetUp(lo)
	}

	// Add default route via the gateway.
	route := &netlink.Route{
		Gw: gateway.AsSlice(),
	}

	if routeErr := netlink.RouteAdd(route); routeErr != nil {
		return fmt.Errorf("add default route via %s: %w", gateway, routeErr)
	}

	return nil
}

// generateVethName creates a random veth host-side name like "veth1a2b3c".
func generateVethName() (string, error) {
	buf := make([]byte, 4)

	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("bridge: generate veth name: %w", err)
	}

	return fmt.Sprintf("veth%x", buf), nil
}

// generateMAC creates a MAC address from a container IP using Docker's convention:
// 02:42:xx:xx:xx:xx where xx bytes come from the IP address.
func generateMAC(ip netip.Addr) string {
	b := ip.As4()

	return fmt.Sprintf("02:42:%02x:%02x:%02x:%02x", b[0], b[1], b[2], b[3])
}

// mustParseMAC parses a MAC string or panics. Only used with generateMAC output.
func mustParseMAC(s string) net.HardwareAddr {
	mac, err := net.ParseMAC(s)
	if err != nil {
		panic("invalid MAC: " + s)
	}

	return mac
}

// enableIPForward enables IPv4 packet forwarding via /proc.
func enableIPForward() error {
	if err := os.WriteFile("/proc/sys/net/ipv4/ip_forward", []byte("1"), 0o644); err != nil { //nolint:gosec // System knob, needs world-readable.
		return fmt.Errorf("write ip_forward: %w", err)
	}

	return nil
}

// enableRouteLocalnet allows routing of 127.0.0.0/8 on the given interface.
// Required for DNAT from localhost to bridge container IPs. Set per-interface
// to avoid the global security implications of net.ipv4.conf.all.route_localnet.
func enableRouteLocalnet(ifName string) error {
	path := "/proc/sys/net/ipv4/conf/" + ifName + "/route_localnet"

	if err := os.WriteFile(path, []byte("1"), 0o644); err != nil { //nolint:gosec // System knob, needs world-readable.
		return fmt.Errorf("write route_localnet for %s: %w", ifName, err)
	}

	return nil
}
