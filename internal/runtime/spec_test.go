package runtime

import (
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/moby/moby/api/types/container"
	ocispec "github.com/opencontainers/runtime-spec/specs-go"
)

// --- Scenario: hello-world (simplest possible container) ---
// Equivalent: docker run --rm hello-world
// Image has: Entrypoint=["/hello"], Cmd=nil, Env=["PATH=..."].
func TestGenerateSpec_HelloWorld(t *testing.T) {
	t.Parallel()

	spec, err := GenerateSpec(&SpecOpts{
		Config:          &container.Config{},
		HostConfig:      &container.HostConfig{},
		ImageEntrypoint: []string{"/hello"},
		ImageEnv:        []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"},
		RootPath:        "/var/lib/kogia/containers/abc123/rootfs",
		Hostname:        "abc123def456",
	})
	if err != nil {
		t.Fatalf("GenerateSpec: %v", err)
	}

	// OCI version.
	if spec.Version != "1.2.0" {
		t.Errorf("version = %q, want %q", spec.Version, "1.2.0")
	}

	// Process args = image entrypoint only.
	assertStringSlice(t, "args", spec.Process.Args, []string{"/hello"})

	// Hostname.
	if spec.Hostname != "abc123def456" {
		t.Errorf("hostname = %q, want %q", spec.Hostname, "abc123def456")
	}

	// Root.
	if spec.Root.Path != "/var/lib/kogia/containers/abc123/rootfs" {
		t.Errorf("root.path = %q", spec.Root.Path)
	}

	if spec.Root.Readonly {
		t.Error("root.readonly should be false for hello-world")
	}

	// Terminal off for non-TTY.
	if spec.Process.Terminal {
		t.Error("terminal should be false")
	}

	// noNewPrivileges should be true (non-privileged).
	if !spec.Process.NoNewPrivileges {
		t.Error("noNewPrivileges should be true")
	}

	// User should be root.
	if spec.Process.User.UID != 0 || spec.Process.User.GID != 0 {
		t.Errorf("user = %d:%d, want 0:0", spec.Process.User.UID, spec.Process.User.GID)
	}

	// Cwd defaults to "/".
	if spec.Process.Cwd != "/" {
		t.Errorf("cwd = %q, want %q", spec.Process.Cwd, "/")
	}

	// Default capabilities (14).
	if spec.Process.Capabilities == nil {
		t.Fatal("capabilities should not be nil")
	}

	assertCapsContain(t, "bounding", spec.Process.Capabilities.Bounding, defaultCaps)
	assertCapsContain(t, "effective", spec.Process.Capabilities.Effective, defaultCaps)

	// Should have default namespaces: pid, mount, ipc, uts, network.
	assertNamespaces(t, spec.Linux.Namespaces, []ocispec.LinuxNamespaceType{
		ocispec.PIDNamespace, ocispec.MountNamespace, ocispec.IPCNamespace, ocispec.UTSNamespace, ocispec.NetworkNamespace,
	})

	// Masked paths present.
	if len(spec.Linux.MaskedPaths) == 0 {
		t.Error("maskedPaths should not be empty")
	}

	// Readonly paths present.
	if len(spec.Linux.ReadonlyPaths) == 0 {
		t.Error("readonlyPaths should not be empty")
	}

	// Default mounts present (/proc, /dev, /dev/pts, /dev/shm, /dev/mqueue, /sys, /sys/fs/cgroup).
	assertMountDests(t, spec.Mounts, []string{
		"/proc", "/dev", "/dev/pts", "/dev/shm", "/dev/mqueue", "/sys", "/sys/fs/cgroup",
	})

	// No resources for hello-world.
	if spec.Linux.Resources != nil {
		t.Errorf("resources should be nil for no-limit container, got %+v", spec.Linux.Resources)
	}

	// CgroupsPath uses hostname.
	if spec.Linux.CgroupsPath != "kogia/abc123def456" {
		t.Errorf("cgroupsPath = %q, want %q", spec.Linux.CgroupsPath, "kogia/abc123def456")
	}

	// Env should contain PATH and HOSTNAME.
	envMap := envToMap(spec.Process.Env)
	if envMap["PATH"] != "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin" {
		t.Errorf("PATH = %q", envMap["PATH"])
	}

	if envMap["HOSTNAME"] != "abc123def456" {
		t.Errorf("HOSTNAME = %q, want %q", envMap["HOSTNAME"], "abc123def456")
	}
}

// --- Scenario: alpine with explicit command ---
// Equivalent: docker run --rm alpine cat /etc/os-release
// Image has: Entrypoint=nil, Cmd=["/bin/sh"], Env=["PATH=..."]
// User overrides: Cmd=["cat", "/etc/os-release"].
func TestGenerateSpec_AlpineExplicitCmd(t *testing.T) {
	t.Parallel()

	spec, err := GenerateSpec(&SpecOpts{
		Config: &container.Config{
			Cmd: []string{"cat", "/etc/os-release"},
		},
		HostConfig: &container.HostConfig{},
		ImageCmd:   []string{"/bin/sh"},
		ImageEnv:   []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"},
		RootPath:   "/rootfs",
		Hostname:   "abcdef123456",
	})
	if err != nil {
		t.Fatalf("GenerateSpec: %v", err)
	}

	// Container cmd overrides image cmd; no entrypoint.
	assertStringSlice(t, "args", spec.Process.Args, []string{"cat", "/etc/os-release"})
}

