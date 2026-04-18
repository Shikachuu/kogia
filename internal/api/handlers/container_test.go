package handlers

import (
	"net/netip"
	"sort"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	mobynetwork "github.com/moby/moby/api/types/network"
)

func TestParseSince(t *testing.T) {
	t.Parallel()

	t.Run("unix timestamp integer", func(t *testing.T) {
		t.Parallel()

		got, err := parseSince("1700000000")
		assertNoErr(t, err)
		assertUnix(t, got, 1700000000)
	})

	t.Run("unix timestamp float", func(t *testing.T) {
		t.Parallel()

		got, err := parseSince("1700000000.123456789")
		assertNoErr(t, err)
		assertUnix(t, got, 1700000000)

		if got.Nanosecond() == 0 {
			t.Error("expected non-zero nanoseconds")
		}
	})

	t.Run("duration string", func(t *testing.T) {
		t.Parallel()

		now := time.Now()

		got, err := parseSince("5m")
		assertNoErr(t, err)

		diff := now.Sub(got)
		if diff < 4*time.Minute || diff > 6*time.Minute {
			t.Errorf("duration offset = %v, want ~5m", diff)
		}
	})

	t.Run("RFC3339", func(t *testing.T) {
		t.Parallel()

		got, err := parseSince("2024-01-15T10:30:00Z")
		assertNoErr(t, err)

		want, _ := time.Parse(time.RFC3339, "2024-01-15T10:30:00Z")
		if !got.Equal(want) {
			t.Errorf("got %v, want %v", got, want)
		}
	})

	t.Run("RFC3339Nano", func(t *testing.T) {
		t.Parallel()

		got, err := parseSince("2024-01-15T10:30:00.123456789Z")
		assertNoErr(t, err)

		if got.Nanosecond() != 123456789 {
			t.Errorf("nanosecond = %d, want 123456789", got.Nanosecond())
		}
	})

	t.Run("invalid input", func(t *testing.T) {
		t.Parallel()

		_, err := parseSince("not-a-time")
		if err == nil {
			t.Fatal("expected error for invalid input")
		}
	})
}

