// Package network implements container bridge networking.
package network

import (
	"net/netip"
	"time"
)

// Record is the internal representation of a Docker network stored in bbolt.
type Record struct {
	Created    time.Time         `json:"created"`
	Gateway    netip.Addr        `json:"gateway"`
	Options    map[string]string `json:"options,omitempty"`
	Labels     map[string]string `json:"labels,omitempty"`
	Subnet     netip.Prefix      `json:"subnet"`
	ID         string            `json:"id"`
	Name       string            `json:"name"`
	Driver     string            `json:"driver"`
	BridgeName string            `json:"bridgeName"`
	Internal   bool              `json:"internal,omitempty"`
}

// EndpointRecord is persisted per container-network pair.
type EndpointRecord struct {
	NetworkID     string        `json:"networkID"`
	ContainerID   string        `json:"containerID"`
	ContainerName string        `json:"containerName"`
	IPAddress     netip.Addr    `json:"ipAddress"`
	MacAddress    string        `json:"macAddress"`
	VethHost      string        `json:"vethHost"` // host-side veth name (for cleanup)
	PortMappings  []PortMapping `json:"portMappings,omitempty"`
	DNSNames      []string      `json:"dnsNames,omitempty"` // container name + aliases
}

// PortMapping describes a host-to-container port forwarding rule.
type PortMapping struct {
	HostIP        netip.Addr `json:"hostIP"`
	Protocol      string     `json:"protocol"`
	HostPort      uint16     `json:"hostPort"`
	ContainerPort uint16     `json:"containerPort"`
}