// --- Scenario: nginx detached with resource limits ---
// Equivalent: docker run -d --memory=64m --cpus=0.5 --user 1000:1000 --read-only nginx.
func TestGenerateSpec_NginxResourceLimits(t *testing.T) {
	t.Parallel()

	memLimit := int64(64 * 1024 * 1024) // 64 MiB
	pidsLimit := int64(100)

	spec, err := GenerateSpec(&SpecOpts{
		Config: &container.Config{
			User: "1000:1000",
		},
		HostConfig: &container.HostConfig{
			ReadonlyRootfs: true,
			Resources: container.Resources{
				Memory:    memLimit,
				NanoCPUs:  500000000, // 0.5 CPUs
				PidsLimit: &pidsLimit,
			},
		},
		ImageEntrypoint: []string{"/docker-entrypoint.sh"},
		ImageCmd:        []string{"nginx", "-g", "daemon off;"},
		ImageEnv: []string{
			"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
			"NGINX_VERSION=1.25.3",
			"NJS_VERSION=0.8.2",
		},
		ImageCwd: "/",
		RootPath: "/rootfs",
		Hostname: "nginx123456ab",
	})
	if err != nil {
		t.Fatalf("GenerateSpec: %v", err)
	}

	// Args = image entrypoint + image cmd.
	assertStringSlice(t, "args", spec.Process.Args, []string{
		"/docker-entrypoint.sh", "nginx", "-g", "daemon off;",
	})

	// User 1000:1000.
	if spec.Process.User.UID != 1000 {
		t.Errorf("uid = %d, want 1000", spec.Process.User.UID)
	}

	if spec.Process.User.GID != 1000 {
		t.Errorf("gid = %d, want 1000", spec.Process.User.GID)
	}

	// Read-only root.
	if !spec.Root.Readonly {
		t.Error("root.readonly should be true")
	}

	// Memory limit.
	if spec.Linux.Resources == nil || spec.Linux.Resources.Memory == nil {
		t.Fatal("memory resources should be set")
	}

	if *spec.Linux.Resources.Memory.Limit != memLimit {
		t.Errorf("memory.limit = %d, want %d", *spec.Linux.Resources.Memory.Limit, memLimit)
	}

	// NanoCPUs → quota/period: 0.5 CPUs = 50000 quota / 100000 period.
	if spec.Linux.Resources.CPU == nil {
		t.Fatal("CPU resources should be set")
	}

	if spec.Linux.Resources.CPU.Period == nil || *spec.Linux.Resources.CPU.Period != 100000 {
		t.Errorf("cpu.period = %v, want 100000", spec.Linux.Resources.CPU.Period)
	}

	if spec.Linux.Resources.CPU.Quota == nil || *spec.Linux.Resources.CPU.Quota != 50000 {
		t.Errorf("cpu.quota = %d, want 50000 (500000000 * 100000 / 1e9)", *spec.Linux.Resources.CPU.Quota)
	}

	// PIDs limit.
	if spec.Linux.Resources.Pids == nil || spec.Linux.Resources.Pids.Limit != 100 {
		t.Errorf("pids.limit = %v, want 100", spec.Linux.Resources.Pids)
	}

	// Env should have image vars + HOSTNAME.
	envMap := envToMap(spec.Process.Env)
	if envMap["NGINX_VERSION"] != "1.25.3" {
		t.Errorf("NGINX_VERSION = %q, want %q", envMap["NGINX_VERSION"], "1.25.3")
	}
}

// --- Scenario: privileged container ---
// Equivalent: docker run --privileged alpine sh.
func TestGenerateSpec_Privileged(t *testing.T) {
	t.Parallel()

	spec, err := GenerateSpec(&SpecOpts{
		Config: &container.Config{
			Cmd: []string{"sh"},
		},
		HostConfig: &container.HostConfig{
			Privileged: true,
		},
		ImageEnv: []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"},
		RootPath: "/rootfs",
		Hostname: "priv12345678",
	})
	if err != nil {
		t.Fatalf("GenerateSpec: %v", err)
	}

	// noNewPrivileges should be false for privileged.
	if spec.Process.NoNewPrivileges {
		t.Error("noNewPrivileges should be false for privileged container")
	}

	// Should have ALL capabilities.
	if spec.Process.Capabilities == nil {
		t.Fatal("capabilities should not be nil")
	}

	allCaps := allCapabilities()

	if len(spec.Process.Capabilities.Bounding) != len(allCaps.Bounding) {
		t.Errorf("privileged bounding caps = %d, want %d",
			len(spec.Process.Capabilities.Bounding), len(allCaps.Bounding))
	}

	// MaskedPaths and ReadonlyPaths should be cleared.
	if spec.Linux.MaskedPaths != nil {
		t.Errorf("maskedPaths should be nil for privileged, got %v", spec.Linux.MaskedPaths)
	}

	if spec.Linux.ReadonlyPaths != nil {
		t.Errorf("readonlyPaths should be nil for privileged, got %v", spec.Linux.ReadonlyPaths)
	}
}

// --- Scenario: cap-drop ALL, cap-add NET_BIND_SERVICE ---
// Equivalent: docker run --cap-drop ALL --cap-add NET_BIND_SERVICE alpine sh.
func TestGenerateSpec_CapDropAllAddOne(t *testing.T) {
	t.Parallel()

	spec, err := GenerateSpec(&SpecOpts{
		Config: &container.Config{
			Cmd: []string{"sh"},
		},
		HostConfig: &container.HostConfig{
			CapDrop: []string{"ALL"},
			CapAdd:  []string{"NET_BIND_SERVICE"},
		},
		ImageEnv: []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"},
		RootPath: "/rootfs",
		Hostname: "capdrop123456",
	})
	if err != nil {
		t.Fatalf("GenerateSpec: %v", err)
	}

	// Should have exactly 1 capability.
	if len(spec.Process.Capabilities.Bounding) != 1 {
		t.Fatalf("bounding caps = %v, want [CAP_NET_BIND_SERVICE]",
			spec.Process.Capabilities.Bounding)
	}

	if spec.Process.Capabilities.Bounding[0] != "CAP_NET_BIND_SERVICE" {
		t.Errorf("bounding[0] = %q, want %q",
			spec.Process.Capabilities.Bounding[0], "CAP_NET_BIND_SERVICE")
	}

	// All sets should match.
	assertStringSlice(t, "effective", spec.Process.Capabilities.Effective, []string{"CAP_NET_BIND_SERVICE"})
	assertStringSlice(t, "permitted", spec.Process.Capabilities.Permitted, []string{"CAP_NET_BIND_SERVICE"})
}

