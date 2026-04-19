package image

import (
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"
)

func TestAuthFromHeader(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		header   string
		wantUser string
		wantNil  bool
	}{
		{
			name:    "empty header",
			header:  "",
			wantNil: true,
		},
		{
			name:    "invalid base64",
			header:  "not-valid-base64!!!",
			wantNil: true,
		},
		{
			name:     "valid url-encoded credentials",
			header:   base64.URLEncoding.EncodeToString([]byte(`{"username":"user","password":"pass"}`)),
			wantUser: "user",
		},
		{
			name:     "valid std-encoded credentials",
			header:   base64.StdEncoding.EncodeToString([]byte(`{"username":"admin","password":"secret"}`)),
			wantUser: "admin",
		},
		{
			name:    "valid base64 but empty credentials",
			header:  base64.URLEncoding.EncodeToString([]byte(`{}`)),
			wantNil: true,
		},
		{
			name:     "identity token",
			header:   base64.URLEncoding.EncodeToString([]byte(`{"identitytoken":"tok123"}`)),
			wantNil:  false,
			wantUser: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			auth := AuthFromHeader(tt.header)
			if tt.wantNil {
				if auth != nil {
					t.Fatalf("expected nil, got %+v", auth)
				}

				return
			}

			if auth == nil {
				t.Fatal("expected non-nil auth")
			}

			if auth.Username != tt.wantUser {
				t.Errorf("username = %q, want %q", auth.Username, tt.wantUser)
			}
		})
	}
}

func TestCheckAuth(t *testing.T) {
	t.Parallel()

	tests := []struct {
		auth    *AuthConfig
		name    string
		wantErr bool
	}{
		{
			name:    "identity token skips registry validation",
			auth:    &AuthConfig{IdentityToken: "opaque-token-123"},
			wantErr: false,
		},
		{
			name:    "identity token with empty username",
			auth:    &AuthConfig{IdentityToken: "tok", Username: "", Password: ""},
			wantErr: false,
		},
		{
			name:    "bad credentials against unreachable registry",
			auth:    &AuthConfig{Username: "user", Password: "pass", ServerAddress: "localhost:1"},
			wantErr: true,
		},
	}

	s := &Store{}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := s.CheckAuth(context.Background(), tt.auth)
			if (err != nil) != tt.wantErr {
				t.Errorf("CheckAuth() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestAuthFromDockerConfig(t *testing.T) {
	// Cannot use t.Parallel() with t.Setenv().

	// Create a temp config file.
	dir := t.TempDir()
	configJSON := `{
		"auths": {
			"https://index.docker.io/v1/": {
				"auth": "` + base64.StdEncoding.EncodeToString([]byte("myuser:mypass")) + `"
			},
			"ghcr.io": {
				"auth": "` + base64.StdEncoding.EncodeToString([]byte("ghuser:ghtoken")) + `"
			}
		}
	}`

	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(configJSON), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Setenv("DOCKER_CONFIG", dir)

	tests := []struct {
		name     string
		registry string
		wantUser string
		wantNil  bool
	}{
		{
			name:     "docker.io resolves to index.docker.io",
			registry: "docker.io",
			wantUser: "myuser",
		},
		{
			name:     "direct ghcr.io lookup",
			registry: "ghcr.io",
			wantUser: "ghuser",
		},
		{
			name:     "empty registry defaults to docker hub",
			registry: "",
			wantUser: "myuser",
		},
		{
			name:     "unknown registry",
			registry: "quay.io",
			wantNil:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			auth := AuthFromDockerConfig(tt.registry)
			if tt.wantNil {
				if auth != nil {
					t.Fatalf("expected nil, got %+v", auth)
				}

				return
			}

			if auth == nil {
				t.Fatal("expected non-nil auth")
			}

			if auth.Username != tt.wantUser {
				t.Errorf("username = %q, want %q", auth.Username, tt.wantUser)
			}
		})
	}
}
