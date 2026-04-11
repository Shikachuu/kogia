package network

import (
	"bufio"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/miekg/dns"
)

const (
	dnsTTL             = 0 // Container records are always fresh.
	dnsUpstreamTimeout = 5 * time.Second
	dnsCacheTimeout    = 30 * time.Second
)

// dnsListener tracks a UDP+TCP DNS server pair bound to a gateway IP.
type dnsListener struct {
	udpServer *dns.Server
	tcpServer *dns.Server
}

// dnsCache stores a cached upstream DNS response.
type dnsCache struct {
	msg     *dns.Msg
	expires time.Time
}

// DNS provides an authoritative DNS server for container name resolution.
// It listens on bridge gateway IPs and resolves container names to IPs.
// Unknown queries are forwarded to the host's upstream nameservers.
type DNS struct {
	records         map[string]map[string]netip.Addr
	listeners       map[string]*dnsListener
	cache           map[string]*dnsCache
	upstreamServers []string
	cacheMu         sync.RWMutex
	mu              sync.RWMutex
}

// NewDNS creates a new DNS server. Upstream servers are parsed from the host's
// /etc/resolv.conf.
func NewDNS() *DNS {
	upstream := parseHostResolv()

	return &DNS{
		records:         make(map[string]map[string]netip.Addr),
		listeners:       make(map[string]*dnsListener),
		upstreamServers: upstream,
		cache:           make(map[string]*dnsCache),
	}
}

// AddListener starts a DNS server (UDP+TCP) on the given gateway IP, port 53.
func (d *DNS) AddListener(gatewayIP netip.Addr) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	key := gatewayIP.String()
	if _, exists := d.listeners[key]; exists {
		return nil // Already listening.
	}

	addr := net.JoinHostPort(key, "53")

	mux := dns.NewServeMux()
	mux.HandleFunc(".", d.handleQuery)

	udpServer := &dns.Server{
		Addr:    addr,
		Net:     "udp",
		Handler: mux,
	}

	tcpServer := &dns.Server{
		Addr:    addr,
		Net:     "tcp",
		Handler: mux,
	}

	// Start UDP server.
	go func() {
		if err := udpServer.ListenAndServe(); err != nil {
			slog.Debug("dns udp server stopped", "addr", addr, "err", err)
		}
	}()

	// Start TCP server.
	go func() {
		if err := tcpServer.ListenAndServe(); err != nil {
			slog.Debug("dns tcp server stopped", "addr", addr, "err", err)
		}
	}()

	d.listeners[key] = &dnsListener{
		udpServer: udpServer,
		tcpServer: tcpServer,
	}

	slog.Debug("dns listener started", "addr", addr)

	return nil
}

// RemoveListener stops the DNS server for a gateway IP.
func (d *DNS) RemoveListener(gatewayIP netip.Addr) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	key := gatewayIP.String()

	ln, exists := d.listeners[key]
	if !exists {
		return nil
	}

	_ = ln.udpServer.Shutdown()
	_ = ln.tcpServer.Shutdown()

	delete(d.listeners, key)

	slog.Debug("dns listener stopped", "addr", key)

	return nil
}

// Register adds an A record mapping containerName -> ip for a network.
func (d *DNS) Register(networkID, containerName string, ip netip.Addr) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.records[networkID] == nil {
		d.records[networkID] = make(map[string]netip.Addr)
	}

	d.records[networkID][containerName] = ip

	slog.Debug("dns registered", "network", networkID[:12], "name", containerName, "ip", ip)
}

// Deregister removes the DNS record for a container on a network.
func (d *DNS) Deregister(networkID, containerName string) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if recs, ok := d.records[networkID]; ok {
		delete(recs, containerName)

		if len(recs) == 0 {
			delete(d.records, networkID)
		}
	}

	slog.Debug("dns deregistered", "network", networkID[:12], "name", containerName)
}

// DeregisterNetwork removes all DNS records for a network.
func (d *DNS) DeregisterNetwork(networkID string) {
	d.mu.Lock()
	defer d.mu.Unlock()

	delete(d.records, networkID)
}

// Close stops all DNS listeners.
func (d *DNS) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()

	for key, ln := range d.listeners {
		_ = ln.udpServer.Shutdown()
		_ = ln.tcpServer.Shutdown()

		delete(d.listeners, key)
	}

	return nil
}