// --- Scenario: specific cap-drop ---
// Equivalent: docker run --cap-drop NET_RAW --cap-drop SYS_CHROOT alpine sh.
func TestGenerateSpec_CapDropSpecific(t *testing.T) {
	t.Parallel()

	spec, err := GenerateSpec(&SpecOpts{
		Config: &container.Config{
			Cmd: []string{"sh"},
		},
		HostConfig: &container.HostConfig{
			CapDrop: []string{"NET_RAW", "SYS_CHROOT"},
		},
		ImageEnv: []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"},
		RootPath: "/rootfs",
		Hostname: "capdrop123456",
	})
	if err != nil {
		t.Fatalf("GenerateSpec: %v", err)
	}

	// Should have 12 caps (14 defaults - 2 dropped).
	if len(spec.Process.Capabilities.Bounding) != 12 {
		t.Errorf("bounding caps count = %d, want 12", len(spec.Process.Capabilities.Bounding))
	}

	if containsCap(spec.Process.Capabilities.Bounding, "CAP_NET_RAW") {
		t.Error("CAP_NET_RAW should have been dropped")
	}

	if containsCap(spec.Process.Capabilities.Bounding, "CAP_SYS_CHROOT") {
		t.Error("CAP_SYS_CHROOT should have been dropped")
	}

	// Remaining defaults should still be present.
	if !containsCap(spec.Process.Capabilities.Bounding, "CAP_CHOWN") {
		t.Error("CAP_CHOWN should still be present")
	}
}

// --- Entrypoint/Cmd merge tests ---

func TestBuildArgs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name            string
		configEntry     []string
		configCmd       []string
		imageEntrypoint []string
		imageCmd        []string
		want            []string
	}{
		{
			name:            "image entrypoint only (hello-world)",
			imageEntrypoint: []string{"/hello"},
			want:            []string{"/hello"},
		},
		{
			name:     "image cmd only (alpine default)",
			imageCmd: []string{"/bin/sh"},
			want:     []string{"/bin/sh"},
		},
		{
			name:            "image entrypoint + image cmd (nginx)",
			imageEntrypoint: []string{"/docker-entrypoint.sh"},
			imageCmd:        []string{"nginx", "-g", "daemon off;"},
			want:            []string{"/docker-entrypoint.sh", "nginx", "-g", "daemon off;"},
		},
		{
			name:            "container cmd overrides image cmd",
			imageEntrypoint: []string{"/docker-entrypoint.sh"},
			imageCmd:        []string{"nginx", "-g", "daemon off;"},
			configCmd:       []string{"bash"},
			want:            []string{"/docker-entrypoint.sh", "bash"},
		},
		{
			name:            "container entrypoint overrides image entrypoint",
			configEntry:     []string{"/custom-entry.sh"},
			imageEntrypoint: []string{"/docker-entrypoint.sh"},
			imageCmd:        []string{"nginx"},
			want:            []string{"/custom-entry.sh"},
		},
		{
			name:            "container entrypoint + container cmd",
			configEntry:     []string{"python", "/app/main.py"},
			configCmd:       []string{"--debug"},
			imageEntrypoint: []string{"/bin/sh"},
			imageCmd:        []string{"-c", "echo hello"},
			want:            []string{"python", "/app/main.py", "--debug"},
		},
		{
			name:      "container cmd only, no image entrypoint",
			configCmd: []string{"cat", "/etc/os-release"},
			imageCmd:  []string{"/bin/sh"},
			want:      []string{"cat", "/etc/os-release"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cfg := &container.Config{}
			if tt.configEntry != nil {
				cfg.Entrypoint = tt.configEntry
			}

			if tt.configCmd != nil {
				cfg.Cmd = tt.configCmd
			}

			opts := &SpecOpts{
				Config:          cfg,
				ImageEntrypoint: tt.imageEntrypoint,
				ImageCmd:        tt.imageCmd,
			}

			got := BuildArgs(opts)
			assertStringSlice(t, "args", got, tt.want)
		})
	}
}

// --- Env merge tests ---

func TestBuildEnv(t *testing.T) {
	t.Parallel()

	tests := []struct {
		wantKeys map[string]string
		name     string
		hostname string
		imageEnv []string
		contEnv  []string
	}{
		{
			name:     "image env only",
			imageEnv: []string{"PATH=/usr/bin", "HOME=/root"},
			hostname: "host1",
			wantKeys: map[string]string{
				"PATH":     "/usr/bin",
				"HOME":     "/root",
				"HOSTNAME": "host1",
			},
		},
		{
			name:     "container env overrides image env",
			imageEnv: []string{"PATH=/usr/bin", "FOO=bar"},
			contEnv:  []string{"FOO=baz", "EXTRA=yes"},
			hostname: "host2",
			wantKeys: map[string]string{
				"PATH":     "/usr/bin",
				"FOO":      "baz",
				"EXTRA":    "yes",
				"HOSTNAME": "host2",
			},
		},
		{
			name:     "default PATH when not provided",
			hostname: "host3",
			wantKeys: map[string]string{
				"PATH":     "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
				"HOSTNAME": "host3",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var cfg *container.Config
			if tt.contEnv != nil {
				cfg = &container.Config{Env: tt.contEnv}
			}

			opts := &SpecOpts{
				Config:   cfg,
				ImageEnv: tt.imageEnv,
				Hostname: tt.hostname,
			}

			env := buildEnv(opts)
			envMap := envToMap(env)

			for k, want := range tt.wantKeys {
				if got := envMap[k]; got != want {
					t.Errorf("env[%q] = %q, want %q", k, got, want)
				}
			}
		})
	}
}

// --- User parsing tests ---

