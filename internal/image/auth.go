package image

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/containers/image/v5/docker"
	imagetypes "github.com/containers/image/v5/types"
)

// AuthConfig holds registry credentials for image operations.
type AuthConfig struct {
	Username      string `json:"username,omitempty"`
	Password      string `json:"password,omitempty"`
	IdentityToken string `json:"identitytoken,omitempty"`
	ServerAddress string `json:"serveraddress,omitempty"`
}

// IsEmpty returns true if the config has no usable credentials.
func (a *AuthConfig) IsEmpty() bool {
	return a == nil || (a.Username == "" && a.Password == "" && a.IdentityToken == "")
}

// AuthFromHeader decodes the X-Registry-Auth header value.
// The value is base64url-encoded JSON. Returns nil if empty or invalid.
func AuthFromHeader(headerValue string) *AuthConfig {
	if headerValue == "" {
		return nil
	}

	decoded, err := base64.URLEncoding.DecodeString(headerValue)
	if err != nil {
		// Docker CLI sometimes uses standard base64 encoding.
		decoded, err = base64.StdEncoding.DecodeString(headerValue)
		if err != nil {
			return nil
		}
	}

	var auth AuthConfig

	if unmarshalErr := json.Unmarshal(decoded, &auth); unmarshalErr != nil {
		return nil
	}

	if auth.IsEmpty() {
		return nil
	}

	return &auth
}

// dockerConfigFile represents the structure of ~/.docker/config.json.
type dockerConfigFile struct {
	Auths map[string]dockerConfigAuth `json:"auths"`
}

// dockerConfigAuth represents a single registry entry in the config.
type dockerConfigAuth struct {
	Auth string `json:"auth"` // base64-encoded "username:password"
}

// AuthFromDockerConfig reads credentials for registry from ~/.docker/config.json.
// Only supports inline auth field; credsStore/credHelpers are not handled.
// Returns nil if no credentials are found.
func AuthFromDockerConfig(registry string) *AuthConfig {
	configPath := dockerConfigPath()
	if configPath == "" {
		return nil
	}

	data, err := os.ReadFile(configPath) //nolint:gosec // Path is derived from well-known config locations, not user input.
	if err != nil {
		return nil
	}

	var cfg dockerConfigFile

	if unmarshalErr := json.Unmarshal(data, &cfg); unmarshalErr != nil {
		return nil
	}

	if registry == "" {
		registry = "https://index.docker.io/v1/"
	}

	entry, ok := cfg.Auths[registry]
	if !ok {
		// Try with/without https:// prefix and trailing slash variants.
		for _, candidate := range registryVariants(registry) {
			if entry, ok = cfg.Auths[candidate]; ok {
				break
			}
		}
	}

	if !ok || entry.Auth == "" {
		return nil
	}

	decoded, err := base64.StdEncoding.DecodeString(entry.Auth)
	if err != nil {
		return nil
	}

	username, password, found := strings.Cut(string(decoded), ":")
	if !found {
		return nil
	}

	return &AuthConfig{
		Username:      username,
		Password:      password,
		ServerAddress: registry,
	}
}

// CheckAuth validates registry credentials by hitting the registry's /v2/ endpoint.
// Returns nil on success, or the raw error from the registry check.
// Callers should use errors.As with docker.ErrUnauthorizedForCredentials to
// distinguish auth failures from network errors.
func (s *Store) CheckAuth(ctx context.Context, auth *AuthConfig) error {
	if auth.IdentityToken != "" {
		// Identity tokens are opaque; we can't validate them server-side
		// without a full OAuth flow. Accept and let the CLI persist them.
		return nil
	}

	sysCtx := &imagetypes.SystemContext{}

	if err := docker.CheckAuth(ctx, sysCtx, auth.Username, auth.Password, auth.ServerAddress); err != nil {
		return fmt.Errorf("auth: check credentials for %s: %w", auth.ServerAddress, err)
	}

	return nil
}

// ResolveAuth returns credentials using the priority: header → config.json → nil.
func ResolveAuth(headerValue, registry string) *AuthConfig {
	if auth := AuthFromHeader(headerValue); auth != nil {
		return auth
	}

	return AuthFromDockerConfig(registry)
}

func dockerConfigPath() string {
	if p := os.Getenv("DOCKER_CONFIG"); p != "" {
		return filepath.Join(p, "config.json")
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}

	return filepath.Join(home, ".docker", "config.json")
}

func registryVariants(registry string) []string {
	variants := make([]string, 0, 4)

	// docker.io → index.docker.io variants
	if registry == "docker.io" || registry == "https://docker.io" {
		variants = append(variants,
			"https://index.docker.io/v1/",
			"index.docker.io",
			"https://index.docker.io",
		)
	}

	withHTTPS := "https://" + registry
	withSlash := registry + "/"
	withHTTPSSlash := withHTTPS + "/"

	variants = append(variants, withHTTPS, withSlash, withHTTPSSlash)

	return variants
}
