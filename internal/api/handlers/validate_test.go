package handlers

import (
	"strings"
	"testing"

	"github.com/Shikachuu/kogia/internal/api/errdefs"
	"github.com/moby/moby/api/types/container"
)

func TestValidateContainerName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		input   string
		wantErr bool
	}{
		{"valid simple", "mycontainer", false},
		{"valid with dash", "my-container", false},
		{"valid with dot", "my.container", false},
		{"valid with underscore", "my_container", false},
		{"valid with leading slash", "/mycontainer", false},
		{"valid alphanumeric", "abc123", false},
		{"invalid empty", "", true},
		{"invalid single char", "a", true},
		{"invalid spaces", "my container", true},
		{"invalid special chars", "my@container", true},
		{"invalid starts with dash", "-mycontainer", true},
		{"invalid starts with dot", ".mycontainer", true},
		{"too long", strings.Repeat("a", 256), true},
		{"max length", strings.Repeat("a", 255), false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := validateContainerName(tt.input)
			if (err != nil) != tt.wantErr {
				t.Errorf("validateContainerName(%q) error = %v, wantErr %v", tt.input, err, tt.wantErr)
			}

			if err != nil && !errdefs.IsInvalidParameter(err) {
				t.Errorf("expected InvalidParameterError, got %T", err)
			}
		})
	}
}

func TestValidateImageRef(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		input   string
		wantErr bool
	}{
		{"valid simple", "alpine", false},
		{"valid with tag", "alpine:latest", false},
		{"valid with registry", "docker.io/library/alpine:3.18", false},
		{"valid with digest", "alpine@sha256:abcdef1234567890abcdef1234567890abcdef1234567890abcdef1234567890", false},
		{"valid ghcr", "ghcr.io/myorg/myimage:v1.0", false},
		{"invalid empty", "", true},
		{"invalid whitespace only", "   ", true},
		{"invalid format", "INVALID:!@#$", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := validateImageRef(tt.input)
			if (err != nil) != tt.wantErr {
				t.Errorf("validateImageRef(%q) error = %v, wantErr %v", tt.input, err, tt.wantErr)
			}

			if err != nil && !errdefs.IsInvalidParameter(err) {
				t.Errorf("expected InvalidParameterError, got %T", err)
			}
		})
	}
}

func TestValidateSignal(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		input   string
		wantErr bool
	}{
		{"SIGTERM", "SIGTERM", false},
		{"SIGKILL", "SIGKILL", false},
		{"SIGHUP", "SIGHUP", false},
		{"lowercase sigterm", "sigterm", false},
		{"without prefix KILL", "KILL", false},
		{"without prefix term", "term", false},
		{"numeric 9", "9", false},
		{"numeric 1", "1", false},
		{"numeric 31", "31", false},
		{"SIGUSR1", "SIGUSR1", false},
		{"SIGUSR2", "SIGUSR2", false},
		{"SIGINT", "SIGINT", false},
		{"SIGQUIT", "SIGQUIT", false},
		{"SIGSTOP", "SIGSTOP", false},
		{"SIGCONT", "SIGCONT", false},
		{"invalid empty", "", true},
		{"invalid signal name", "SIGFOO", true},
		{"numeric 0", "0", true},
		{"numeric 32", "32", true},
		{"numeric negative", "-1", true},
		{"invalid string", "notasignal", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := validateSignal(tt.input)
			if (err != nil) != tt.wantErr {
				t.Errorf("validateSignal(%q) error = %v, wantErr %v", tt.input, err, tt.wantErr)
			}

			if err != nil && !errdefs.IsInvalidParameter(err) {
				t.Errorf("expected InvalidParameterError, got %T", err)
			}
		})
	}
}

func TestValidateTimeout(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		raw        string
		defaultVal int
		want       int
		wantErr    bool
	}{
		{"empty returns default", "", 10, 10, false},
		{"valid zero", "0", 10, 0, false},
		{"valid positive", "30", 10, 30, false},
		{"valid large", "3600", 10, 3600, false},
		{"negative", "-1", 10, 0, true},
		{"non-numeric", "abc", 10, 0, true},
		{"float", "1.5", 10, 0, true},
		{"empty with zero default", "", 0, 0, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := validateTimeout(tt.raw, tt.defaultVal)
			if (err != nil) != tt.wantErr {
				t.Errorf("validateTimeout(%q, %d) error = %v, wantErr %v", tt.raw, tt.defaultVal, err, tt.wantErr)
			}

			if err != nil && !errdefs.IsInvalidParameter(err) {
				t.Errorf("expected InvalidParameterError, got %T", err)
			}

			if !tt.wantErr && got != tt.want {
				t.Errorf("validateTimeout(%q, %d) = %d, want %d", tt.raw, tt.defaultVal, got, tt.want)
			}
		})
	}
}