func TestParseUser_Numeric(t *testing.T) {
	t.Parallel()

	tests := []struct {
		input   string
		wantUID uint32
		wantGID uint32
	}{
		{input: "0", wantUID: 0, wantGID: 0},
		{input: "1000", wantUID: 1000, wantGID: 0},
		{input: "1000:1000", wantUID: 1000, wantGID: 1000},
		{input: "0:0", wantUID: 0, wantGID: 0},
		{input: "65534:65534", wantUID: 65534, wantGID: 65534},
		{input: "1000:", wantUID: 1000, wantGID: 0}, // trailing colon, empty gid
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			t.Parallel()

			user, err := parseUser(tt.input, "/nonexistent")
			if err != nil {
				t.Fatalf("unexpected error for %q: %v", tt.input, err)
			}

			if user.UID != tt.wantUID {
				t.Errorf("uid = %d, want %d", user.UID, tt.wantUID)
			}

			if user.GID != tt.wantGID {
				t.Errorf("gid = %d, want %d", user.GID, tt.wantGID)
			}
		})
	}
}

// setupFakeRootfs creates a minimal rootfs with /etc/passwd and /etc/group
// matching a typical Alpine-based container image.
func setupFakeRootfs(t *testing.T) string {
	t.Helper()

	rootfs := t.TempDir()
	etcDir := filepath.Join(rootfs, "etc")

	if err := os.MkdirAll(etcDir, 0o755); err != nil {
		t.Fatal(err)
	}

	passwd := `root:x:0:0:root:/root:/bin/ash
bin:x:1:1:bin:/bin:/sbin/nologin
daemon:x:2:2:daemon:/sbin:/sbin/nologin
nobody:x:65534:65534:nobody:/:/sbin/nologin
www-data:x:82:82:Linux User,,,:/var/www:/sbin/nologin
node:x:1000:1000::/home/node:/bin/sh
postgres:x:70:70::/var/lib/postgresql:/bin/sh
`

	if err := os.WriteFile(filepath.Join(etcDir, "passwd"), []byte(passwd), 0o644); err != nil {
		t.Fatal(err)
	}

	group := `root:x:0:root
bin:x:1:root,bin,daemon
daemon:x:2:root,bin,daemon
nogroup:x:65534:
www-data:x:82:
node:x:1000:
postgres:x:70:
docker:x:999:
`

	if err := os.WriteFile(filepath.Join(etcDir, "group"), []byte(group), 0o644); err != nil {
		t.Fatal(err)
	}

	return rootfs
}

func TestParseUser_NameResolution(t *testing.T) {
	t.Parallel()

	rootfs := setupFakeRootfs(t)

	tests := []struct {
		name    string
		input   string
		wantUID uint32
		wantGID uint32
	}{
		{name: "root by name", input: "root", wantUID: 0, wantGID: 0},
		{name: "nobody (uses primary gid)", input: "nobody", wantUID: 65534, wantGID: 65534},
		{name: "www-data", input: "www-data", wantUID: 82, wantGID: 82},
		{name: "node", input: "node", wantUID: 1000, wantGID: 1000},
		{name: "postgres", input: "postgres", wantUID: 70, wantGID: 70},
		{name: "user:group names", input: "nobody:nogroup", wantUID: 65534, wantGID: 65534},
		{name: "user name:numeric gid", input: "node:0", wantUID: 1000, wantGID: 0},
		{name: "numeric uid:group name", input: "1000:docker", wantUID: 1000, wantGID: 999},
		{name: "user:different group", input: "www-data:docker", wantUID: 82, wantGID: 999},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			user, err := parseUser(tt.input, rootfs)
			if err != nil {
				t.Fatalf("parseUser(%q): %v", tt.input, err)
			}

			if user.UID != tt.wantUID {
				t.Errorf("uid = %d, want %d", user.UID, tt.wantUID)
			}

			if user.GID != tt.wantGID {
				t.Errorf("gid = %d, want %d", user.GID, tt.wantGID)
			}
		})
	}
}

func TestParseUser_ResolutionErrors(t *testing.T) {
	t.Parallel()

	rootfs := setupFakeRootfs(t)

	tests := []struct {
		wantErr error
		name    string
		input   string
	}{
		{name: "unknown user", input: "nonexistent", wantErr: ErrUserResolution},
		{name: "unknown group", input: "root:nonexistent", wantErr: ErrGroupResolution},
		{name: "unknown user and group", input: "fake:fake", wantErr: ErrUserResolution},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := parseUser(tt.input, rootfs)
			if err == nil {
				t.Fatalf("expected error for %q", tt.input)
			}

			if !errors.Is(err, tt.wantErr) {
				t.Errorf("expected %v, got %v", tt.wantErr, err)
			}
		})
	}
}

func TestParseUser_NoPasswdFile(t *testing.T) {
	t.Parallel()

	emptyRoot := t.TempDir()

	// Numeric still works without /etc/passwd.
	user, err := parseUser("1000:1000", emptyRoot)
	if err != nil {
		t.Fatalf("numeric user should work without /etc/passwd: %v", err)
	}

	if user.UID != 1000 || user.GID != 1000 {
		t.Errorf("user = %d:%d, want 1000:1000", user.UID, user.GID)
	}

	// Name-based fails gracefully.
	_, err = parseUser("nobody", emptyRoot)
	if !errors.Is(err, ErrUserResolution) {
		t.Errorf("expected ErrUserResolution, got %v", err)
	}
}

// --- Resource limits tests ---

func TestBuildResources_MemoryOnly(t *testing.T) {
	t.Parallel()

	memLimit := int64(128 * 1024 * 1024) // 128 MiB
	memSwap := int64(256 * 1024 * 1024)  // 256 MiB
	oomDisable := true

	spec, err := GenerateSpec(&SpecOpts{
		Config: &container.Config{Cmd: []string{"sh"}},
		HostConfig: &container.HostConfig{
			Resources: container.Resources{
				Memory:         memLimit,
				MemorySwap:     memSwap,
				OomKillDisable: &oomDisable,
			},
		},
		ImageEnv: []string{"PATH=/bin"},
		RootPath: "/rootfs",
		Hostname: "mem123456789a",
	})
	if err != nil {
		t.Fatalf("GenerateSpec: %v", err)
	}

	mem := spec.Linux.Resources.Memory
	if mem == nil {
		t.Fatal("memory resources should be set")
	}

	if *mem.Limit != memLimit {
		t.Errorf("memory.limit = %d, want %d", *mem.Limit, memLimit)
	}

	if *mem.Swap != memSwap {
		t.Errorf("memory.swap = %d, want %d", *mem.Swap, memSwap)
	}

	if mem.DisableOOMKiller == nil || !*mem.DisableOOMKiller {
		t.Error("disableOOMKiller should be true")
	}
}

