package network

import (
	"net/netip"
	"testing"
)

func TestPtrToIP(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		ptr     string
		wantIP  string
		wantErr bool
	}{
		{
			name:   "valid PTR",
			ptr:    "2.0.17.172.in-addr.arpa.",
			wantIP: "172.17.0.2",
		},
		{
			name:   "valid PTR without trailing dot",
			ptr:    "100.1.168.192.in-addr.arpa",
			wantIP: "192.168.1.100",
		},
		{
			name:    "missing suffix",
			ptr:     "2.0.17.172.example.com.",
			wantErr: true,
		},
		{
			name:    "wrong number of octets",
			ptr:     "0.17.172.in-addr.arpa.",
			wantErr: true,
		},
		{
			name:    "invalid octet",
			ptr:     "abc.0.17.172.in-addr.arpa.",
			wantErr: true,
		},
		{
			name:    "empty string",
			ptr:     "",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			ip, err := ptrToIP(tt.ptr)
			if tt.wantErr {
				if err == nil {
					t.Errorf("expected error, got ip=%s", ip)
				}

				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if ip.String() != tt.wantIP {
				t.Errorf("ptrToIP(%q) = %s, want %s", tt.ptr, ip, tt.wantIP)
			}
		})
	}
}

func TestDNS_RegisterLookup(t *testing.T) {
	t.Parallel()

	d := &DNS{
		records:   make(map[string]map[string]netip.Addr),
		listeners: make(map[string]*dnsListener),
		cache:     make(map[string]*dnsCache),
	}

	networkID := "abc123def456abc123def456abc123def456abc123def456abc123def456abcd"
	ip := netip.MustParseAddr("172.18.0.2")

	// Register.
	d.Register(networkID, "mycontainer", ip)

	// Lookup should succeed (with trailing dot as FQDN).
	got := d.lookupA("mycontainer.")
	if got != ip {
		t.Errorf("lookupA(mycontainer.) = %s, want %s", got, ip)
	}

	// Lookup without trailing dot.
	got = d.lookupA("mycontainer")
	if got != ip {
		t.Errorf("lookupA(mycontainer) = %s, want %s", got, ip)
	}

	// Unknown name.
	got = d.lookupA("unknown")
	if got.IsValid() {
		t.Errorf("lookupA(unknown) = %s, want invalid", got)
	}

	// Deregister.
	d.Deregister(networkID, "mycontainer")

	got = d.lookupA("mycontainer")
	if got.IsValid() {
		t.Errorf("after deregister: lookupA(mycontainer) = %s, want invalid", got)
	}
}

func TestDNS_LookupPTR(t *testing.T) {
	t.Parallel()

	d := &DNS{
		records:   make(map[string]map[string]netip.Addr),
		listeners: make(map[string]*dnsListener),
		cache:     make(map[string]*dnsCache),
	}

	networkID := "abc123def456abc123def456abc123def456abc123def456abc123def456abcd"
	ip := netip.MustParseAddr("172.17.0.5")

	d.Register(networkID, "webserver", ip)

	// PTR lookup: 5.0.17.172.in-addr.arpa.
	name := d.lookupPTR("5.0.17.172.in-addr.arpa.")
	if name != "webserver" {
		t.Errorf("lookupPTR = %q, want %q", name, "webserver")
	}

	// Unknown PTR.
	name = d.lookupPTR("99.0.17.172.in-addr.arpa.")
	if name != "" {
		t.Errorf("lookupPTR(unknown) = %q, want empty", name)
	}
}

func TestDNS_DeregisterNetwork(t *testing.T) {
	t.Parallel()

	d := &DNS{
		records:   make(map[string]map[string]netip.Addr),
		listeners: make(map[string]*dnsListener),
		cache:     make(map[string]*dnsCache),
	}

	networkID := "abc123def456abc123def456abc123def456abc123def456abc123def456abcd"

	d.Register(networkID, "container1", netip.MustParseAddr("10.0.0.2"))
	d.Register(networkID, "container2", netip.MustParseAddr("10.0.0.3"))

	// Both should resolve.
	if !d.lookupA("container1").IsValid() {
		t.Error("container1 should resolve before DeregisterNetwork")
	}

	if !d.lookupA("container2").IsValid() {
		t.Error("container2 should resolve before DeregisterNetwork")
	}

	d.DeregisterNetwork(networkID)

	// Neither should resolve now.
	if d.lookupA("container1").IsValid() {
		t.Error("container1 should not resolve after DeregisterNetwork")
	}

	if d.lookupA("container2").IsValid() {
		t.Error("container2 should not resolve after DeregisterNetwork")
	}
}