func TestValidateContainerConfig(t *testing.T) {
	t.Parallel()

	tests := []struct {
		cfg     *container.Config
		name    string
		wantErr bool
	}{
		{name: "nil config", cfg: nil, wantErr: true},
		{name: "valid minimal", cfg: &container.Config{Image: "alpine"}, wantErr: false},
		{name: "empty image", cfg: &container.Config{Image: ""}, wantErr: true},
		{name: "whitespace image", cfg: &container.Config{Image: "   "}, wantErr: true},
		{name: "valid with workdir", cfg: &container.Config{Image: "alpine", WorkingDir: "/app"}, wantErr: false},
		{name: "relative workdir", cfg: &container.Config{Image: "alpine", WorkingDir: "app"}, wantErr: true},
		{name: "valid env", cfg: &container.Config{Image: "alpine", Env: []string{"FOO=bar", "BAZ="}}, wantErr: false},
		{name: "env missing equals", cfg: &container.Config{Image: "alpine", Env: []string{"FOO"}}, wantErr: true},
		{name: "env empty key", cfg: &container.Config{Image: "alpine", Env: []string{"=value"}}, wantErr: true},
		{name: "valid full config", cfg: &container.Config{
			Image:      "alpine:latest",
			WorkingDir: "/home/user",
			Env:        []string{"PATH=/usr/bin", "HOME=/home/user"},
		}, wantErr: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := validateContainerConfig(tt.cfg)
			if (err != nil) != tt.wantErr {
				t.Errorf("validateContainerConfig() error = %v, wantErr %v", err, tt.wantErr)
			}

			if err != nil && !errdefs.IsInvalidParameter(err) {
				t.Errorf("expected InvalidParameterError, got %T", err)
			}
		})
	}
}

func TestValidateHostConfig(t *testing.T) {
	t.Parallel()

	tests := []struct {
		hc      *container.HostConfig
		name    string
		wantErr bool
	}{
		{name: "nil hostconfig", hc: nil, wantErr: false},
		{name: "empty hostconfig", hc: &container.HostConfig{}, wantErr: false},
		{name: "valid memory", hc: &container.HostConfig{Resources: container.Resources{Memory: 8 * 1024 * 1024}}, wantErr: false},
		{name: "memory zero (unlimited)", hc: &container.HostConfig{Resources: container.Resources{Memory: 0}}, wantErr: false},
		{name: "memory too low", hc: &container.HostConfig{Resources: container.Resources{Memory: 1024}}, wantErr: true},
		{name: "memory exactly 6MB", hc: &container.HostConfig{Resources: container.Resources{Memory: 6 * 1024 * 1024}}, wantErr: false},
		{name: "memory swap without memory", hc: &container.HostConfig{Resources: container.Resources{MemorySwap: 100}}, wantErr: true},
		{name: "memory swap with memory", hc: &container.HostConfig{Resources: container.Resources{Memory: 8 * 1024 * 1024, MemorySwap: 16 * 1024 * 1024}}, wantErr: false},
		{name: "valid restart always", hc: &container.HostConfig{
			RestartPolicy: container.RestartPolicy{Name: "always"},
		}, wantErr: false},
		{name: "valid restart on-failure", hc: &container.HostConfig{
			RestartPolicy: container.RestartPolicy{Name: "on-failure", MaximumRetryCount: 3},
		}, wantErr: false},
		{name: "valid restart no", hc: &container.HostConfig{
			RestartPolicy: container.RestartPolicy{Name: "no"},
		}, wantErr: false},
		{name: "valid restart unless-stopped", hc: &container.HostConfig{
			RestartPolicy: container.RestartPolicy{Name: "unless-stopped"},
		}, wantErr: false},
		{name: "invalid restart policy", hc: &container.HostConfig{
			RestartPolicy: container.RestartPolicy{Name: "invalid"},
		}, wantErr: true},
		{name: "negative retry count", hc: &container.HostConfig{
			RestartPolicy: container.RestartPolicy{Name: "on-failure", MaximumRetryCount: -1},
		}, wantErr: true},
		{name: "valid log driver json-file", hc: &container.HostConfig{
			LogConfig: container.LogConfig{Type: "json-file"},
		}, wantErr: false},
		{name: "empty log driver", hc: &container.HostConfig{
			LogConfig: container.LogConfig{Type: ""},
		}, wantErr: false},
		{name: "unsupported log driver", hc: &container.HostConfig{
			LogConfig: container.LogConfig{Type: "syslog"},
		}, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := validateHostConfig(tt.hc)
			if (err != nil) != tt.wantErr {
				t.Errorf("validateHostConfig() error = %v, wantErr %v", err, tt.wantErr)
			}

			if err != nil && !errdefs.IsInvalidParameter(err) {
				t.Errorf("expected InvalidParameterError, got %T", err)
			}
		})
	}
}