func TestBuildResources_CPUShares(t *testing.T) {
	t.Parallel()

	spec, err := GenerateSpec(&SpecOpts{
		Config: &container.Config{Cmd: []string{"sh"}},
		HostConfig: &container.HostConfig{
			Resources: container.Resources{
				CPUShares:  512,
				CpusetCpus: "0-3",
				CpusetMems: "0",
			},
		},
		ImageEnv: []string{"PATH=/bin"},
		RootPath: "/rootfs",
		Hostname: "cpu123456789a",
	})
	if err != nil {
		t.Fatalf("GenerateSpec: %v", err)
	}

	cpu := spec.Linux.Resources.CPU
	if cpu == nil {
		t.Fatal("CPU resources should be set")
	}

	if cpu.Shares == nil || *cpu.Shares != 512 {
		t.Errorf("cpu.shares = %v, want 512", cpu.Shares)
	}

	if cpu.Cpus != "0-3" {
		t.Errorf("cpu.cpus = %q, want %q", cpu.Cpus, "0-3")
	}

	if cpu.Mems != "0" {
		t.Errorf("cpu.mems = %q, want %q", cpu.Mems, "0")
	}
}

func TestBuildResources_CPUQuotaPeriod(t *testing.T) {
	t.Parallel()

	spec, err := GenerateSpec(&SpecOpts{
		Config: &container.Config{Cmd: []string{"sh"}},
		HostConfig: &container.HostConfig{
			Resources: container.Resources{
				CPUQuota:  50000,
				CPUPeriod: 100000,
			},
		},
		ImageEnv: []string{"PATH=/bin"},
		RootPath: "/rootfs",
		Hostname: "quota1234567a",
	})
	if err != nil {
		t.Fatalf("GenerateSpec: %v", err)
	}

	cpu := spec.Linux.Resources.CPU
	if cpu == nil {
		t.Fatal("CPU resources should be set")
	}

	if cpu.Quota == nil || *cpu.Quota != 50000 {
		t.Errorf("cpu.quota = %v, want 50000", cpu.Quota)
	}

	if cpu.Period == nil || *cpu.Period != 100000 {
		t.Errorf("cpu.period = %v, want 100000", cpu.Period)
	}
}

func TestBuildResources_NoLimits(t *testing.T) {
	t.Parallel()

	spec, err := GenerateSpec(&SpecOpts{
		Config:     &container.Config{Cmd: []string{"sh"}},
		HostConfig: &container.HostConfig{},
		ImageEnv:   []string{"PATH=/bin"},
		RootPath:   "/rootfs",
		Hostname:   "nolimit12345a",
	})
	if err != nil {
		t.Fatalf("GenerateSpec: %v", err)
	}

	if spec.Linux.Resources != nil {
		t.Errorf("resources should be nil when no limits are set, got %+v", spec.Linux.Resources)
	}
}

// --- Ulimits test ---

func TestBuildRlimits(t *testing.T) {
	t.Parallel()

	spec, err := GenerateSpec(&SpecOpts{
		Config: &container.Config{Cmd: []string{"sh"}},
		HostConfig: &container.HostConfig{
			Resources: container.Resources{
				Ulimits: []*container.Ulimit{
					{Name: "nofile", Soft: 1024, Hard: 2048},
					{Name: "nproc", Soft: 512, Hard: 1024},
				},
			},
		},
		ImageEnv: []string{"PATH=/bin"},
		RootPath: "/rootfs",
		Hostname: "ulimit1234567",
	})
	if err != nil {
		t.Fatalf("GenerateSpec: %v", err)
	}

	if len(spec.Process.Rlimits) != 2 {
		t.Fatalf("rlimits count = %d, want 2", len(spec.Process.Rlimits))
	}

	rlimitMap := make(map[string]ocispec.POSIXRlimit)
	for _, r := range spec.Process.Rlimits {
		rlimitMap[r.Type] = r
	}

	nofile := rlimitMap["RLIMIT_NOFILE"]
	if nofile.Soft != 1024 || nofile.Hard != 2048 {
		t.Errorf("RLIMIT_NOFILE = %d/%d, want 1024/2048", nofile.Soft, nofile.Hard)
	}

	nproc := rlimitMap["RLIMIT_NPROC"]
	if nproc.Soft != 512 || nproc.Hard != 1024 {
		t.Errorf("RLIMIT_NPROC = %d/%d, want 512/1024", nproc.Soft, nproc.Hard)
	}
}

// --- Working directory tests ---

func TestBuildCwd(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		configCwd string
		imageCwd  string
		want      string
	}{
		{name: "defaults to /", want: "/"},
		{name: "image cwd", imageCwd: "/app", want: "/app"},
		{name: "container overrides image", configCwd: "/workspace", imageCwd: "/app", want: "/workspace"},
		{name: "container cwd without image", configCwd: "/data", want: "/data"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var cfg *container.Config
			if tt.configCwd != "" {
				cfg = &container.Config{WorkingDir: tt.configCwd}
			}

			opts := &SpecOpts{Config: cfg, ImageCwd: tt.imageCwd}
			got := buildCwd(opts)

			if got != tt.want {
				t.Errorf("cwd = %q, want %q", got, tt.want)
			}
		})
	}
}

// --- Error cases ---

func TestGenerateSpec_NoCommand(t *testing.T) {
	t.Parallel()

	_, err := GenerateSpec(&SpecOpts{
		Config:   &container.Config{},
		RootPath: "/rootfs",
		Hostname: "err123456789a",
	})

	if !errors.Is(err, ErrNoCommand) {
		t.Errorf("expected ErrNoCommand, got %v", err)
	}
}