func assertNoErr(t *testing.T, err error) {
	t.Helper()

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func assertUnix(t *testing.T, got time.Time, wantSec int64) {
	t.Helper()

	if got.Unix() != wantSec {
		t.Errorf("unix = %d, want %d", got.Unix(), wantSec)
	}
}

func TestInspectToSummary(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		input        *container.InspectResponse
		wantID       string
		wantImage    string
		wantState    container.ContainerState
		wantLabels   map[string]string
		wantPorts    []container.PortSummary
		wantNetworks []string // expected network names in NetworkSettings
	}{
		{
			name: "basic fields without network",
			input: &container.InspectResponse{
				ID:    "abc123",
				Name:  "/myapp",
				Image: "sha256:deadbeef",
				Config: &container.Config{
					Image:  "nginx:latest",
					Labels: map[string]string{"app": "web"},
					Cmd:    []string{"/bin/sh", "-c", "echo hello"},
				},
				State: &container.State{Status: container.StateRunning},
			},
			wantID:    "abc123",
			wantImage: "nginx:latest",
			wantState: container.StateRunning,
			wantLabels: map[string]string{"app": "web"},
		},
		{
			name: "nil config and state",
			input: &container.InspectResponse{
				ID: "minimal",
			},
			wantID: "minimal",
		},
		{
			name: "nil network settings does not panic",
			input: &container.InspectResponse{
				ID:     "nonet",
				Config: &container.Config{Image: "alpine"},
			},
			wantID:    "nonet",
			wantImage: "alpine",
		},
		{
			name: "port with host binding",
			input: &container.InspectResponse{
				ID:     "withports",
				Config: &container.Config{},
				NetworkSettings: &container.NetworkSettings{
					Ports: mobynetwork.PortMap{
						mobynetwork.MustParsePort("80/tcp"): {
							{HostIP: netip.MustParseAddr("0.0.0.0"), HostPort: "8080"},
						},
					},
				},
			},
			wantID: "withports",
			wantPorts: []container.PortSummary{
				{PrivatePort: 80, PublicPort: 8080, Type: "tcp", IP: netip.MustParseAddr("0.0.0.0")},
			},
		},
		{
			name: "exposed port without host binding",
			input: &container.InspectResponse{
				ID:     "exposed",
				Config: &container.Config{},
				NetworkSettings: &container.NetworkSettings{
					Ports: mobynetwork.PortMap{
						mobynetwork.MustParsePort("443/tcp"): {},
					},
				},
			},
			wantID: "exposed",
			wantPorts: []container.PortSummary{
				{PrivatePort: 443, Type: "tcp"},
			},
		},
		{
			name: "single network",
			input: &container.InspectResponse{
				ID:     "onenet",
				Config: &container.Config{},
				NetworkSettings: &container.NetworkSettings{
					Networks: map[string]*mobynetwork.EndpointSettings{
						"bridge": {
							NetworkID: "net1",
							IPAddress: netip.MustParseAddr("172.17.0.2"),
						},
					},
				},
			},
			wantID:       "onenet",
			wantNetworks: []string{"bridge"},
		},
		{
			name: "multiple networks",
			input: &container.InspectResponse{
				ID:     "multinet",
				Config: &container.Config{},
				NetworkSettings: &container.NetworkSettings{
					Networks: map[string]*mobynetwork.EndpointSettings{
						"bridge": {
							NetworkID: "net1",
							IPAddress: netip.MustParseAddr("172.17.0.2"),
						},
						"custom": {
							NetworkID: "net2",
							IPAddress: netip.MustParseAddr("10.0.0.5"),
						},
					},
				},
			},
			wantID:       "multinet",
			wantNetworks: []string{"bridge", "custom"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := inspectToSummary(tt.input)

			if got.ID != tt.wantID {
				t.Errorf("ID = %q, want %q", got.ID, tt.wantID)
			}

			if tt.wantImage != "" && got.Image != tt.wantImage {
				t.Errorf("Image = %q, want %q", got.Image, tt.wantImage)
			}

			if tt.wantState != "" && got.State != tt.wantState {
				t.Errorf("State = %q, want %q", got.State, tt.wantState)
			}

			assertLabels(t, got.Labels, tt.wantLabels)
			assertPorts(t, got.Ports, tt.wantPorts)
			assertNetworkNames(t, got.NetworkSettings, tt.wantNetworks)

			// Nil NetworkSettings input should not produce non-nil output.
			if tt.wantNetworks == nil && tt.wantPorts == nil && got.NetworkSettings != nil {
				t.Error("NetworkSettings should be nil when input has no network settings")
			}
		})
	}
}

func assertLabels(t *testing.T, got, want map[string]string) {
	t.Helper()

	for k, v := range want {
		if got[k] != v {
			t.Errorf("Labels[%q] = %q, want %q", k, got[k], v)
		}
	}
}

func assertPorts(t *testing.T, got, want []container.PortSummary) {
	t.Helper()

	if want == nil {
		return
	}

	if len(got) != len(want) {
		t.Fatalf("Ports count = %d, want %d", len(got), len(want))
	}

	for i, wp := range want {
		gp := got[i]
		if gp.PrivatePort != wp.PrivatePort || gp.PublicPort != wp.PublicPort || gp.Type != wp.Type {
			t.Errorf("Ports[%d] = {%d, %d, %q}, want {%d, %d, %q}",
				i, gp.PrivatePort, gp.PublicPort, gp.Type,
				wp.PrivatePort, wp.PublicPort, wp.Type)
		}
	}
}

func assertNetworkNames(t *testing.T, got *container.NetworkSettingsSummary, want []string) {
	t.Helper()

	if want == nil {
		return
	}

	if got == nil {
		t.Fatal("NetworkSettings is nil, want networks")
	}

	var gotNets []string
	for name := range got.Networks {
		gotNets = append(gotNets, name)
	}

	sort.Strings(gotNets)
	sort.Strings(want)

	if len(gotNets) != len(want) {
		t.Fatalf("networks = %v, want %v", gotNets, want)
	}

	for i := range gotNets {
		if gotNets[i] != want[i] {
			t.Errorf("network[%d] = %q, want %q", i, gotNets[i], want[i])
		}
	}
}
