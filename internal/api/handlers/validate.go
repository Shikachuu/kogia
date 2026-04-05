package handlers

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/Shikachuu/kogia/internal/api/errdefs"
	"github.com/distribution/reference"
	"github.com/moby/moby/api/types/container"
)

// containerNameRe matches a valid Docker container name (with optional leading slash).
var containerNameRe = regexp.MustCompile(`^/?[a-zA-Z0-9][a-zA-Z0-9_.-]+$`)

const containerNameMaxLen = 255

// validateContainerName checks that name is a valid Docker container name.
func validateContainerName(name string) error {
	if len(name) > containerNameMaxLen {
		return fmt.Errorf("validate container name: %w",
			errdefs.InvalidParameter(
				fmt.Sprintf("invalid container name %q: name exceeds %d characters", name, containerNameMaxLen), nil))
	}

	if !containerNameRe.MatchString(name) {
		return fmt.Errorf("validate container name: %w",
			errdefs.InvalidParameter(
				fmt.Sprintf("invalid container name %q: must match %s", name, containerNameRe.String()), nil))
	}

	return nil
}

// validateImageRef checks that ref is a non-empty, valid image reference.
func validateImageRef(ref string) error {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return fmt.Errorf("validate image ref: %w",
			errdefs.InvalidParameter("invalid image reference: empty string", nil))
	}

	if _, err := reference.ParseNormalizedNamed(ref); err != nil {
		return fmt.Errorf("validate image ref: %w",
			errdefs.InvalidParameter(fmt.Sprintf("invalid image reference %q: %s", ref, err), nil))
	}

	return nil
}

// knownSignals is the set of accepted signal names (upper-case, without SIG prefix).
var knownSignals = map[string]struct{}{
	"TERM": {}, "KILL": {}, "HUP": {}, "USR1": {}, "USR2": {},
	"INT": {}, "QUIT": {}, "STOP": {}, "CONT": {},
}

// validateSignal checks that signal is a known signal name (with or without SIG
// prefix, case-insensitive) or a numeric value in the range 1-31.
func validateSignal(signal string) error {
	if signal == "" {
		return fmt.Errorf("validate signal: %w",
			errdefs.InvalidParameter("invalid signal: empty string", nil))
	}

	// Try numeric.
	if n, err := strconv.Atoi(signal); err == nil {
		if n < 1 || n > 31 {
			return fmt.Errorf("validate signal: %w",
				errdefs.InvalidParameter(
					fmt.Sprintf("invalid signal: %d (must be 1-31)", n), nil))
		}

		return nil
	}

	// Normalize: upper-case, strip SIG prefix.
	name := strings.ToUpper(signal)
	name = strings.TrimPrefix(name, "SIG")

	if _, ok := knownSignals[name]; !ok {
		return fmt.Errorf("validate signal: %w",
			errdefs.InvalidParameter("invalid signal: "+signal, nil))
	}

	return nil
}

// validateTimeout parses a timeout query string value. An empty string returns
// defaultVal. The value must be a non-negative integer.
func validateTimeout(raw string, defaultVal int) (int, error) {
	if raw == "" {
		return defaultVal, nil
	}

	v, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("validate timeout: %w",
			errdefs.InvalidParameter(
				fmt.Sprintf("invalid timeout value: %q", raw), nil))
	}

	if v < 0 {
		return 0, fmt.Errorf("validate timeout: %w",
			errdefs.InvalidParameter(
				fmt.Sprintf("invalid timeout value: %d (must be >= 0)", v), nil))
	}

	return v, nil
}

// validateContainerConfig validates a container.Config for creation.
func validateContainerConfig(cfg *container.Config) error {
	if cfg == nil {
		return fmt.Errorf("validate container config: %w",
			errdefs.InvalidParameter("config is required", nil))
	}

	if strings.TrimSpace(cfg.Image) == "" {
		return fmt.Errorf("validate container config: %w",
			errdefs.InvalidParameter("image is required", nil))
	}

	if cfg.WorkingDir != "" && !strings.HasPrefix(cfg.WorkingDir, "/") {
		return fmt.Errorf("validate container config: %w",
			errdefs.InvalidParameter(
				fmt.Sprintf("working directory %q must be absolute", cfg.WorkingDir), nil))
	}

	for _, env := range cfg.Env {
		idx := strings.IndexByte(env, '=')
		if idx < 0 {
			return fmt.Errorf("validate container config: %w",
				errdefs.InvalidParameter(
					fmt.Sprintf("invalid environment variable %q: must contain '='", env), nil))
		}

		if idx == 0 {
			return fmt.Errorf("validate container config: %w",
				errdefs.InvalidParameter(
					fmt.Sprintf("invalid environment variable %q: empty key", env), nil))
		}
	}

	return nil
}

const minMemoryBytes = 6 * 1024 * 1024 // 6 MB

// validRestartPolicies enumerates allowed restart policy names.
var validRestartPolicies = map[string]struct{}{
	"":               {},
	"no":             {},
	"always":         {},
	"on-failure":     {},
	"unless-stopped": {},
}

// validateHostConfig validates a container.HostConfig for creation.
func validateHostConfig(hc *container.HostConfig) error {
	if hc == nil {
		return nil
	}

	if hc.Memory != 0 && hc.Memory < int64(minMemoryBytes) {
		return fmt.Errorf("validate host config: %w",
			errdefs.InvalidParameter(
				fmt.Sprintf("minimum memory limit is 6MB, got %d bytes", hc.Memory), nil))
	}

	if hc.MemorySwap != 0 && hc.Memory == 0 {
		return fmt.Errorf("validate host config: %w",
			errdefs.InvalidParameter("memory swap requires memory limit to be set", nil))
	}

	if _, ok := validRestartPolicies[string(hc.RestartPolicy.Name)]; !ok {
		return fmt.Errorf("validate host config: %w",
			errdefs.InvalidParameter(
				fmt.Sprintf("invalid restart policy %q", hc.RestartPolicy.Name), nil))
	}

	if hc.RestartPolicy.MaximumRetryCount < 0 {
		return fmt.Errorf("validate host config: %w",
			errdefs.InvalidParameter(
				fmt.Sprintf("invalid restart policy maximum retry count: %d (must be >= 0)",
					hc.RestartPolicy.MaximumRetryCount), nil))
	}

	logType := hc.LogConfig.Type
	if logType != "" && logType != "json-file" {
		return fmt.Errorf("validate host config: %w",
			errdefs.InvalidParameter(
				fmt.Sprintf("unsupported log driver %q (supported: json-file)", logType), nil))
	}

	return nil
}