func TestGenerateSpec_UnknownUser(t *testing.T) {
	t.Parallel()

	_, err := GenerateSpec(&SpecOpts{
		Config: &container.Config{
			Cmd:  []string{"sh"},
			User: "nonexistent_user",
		},
		HostConfig: &container.HostConfig{},
		ImageEnv:   []string{"PATH=/bin"},
		RootPath:   t.TempDir(), // empty rootfs, no /etc/passwd
		Hostname:   "userr12345678",
	})

	if !errors.Is(err, ErrUserResolution) {
		t.Errorf("expected ErrUserResolution, got %v", err)
	}
}

// Full integration: docker run --user www-data nginx.
func TestGenerateSpec_ImageUserByName(t *testing.T) {
	t.Parallel()

	rootfs := setupFakeRootfs(t)

	spec, err := GenerateSpec(&SpecOpts{
		Config:     &container.Config{Cmd: []string{"sh"}},
		HostConfig: &container.HostConfig{},
		ImageUser:  "www-data",
		ImageEnv:   []string{"PATH=/bin"},
		RootPath:   rootfs,
		Hostname:   "wwwdata123456",
	})
	if err != nil {
		t.Fatalf("GenerateSpec: %v", err)
	}

	if spec.Process.User.UID != 82 {
		t.Errorf("uid = %d, want 82 (www-data)", spec.Process.User.UID)
	}

	if spec.Process.User.GID != 82 {
		t.Errorf("gid = %d, want 82 (www-data primary group)", spec.Process.User.GID)
	}
}

// docker run --user postgres:docker myimage.
func TestGenerateSpec_ContainerUserNameGroupName(t *testing.T) {
	t.Parallel()

	rootfs := setupFakeRootfs(t)

	spec, err := GenerateSpec(&SpecOpts{
		Config: &container.Config{
			Cmd:  []string{"pg_start"},
			User: "postgres:docker",
		},
		HostConfig: &container.HostConfig{},
		ImageEnv:   []string{"PATH=/bin"},
		RootPath:   rootfs,
		Hostname:   "pgdocker12345",
	})
	if err != nil {
		t.Fatalf("GenerateSpec: %v", err)
	}

	if spec.Process.User.UID != 70 {
		t.Errorf("uid = %d, want 70 (postgres)", spec.Process.User.UID)
	}

	if spec.Process.User.GID != 999 {
		t.Errorf("gid = %d, want 999 (docker)", spec.Process.User.GID)
	}
}

// --- Complex real-world scenario: Python app with multiple config options ---
// Equivalent: docker run --memory=256m --memory-reservation=128m --cpus=2
//
//	--user 1001:1001 --cap-drop NET_RAW --ulimit nofile=65536:65536
//	-e APP_ENV=production -w /app myapp:latest serve --port 8080
func TestGenerateSpec_ComplexPythonApp(t *testing.T) {
	t.Parallel()

	memLimit := int64(256 * 1024 * 1024)
	memReserv := int64(128 * 1024 * 1024)

	spec, err := GenerateSpec(&SpecOpts{
		Config: &container.Config{
			Cmd:        []string{"serve", "--port", "8080"},
			User:       "1001:1001",
			WorkingDir: "/app",
			Env:        []string{"APP_ENV=production"},
		},
		HostConfig: &container.HostConfig{
			CapDrop: []string{"NET_RAW"},
			Resources: container.Resources{
				Memory:            memLimit,
				MemoryReservation: memReserv,
				NanoCPUs:          2000000000, // 2 CPUs
				Ulimits: []*container.Ulimit{
					{Name: "nofile", Soft: 65536, Hard: 65536},
				},
			},
		},
		ImageEntrypoint: []string{"python", "-m", "myapp"},
		ImageEnv: []string{
			"PATH=/usr/local/bin:/usr/bin:/bin",
			"PYTHONUNBUFFERED=1",
			"APP_ENV=development", // overridden by container env
		},
		ImageCwd: "/opt/app",
		RootPath: "/rootfs",
		Hostname: "python123456ab",
	})
	if err != nil {
		t.Fatalf("GenerateSpec: %v", err)
	}

	// Entrypoint from image + Cmd from container.
	assertStringSlice(t, "args", spec.Process.Args, []string{
		"python", "-m", "myapp", "serve", "--port", "8080",
	})

	// User.
	if spec.Process.User.UID != 1001 || spec.Process.User.GID != 1001 {
		t.Errorf("user = %d:%d, want 1001:1001", spec.Process.User.UID, spec.Process.User.GID)
	}

	// Container WorkingDir overrides image.
	if spec.Process.Cwd != "/app" {
		t.Errorf("cwd = %q, want /app", spec.Process.Cwd)
	}

	// Env: APP_ENV should be "production" (container override).
	envMap := envToMap(spec.Process.Env)
	if envMap["APP_ENV"] != "production" {
		t.Errorf("APP_ENV = %q, want %q", envMap["APP_ENV"], "production")
	}

	if envMap["PYTHONUNBUFFERED"] != "1" {
		t.Errorf("PYTHONUNBUFFERED = %q, want %q", envMap["PYTHONUNBUFFERED"], "1")
	}

	// CAP_NET_RAW should be dropped.
	if containsCap(spec.Process.Capabilities.Bounding, "CAP_NET_RAW") {
		t.Error("CAP_NET_RAW should have been dropped")
	}

	// Other defaults should remain.
	if !containsCap(spec.Process.Capabilities.Bounding, "CAP_CHOWN") {
		t.Error("CAP_CHOWN should still be present")
	}

	// Memory limits.
	if *spec.Linux.Resources.Memory.Limit != memLimit {
		t.Errorf("memory.limit = %d, want %d", *spec.Linux.Resources.Memory.Limit, memLimit)
	}

	if *spec.Linux.Resources.Memory.Reservation != memReserv {
		t.Errorf("memory.reservation = %d, want %d", *spec.Linux.Resources.Memory.Reservation, memReserv)
	}

	// NanoCPUs: 2 CPUs = 200000 quota / 100000 period.
	cpu := spec.Linux.Resources.CPU
	if cpu.Quota == nil || *cpu.Quota != 200000 {
		t.Errorf("cpu.quota = %d, want 200000 (2000000000 * 100000 / 1e9)", *cpu.Quota)
	}

	// Rlimits.
	if len(spec.Process.Rlimits) != 1 {
		t.Fatalf("rlimits = %d, want 1", len(spec.Process.Rlimits))
	}

	if spec.Process.Rlimits[0].Type != "RLIMIT_NOFILE" {
		t.Errorf("rlimit type = %q, want RLIMIT_NOFILE", spec.Process.Rlimits[0].Type)
	}

	if spec.Process.Rlimits[0].Soft != 65536 || spec.Process.Rlimits[0].Hard != 65536 {
		t.Errorf("nofile = %d/%d, want 65536/65536",
			spec.Process.Rlimits[0].Soft, spec.Process.Rlimits[0].Hard)
	}
}