// handleQuery processes incoming DNS queries. It checks local records first,
// then forwards to upstream nameservers.
func (d *DNS) handleQuery(w dns.ResponseWriter, r *dns.Msg) {
	m := new(dns.Msg)
	m.SetReply(r)
	m.Authoritative = true

	for _, q := range r.Question {
		switch q.Qtype {
		case dns.TypeA:
			if ip := d.lookupA(q.Name); ip.IsValid() {
				m.Answer = append(m.Answer, &dns.A{
					Hdr: dns.RR_Header{
						Name:   q.Name,
						Rrtype: dns.TypeA,
						Class:  dns.ClassINET,
						Ttl:    dnsTTL,
					},
					A: ip.AsSlice(),
				})
			}

		case dns.TypePTR:
			if name := d.lookupPTR(q.Name); name != "" {
				m.Answer = append(m.Answer, &dns.PTR{
					Hdr: dns.RR_Header{
						Name:   q.Name,
						Rrtype: dns.TypePTR,
						Class:  dns.ClassINET,
						Ttl:    dnsTTL,
					},
					Ptr: dns.Fqdn(name),
				})
			}
		}
	}

	// If we found answers locally, respond.
	if len(m.Answer) > 0 {
		if writeErr := w.WriteMsg(m); writeErr != nil {
			slog.Debug("dns write response failed", "err", writeErr)
		}

		return
	}

	// Forward to upstream.
	if resp := d.forwardUpstream(r); resp != nil {
		resp.Id = r.Id

		if writeErr := w.WriteMsg(resp); writeErr != nil {
			slog.Debug("dns write forwarded response failed", "err", writeErr)
		}

		return
	}

	// No answer from anyone — return NXDOMAIN.
	m.Rcode = dns.RcodeNameError

	if writeErr := w.WriteMsg(m); writeErr != nil {
		slog.Debug("dns write nxdomain failed", "err", writeErr)
	}
}

// lookupA searches all networks for a container name and returns its IP.
func (d *DNS) lookupA(qname string) netip.Addr {
	// Strip trailing dot from FQDN.
	name := strings.TrimSuffix(qname, ".")

	d.mu.RLock()
	defer d.mu.RUnlock()

	for _, recs := range d.records {
		if ip, ok := recs[name]; ok {
			return ip
		}
	}

	return netip.Addr{}
}

// lookupPTR does a reverse lookup: finds the container name for an IP.
func (d *DNS) lookupPTR(qname string) string {
	// Convert PTR name (e.g. "2.0.17.172.in-addr.arpa.") to IP.
	ip, err := ptrToIP(qname)
	if err != nil {
		return ""
	}

	d.mu.RLock()
	defer d.mu.RUnlock()

	for _, recs := range d.records {
		for name, recIP := range recs {
			if recIP == ip {
				return name
			}
		}
	}

	return ""
}

// forwardUpstream sends the query to upstream nameservers and returns the
// first successful response. Results are cached briefly.
func (d *DNS) forwardUpstream(r *dns.Msg) *dns.Msg {
	if len(d.upstreamServers) == 0 || len(r.Question) == 0 {
		return nil
	}

	// Check cache.
	cacheKey := r.Question[0].String()

	d.cacheMu.RLock()
	cached, ok := d.cache[cacheKey]
	d.cacheMu.RUnlock()

	if ok && time.Now().Before(cached.expires) {
		return cached.msg.Copy()
	}

	// Query upstream servers.
	client := &dns.Client{
		Timeout: dnsUpstreamTimeout,
	}

	for _, server := range d.upstreamServers {
		resp, _, err := client.Exchange(r, server)
		if err != nil {
			continue
		}

		// Cache the response.
		d.cacheMu.Lock()
		d.cache[cacheKey] = &dnsCache{
			msg:     resp.Copy(),
			expires: time.Now().Add(dnsCacheTimeout),
		}
		d.cacheMu.Unlock()

		return resp
	}

	return nil
}

// parseHostResolv reads /etc/resolv.conf and returns nameserver addresses
// in "host:port" format suitable for dns.Client.Exchange.
func parseHostResolv() []string {
	f, err := os.Open("/etc/resolv.conf")
	if err != nil {
		return nil
	}

	defer func() { _ = f.Close() }()

	var servers []string

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(line, "nameserver") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				servers = append(servers, net.JoinHostPort(fields[1], "53"))
			}
		}
	}

	return servers
}

var (
	errNotPTRName = errors.New("not a PTR name")
	errInvalidPTR = errors.New("invalid PTR name")
)

// ptrToIP converts a PTR record name like "2.0.17.172.in-addr.arpa." to an IP.
func ptrToIP(ptr string) (netip.Addr, error) {
	ptr = strings.TrimSuffix(ptr, ".")
	suffix := ".in-addr.arpa"

	if !strings.HasSuffix(ptr, suffix) {
		return netip.Addr{}, fmt.Errorf("%w: %s", errNotPTRName, ptr)
	}

	parts := strings.Split(strings.TrimSuffix(ptr, suffix), ".")
	if len(parts) != 4 {
		return netip.Addr{}, fmt.Errorf("%w: %s", errInvalidPTR, ptr)
	}

	// Reverse the octets.
	ipStr := parts[3] + "." + parts[2] + "." + parts[1] + "." + parts[0]

	addr, err := netip.ParseAddr(ipStr)
	if err != nil {
		return netip.Addr{}, fmt.Errorf("parse PTR IP %s: %w", ipStr, err)
	}

	return addr, nil
}