// --- User from image config ---

func TestGenerateSpec_ImageUser(t *testing.T) {
	t.Parallel()

	spec, err := GenerateSpec(&SpecOpts{
		Config:     &container.Config{Cmd: []string{"sh"}},
		HostConfig: &container.HostConfig{},
		ImageUser:  "65534:65534", // nobody:nogroup
		ImageEnv:   []string{"PATH=/bin"},
		RootPath:   "/rootfs",
		Hostname:   "imguser123456",
	})
	if err != nil {
		t.Fatalf("GenerateSpec: %v", err)
	}

	if spec.Process.User.UID != 65534 || spec.Process.User.GID != 65534 {
		t.Errorf("user = %d:%d, want 65534:65534", spec.Process.User.UID, spec.Process.User.GID)
	}
}

func TestGenerateSpec_ContainerUserOverridesImage(t *testing.T) {
	t.Parallel()

	spec, err := GenerateSpec(&SpecOpts{
		Config: &container.Config{
			Cmd:  []string{"sh"},
			User: "1000:1000",
		},
		HostConfig: &container.HostConfig{},
		ImageUser:  "65534:65534",
		ImageEnv:   []string{"PATH=/bin"},
		RootPath:   "/rootfs",
		Hostname:   "override123456",
	})
	if err != nil {
		t.Fatalf("GenerateSpec: %v", err)
	}

	// Container user should win.
	if spec.Process.User.UID != 1000 || spec.Process.User.GID != 1000 {
		t.Errorf("user = %d:%d, want 1000:1000", spec.Process.User.UID, spec.Process.User.GID)
	}
}

// --- Default mounts verification ---

func TestDefaultMounts(t *testing.T) {
	t.Parallel()

	mounts := defaultMounts()

	expected := map[string]string{
		"/proc":          "proc",
		"/dev":           "tmpfs",
		"/dev/pts":       "devpts",
		"/dev/shm":       "tmpfs",
		"/dev/mqueue":    "mqueue",
		"/sys":           "sysfs",
		"/sys/fs/cgroup": "cgroup",
	}

	if len(mounts) != len(expected) {
		t.Errorf("mounts count = %d, want %d", len(mounts), len(expected))
	}

	for _, m := range mounts {
		wantType, ok := expected[m.Destination]
		if !ok {
			t.Errorf("unexpected mount: %s", m.Destination)

			continue
		}

		if m.Type != wantType {
			t.Errorf("mount %s type = %q, want %q", m.Destination, m.Type, wantType)
		}
	}

	// /sys should be read-only.
	for _, m := range mounts {
		if m.Destination == "/sys" {
			found := false

			for _, opt := range m.Options {
				if opt == "ro" {
					found = true
				}
			}

			if !found {
				t.Error("/sys should have 'ro' option")
			}
		}
	}
}

// --- Nil config safety ---

func TestGenerateSpec_NilHostConfig(t *testing.T) {
	t.Parallel()

	spec, err := GenerateSpec(&SpecOpts{
		Config:   &container.Config{Cmd: []string{"sh"}},
		ImageEnv: []string{"PATH=/bin"},
		RootPath: "/rootfs",
		Hostname: "nilhost123456",
	})
	if err != nil {
		t.Fatalf("GenerateSpec: %v", err)
	}

	if spec.Root.Readonly {
		t.Error("root.readonly should be false with nil HostConfig")
	}

	if spec.Linux.Resources != nil {
		t.Error("resources should be nil with nil HostConfig")
	}
}

// --- Test helpers ---

func assertStringSlice(t *testing.T, name string, got, want []string) {
	t.Helper()

	if len(got) != len(want) {
		t.Errorf("%s length = %d, want %d\n  got:  %v\n  want: %v", name, len(got), len(want), got, want)

		return
	}

	for i := range got {
		if got[i] != want[i] {
			t.Errorf("%s[%d] = %q, want %q", name, i, got[i], want[i])
		}
	}
}

func assertCapsContain(t *testing.T, name string, caps, required []string) {
	t.Helper()

	for _, req := range required {
		if !containsCap(caps, req) {
			t.Errorf("%s missing required capability %s", name, req)
		}
	}
}

func assertNamespaces(t *testing.T, ns []ocispec.LinuxNamespace, wantTypes []ocispec.LinuxNamespaceType) {
	t.Helper()

	gotTypes := make([]string, len(ns))
	for i, n := range ns {
		gotTypes[i] = string(n.Type)
	}

	wantStrs := make([]string, len(wantTypes))
	for i, w := range wantTypes {
		wantStrs[i] = string(w)
	}

	sort.Strings(gotTypes)
	sort.Strings(wantStrs)
	assertStringSlice(t, "namespaces", gotTypes, wantStrs)
}

func assertMountDests(t *testing.T, mounts []ocispec.Mount, dests []string) {
	t.Helper()

	got := make([]string, len(mounts))
	for i, m := range mounts {
		got[i] = m.Destination
	}

	sort.Strings(got)

	want := make([]string, len(dests))
	copy(want, dests)
	sort.Strings(want)

	assertStringSlice(t, "mount destinations", got, want)
}

func TestGenerateSpec_TTY(t *testing.T) {
	t.Parallel()

	rootfs := setupFakeRootfs(t)

	spec, err := GenerateSpec(&SpecOpts{
		Config: &container.Config{
			Image: "alpine",
			Cmd:   []string{"sh"},
			Tty:   true,
		},
		HostConfig: &container.HostConfig{},
		RootPath:   rootfs,
		Hostname:   "test12345678",
	})
	if err != nil {
		t.Fatal(err)
	}

	if !spec.Process.Terminal {
		t.Error("terminal should be true when Config.Tty=true")
	}
}

func TestGenerateSpec_NoTTY_Explicit(t *testing.T) {
	t.Parallel()

	rootfs := setupFakeRootfs(t)

	spec, err := GenerateSpec(&SpecOpts{
		Config: &container.Config{
			Image: "alpine",
			Cmd:   []string{"echo", "hi"},
			Tty:   false,
		},
		HostConfig: &container.HostConfig{},
		RootPath:   rootfs,
		Hostname:   "test12345678",
	})
	if err != nil {
		t.Fatal(err)
	}

	if spec.Process.Terminal {
		t.Error("terminal should be false when Config.Tty=false")
	}
}

func envToMap(env []string) map[string]string {
	m := make(map[string]string)

	for _, e := range env {
		k, v, _ := strings.Cut(e, "=")
		m[k] = v
	}

	return m
}

// --- Network namespace mode tests ---

func TestGenerateSpec_HostNetworkMode(t *testing.T) {
	t.Parallel()

	spec, err := GenerateSpec(&SpecOpts{
		Config:          &container.Config{},
		HostConfig:      &container.HostConfig{},
		ImageEntrypoint: []string{"/bin/sh"},
		RootPath:        "/rootfs",
		Hostname:        "test",
		NetworkMode:     "host",
	})
	if err != nil {
		t.Fatalf("GenerateSpec: %v", err)
	}

	// Host mode: should NOT have a network namespace.
	for _, ns := range spec.Linux.Namespaces {
		if ns.Type == ocispec.NetworkNamespace {
			t.Error("host mode should not have a network namespace")
		}
	}

	// Should still have pid, mount, ipc, uts.
	assertNamespaces(t, spec.Linux.Namespaces, []ocispec.LinuxNamespaceType{
		ocispec.PIDNamespace, ocispec.MountNamespace, ocispec.IPCNamespace, ocispec.UTSNamespace,
	})
}

func TestGenerateSpec_NoneNetworkMode(t *testing.T) {
	t.Parallel()

	spec, err := GenerateSpec(&SpecOpts{
		Config:          &container.Config{},
		HostConfig:      &container.HostConfig{},
		ImageEntrypoint: []string{"/bin/sh"},
		RootPath:        "/rootfs",
		Hostname:        "test",
		NetworkMode:     "none",
	})
	if err != nil {
		t.Fatalf("GenerateSpec: %v", err)
	}

	// None mode: should have a network namespace with empty path (crun creates new one).
	assertNamespaces(t, spec.Linux.Namespaces, []ocispec.LinuxNamespaceType{
		ocispec.PIDNamespace, ocispec.MountNamespace, ocispec.IPCNamespace, ocispec.UTSNamespace, ocispec.NetworkNamespace,
	})

	for _, ns := range spec.Linux.Namespaces {
		if ns.Type == ocispec.NetworkNamespace && ns.Path != "" {
			t.Errorf("none mode network namespace should have empty path, got %q", ns.Path)
		}
	}
}

func TestGenerateSpec_ContainerNetworkMode(t *testing.T) {
	t.Parallel()

	nsPath := "/proc/12345/ns/net"

	spec, err := GenerateSpec(&SpecOpts{
		Config:          &container.Config{},
		HostConfig:      &container.HostConfig{},
		ImageEntrypoint: []string{"/bin/sh"},
		RootPath:        "/rootfs",
		Hostname:        "test",
		NetworkMode:     "container:abc123",
		NetworkNSPath:   nsPath,
	})
	if err != nil {
		t.Fatalf("GenerateSpec: %v", err)
	}

	// container: mode should have a network namespace with the target's ns path.
	var found bool

	for _, ns := range spec.Linux.Namespaces {
		if ns.Type == ocispec.NetworkNamespace {
			found = true

			if ns.Path != nsPath {
				t.Errorf("network namespace path = %q, want %q", ns.Path, nsPath)
			}
		}
	}

	if !found {
		t.Error("container mode should have a network namespace")
	}
}

func TestGenerateSpec_ExtraBindMounts(t *testing.T) {
	t.Parallel()

	extraMounts := []ocispec.Mount{
		{
			Destination: "/etc/hostname",
			Type:        "bind",
			Source:      "/tmp/hostname",
			Options:     []string{"rbind", "rprivate", "rw"},
		},
		{
			Destination: "/etc/hosts",
			Type:        "bind",
			Source:      "/tmp/hosts",
			Options:     []string{"rbind", "rprivate", "rw"},
		},
	}

	spec, err := GenerateSpec(&SpecOpts{
		Config:          &container.Config{},
		HostConfig:      &container.HostConfig{},
		ImageEntrypoint: []string{"/bin/sh"},
		RootPath:        "/rootfs",
		Hostname:        "test",
		ExtraBindMounts: extraMounts,
	})
	if err != nil {
		t.Fatalf("GenerateSpec: %v", err)
	}

	// Verify extra mounts are present.
	mountDests := make(map[string]bool)
	for _, m := range spec.Mounts {
		mountDests[m.Destination] = true
	}

	if !mountDests["/etc/hostname"] {
		t.Error("missing /etc/hostname bind mount")
	}

	if !mountDests["/etc/hosts"] {
		t.Error("missing /etc/hosts bind mount")
	}
}
