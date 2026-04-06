package runtime

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/moby/moby/api/types/container"
	ocispec "github.com/opencontainers/runtime-spec/specs-go"
)

var (
	// ErrNoCommand is returned when no command is specified for the container.
	ErrNoCommand = errors.New("spec: no command specified")
	// ErrUserResolution is returned when a username cannot be resolved from /etc/passwd.
	ErrUserResolution = errors.New("spec: user resolution failed")
	// ErrGroupResolution is returned when a group name cannot be resolved from /etc/group.
	ErrGroupResolution = errors.New("spec: group resolution failed")
	// ErrUserNotFound is returned when a user is not found in /etc/passwd.
	ErrUserNotFound = errors.New("user not found in /etc/passwd")
	// ErrGroupNotFound is returned when a group is not found in /etc/group.
	ErrGroupNotFound = errors.New("group not found in /etc/group")
	// ErrInvalidPasswd is returned when /etc/passwd contains an invalid entry.
	ErrInvalidPasswd = errors.New("invalid entry in /etc/passwd")
	// ErrInvalidGroup is returned when /etc/group contains an invalid entry.
	ErrInvalidGroup = errors.New("invalid entry in /etc/group")
)

// SpecOpts contains the inputs for OCI spec generation.
type SpecOpts struct {
	Config          *container.Config
	HostConfig      *container.HostConfig
	ImageCwd        string
	ImageUser       string
	RootPath        string
	Hostname        string
	ImageEnv        []string
	ImageEntrypoint []string
	ImageCmd        []string
}

// defaultCaps are the Docker default Linux capabilities.
var defaultCaps = []string{
	"CAP_CHOWN",
	"CAP_DAC_OVERRIDE",
	"CAP_FOWNER",
	"CAP_FSETID",
	"CAP_KILL",
	"CAP_SETGID",
	"CAP_SETUID",
	"CAP_SETPCAP",
	"CAP_NET_BIND_SERVICE",
	"CAP_NET_RAW",
	"CAP_SYS_CHROOT",
	"CAP_MKNOD",
	"CAP_AUDIT_WRITE",
	"CAP_SETFCAP",
}

// defaultMaskedPaths are paths masked in the container (Docker defaults).
var defaultMaskedPaths = []string{
	"/proc/asound",
	"/proc/acpi",
	"/proc/kcore",
	"/proc/keys",
	"/proc/latency_stats",
	"/proc/timer_list",
	"/proc/timer_stats",
	"/proc/sched_debug",
	"/proc/scsi",
	"/sys/firmware",
	"/sys/devices/virtual/powercap",
}

// defaultReadonlyPaths are paths mounted read-only (Docker defaults).
var defaultReadonlyPaths = []string{
	"/proc/bus",
	"/proc/fs",
	"/proc/irq",
	"/proc/sys",
	"/proc/sysrq-trigger",
}

// GenerateSpec creates an OCI runtime spec from Docker container configuration.
func GenerateSpec(opts *SpecOpts) (*ocispec.Spec, error) {
	args := BuildArgs(opts)
	if len(args) == 0 {
		return nil, ErrNoCommand
	}

	env := buildEnv(opts)
	cwd := buildCwd(opts)

	user, err := buildUser(opts)
	if err != nil {
		return nil, err
	}

	caps := buildCapabilities(opts)
	rlimits := buildRlimits(opts)

	spec := &ocispec.Spec{
		Version:  "1.2.0",
		Hostname: opts.Hostname,
		Process: &ocispec.Process{
			Terminal:        opts.Config != nil && opts.Config.Tty,
			User:            user,
			Args:            args,
			Env:             env,
			Cwd:             cwd,
			Capabilities:    caps,
			Rlimits:         rlimits,
			NoNewPrivileges: !isPrivileged(opts),
		},
		Root: &ocispec.Root{
			Path:     opts.RootPath,
			Readonly: opts.HostConfig != nil && opts.HostConfig.ReadonlyRootfs,
		},
		Mounts: defaultMounts(),
		Linux: &ocispec.Linux{
			Namespaces:    defaultNamespaces(),
			MaskedPaths:   defaultMaskedPaths,
			ReadonlyPaths: defaultReadonlyPaths,
			Resources:     buildResources(opts),
			CgroupsPath:   "kogia/" + opts.Hostname,
		},
	}

	if isPrivileged(opts) {
		spec.Linux.MaskedPaths = nil
		spec.Linux.ReadonlyPaths = nil
	}

	return spec, nil
}

// BuildArgs merges entrypoint and cmd from container config and image config
// following Docker's precedence rules. Exported so container.go can reuse it.
func BuildArgs(opts *SpecOpts) []string {
	// Docker merges entrypoint + cmd. Container config overrides image config.
	var (
		entrypoint []string
		cmd        []string
	)

	if opts.Config != nil && len(opts.Config.Entrypoint) > 0 {
		entrypoint = opts.Config.Entrypoint
	} else {
		entrypoint = opts.ImageEntrypoint
	}

	if opts.Config != nil && len(opts.Config.Cmd) > 0 {
		cmd = opts.Config.Cmd
	} else if opts.Config == nil || len(opts.Config.Entrypoint) == 0 {
		// Only use image cmd if entrypoint wasn't overridden.
		cmd = opts.ImageCmd
	}

	return append(entrypoint, cmd...)
}

func buildEnv(opts *SpecOpts) []string {
	// Start with image env, then overlay container env.
	envMap := make(map[string]string)

	for _, e := range opts.ImageEnv {
		k, v, _ := strings.Cut(e, "=")
		envMap[k] = v
	}

	if opts.Config != nil {
		for _, e := range opts.Config.Env {
			k, v, _ := strings.Cut(e, "=")
			envMap[k] = v
		}
	}

	// Ensure PATH exists.
	if _, ok := envMap["PATH"]; !ok {
		envMap["PATH"] = DefaultPathEnv
	}

	// Add HOSTNAME.
	envMap["HOSTNAME"] = opts.Hostname

	env := make([]string, 0, len(envMap))
	for k, v := range envMap {
		env = append(env, k+"="+v)
	}

	return env
}

func buildCwd(opts *SpecOpts) string {
	if opts.Config != nil && opts.Config.WorkingDir != "" {
		return opts.Config.WorkingDir
	}

	if opts.ImageCwd != "" {
		return opts.ImageCwd
	}

	return "/"
}

func buildUser(opts *SpecOpts) (ocispec.User, error) {
	userStr := ""
	if opts.Config != nil && opts.Config.User != "" {
		userStr = opts.Config.User
	} else if opts.ImageUser != "" {
		userStr = opts.ImageUser
	}

	if userStr == "" {
		return ocispec.User{UID: 0, GID: 0}, nil
	}

	return parseUser(userStr, opts.RootPath)
}

// parseUser parses user specs in all Docker-supported formats:
// "uid", "uid:gid", "username", "username:group", "uid:group", "username:gid".
// String usernames/groups are resolved by reading /etc/passwd and /etc/group in the rootfs.
func parseUser(userStr, rootPath string) (ocispec.User, error) {
	uidStr, gidStr, hasGID := strings.Cut(userStr, ":")

	uid, uidErr := strconv.ParseUint(uidStr, 10, 32)
	if uidErr != nil {
		// Try resolving username from /etc/passwd in rootfs.
		resolved, resolveErr := lookupUser(rootPath, uidStr)
		if resolveErr != nil {
			return ocispec.User{}, fmt.Errorf("%w: %s: %w", ErrUserResolution, uidStr, resolveErr)
		}

		uid = uint64(resolved.uid)

		// If no explicit group was requested, use the user's primary group from passwd.
		if !hasGID || gidStr == "" {
			return ocispec.User{UID: uint32(uid), GID: resolved.gid}, nil //nolint:gosec // uid was set from resolved.uid (uint32).
		}
	}

	user := ocispec.User{UID: uint32(uid)}

	if hasGID && gidStr != "" {
		gid, gidErr := strconv.ParseUint(gidStr, 10, 32)
		if gidErr != nil {
			// Try resolving group name from /etc/group in rootfs.
			resolvedGID, resolveErr := lookupGroup(rootPath, gidStr)
			if resolveErr != nil {
				return ocispec.User{}, fmt.Errorf("%w: %s: %w", ErrGroupResolution, gidStr, resolveErr)
			}

			gid = uint64(resolvedGID)
		}

		user.GID = uint32(gid)
	}

	return user, nil
}

type passwdEntry struct {
	uid uint32
	gid uint32
}

// lookupUser resolves a username to uid/gid by reading /etc/passwd in the rootfs.
// Format: name:password:uid:gid:gecos:home:shell.
func lookupUser(rootPath, username string) (*passwdEntry, error) {
	passwdPath := filepath.Join(rootPath, "etc", "passwd")

	data, err := os.ReadFile(passwdPath) //nolint:gosec // Reading from container rootfs is intentional.
	if err != nil {
		return nil, fmt.Errorf("read /etc/passwd: %w", err)
	}

	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		fields := strings.SplitN(line, ":", 7)
		if len(fields) < 4 {
			continue
		}

		if fields[0] != username {
			continue
		}

		uid, uidErr := strconv.ParseUint(fields[2], 10, 32)
		if uidErr != nil {
			return nil, fmt.Errorf("%w: uid %q for user %s", ErrInvalidPasswd, fields[2], username)
		}

		gid, gidErr := strconv.ParseUint(fields[3], 10, 32)
		if gidErr != nil {
			return nil, fmt.Errorf("%w: gid %q for user %s", ErrInvalidPasswd, fields[3], username)
		}

		return &passwdEntry{uid: uint32(uid), gid: uint32(gid)}, nil
	}

	return nil, fmt.Errorf("%w: %s", ErrUserNotFound, username)
}

// lookupGroup resolves a group name to gid by reading /etc/group in the rootfs.
// Format: name:password:gid:members.
func lookupGroup(rootPath, groupname string) (uint32, error) {
	groupPath := filepath.Join(rootPath, "etc", "group")

	data, err := os.ReadFile(groupPath) //nolint:gosec // Reading from container rootfs is intentional.
	if err != nil {
		return 0, fmt.Errorf("read /etc/group: %w", err)
	}

	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		fields := strings.SplitN(line, ":", 4)
		if len(fields) < 3 {
			continue
		}

		if fields[0] != groupname {
			continue
		}

		gid, gidErr := strconv.ParseUint(fields[2], 10, 32)
		if gidErr != nil {
			return 0, fmt.Errorf("%w: gid %q for group %s", ErrInvalidGroup, fields[2], groupname)
		}

		return uint32(gid), nil
	}

	return 0, fmt.Errorf("%w: %s", ErrGroupNotFound, groupname)
}

func buildCapabilities(opts *SpecOpts) *ocispec.LinuxCapabilities {
	if isPrivileged(opts) {
		return allCapabilities()
	}

	caps := make([]string, 0, len(defaultCaps)+5)
	caps = append(caps, defaultCaps...)

	if opts.HostConfig != nil {
		for _, drop := range opts.HostConfig.CapDrop {
			if strings.EqualFold(drop, "ALL") {
				caps = nil

				break
			}

			caps = removeCap(caps, "CAP_"+strings.ToUpper(drop))
		}

		for _, add := range opts.HostConfig.CapAdd {
			capName := "CAP_" + strings.ToUpper(add)
			if !containsCap(caps, capName) {
				caps = append(caps, capName)
			}
		}
	}

	return &ocispec.LinuxCapabilities{
		Bounding:    caps,
		Effective:   caps,
		Permitted:   caps,
		Inheritable: caps,
		Ambient:     caps,
	}
}

func allCapabilities() *ocispec.LinuxCapabilities {
	// All capabilities known to Docker.
	all := []string{
		"CAP_AUDIT_CONTROL", "CAP_AUDIT_READ", "CAP_AUDIT_WRITE",
		"CAP_BLOCK_SUSPEND", "CAP_BPF",
		"CAP_CHECKPOINT_RESTORE", "CAP_CHOWN",
		"CAP_DAC_OVERRIDE", "CAP_DAC_READ_SEARCH",
		"CAP_FOWNER", "CAP_FSETID",
		"CAP_IPC_LOCK", "CAP_IPC_OWNER",
		"CAP_KILL", "CAP_LEASE", "CAP_LINUX_IMMUTABLE",
		"CAP_MAC_ADMIN", "CAP_MAC_OVERRIDE",
		"CAP_MKNOD",
		"CAP_NET_ADMIN", "CAP_NET_BIND_SERVICE", "CAP_NET_BROADCAST", "CAP_NET_RAW",
		"CAP_PERFMON",
		"CAP_SETFCAP", "CAP_SETGID", "CAP_SETPCAP", "CAP_SETUID",
		"CAP_SYS_ADMIN", "CAP_SYS_BOOT", "CAP_SYS_CHROOT",
		"CAP_SYS_MODULE", "CAP_SYS_NICE", "CAP_SYS_PACCT",
		"CAP_SYS_PTRACE", "CAP_SYS_RAWIO", "CAP_SYS_RESOURCE",
		"CAP_SYS_TIME", "CAP_SYS_TTY_CONFIG",
		"CAP_SYSLOG", "CAP_WAKE_ALARM",
	}

	return &ocispec.LinuxCapabilities{
		Bounding:    all,
		Effective:   all,
		Permitted:   all,
		Inheritable: all,
		Ambient:     all,
	}
}

func removeCap(caps []string, target string) []string {
	result := make([]string, 0, len(caps))
	for _, c := range caps {
		if !strings.EqualFold(c, target) {
			result = append(result, c)
		}
	}

	return result
}

func containsCap(caps []string, target string) bool {
	for _, c := range caps {
		if strings.EqualFold(c, target) {
			return true
		}
	}

	return false
}

func buildRlimits(opts *SpecOpts) []ocispec.POSIXRlimit {
	if opts.HostConfig == nil || len(opts.HostConfig.Ulimits) == 0 {
		return nil
	}

	rlimits := make([]ocispec.POSIXRlimit, 0, len(opts.HostConfig.Ulimits))
	for _, u := range opts.HostConfig.Ulimits {
		if u == nil {
			continue
		}

		rlimits = append(rlimits, ocispec.POSIXRlimit{
			Type: "RLIMIT_" + strings.ToUpper(u.Name),
			Soft: uint64(max(u.Soft, 0)),
			Hard: uint64(max(u.Hard, 0)),
		})
	}

	return rlimits
}

func buildResources(opts *SpecOpts) *ocispec.LinuxResources {
	if opts.HostConfig == nil {
		return nil
	}

	r := &opts.HostConfig.Resources
	res := &ocispec.LinuxResources{}
	hasResources := false

	// Memory.
	if mem := buildMemoryResources(r); mem != nil {
		res.Memory = mem
		hasResources = true
	}

	// CPU.
	if cpu := buildCPUResources(r); cpu != nil {
		res.CPU = cpu
		hasResources = true
	}

	// PIDs.
	if r.PidsLimit != nil && *r.PidsLimit > 0 {
		res.Pids = &ocispec.LinuxPids{Limit: *r.PidsLimit}
		hasResources = true
	}

	if !hasResources {
		return nil
	}

	return res
}

func buildMemoryResources(r *container.Resources) *ocispec.LinuxMemory {
	if r.Memory <= 0 && r.MemoryReservation <= 0 && r.MemorySwap == 0 && r.OomKillDisable == nil {
		return nil
	}

	mem := &ocispec.LinuxMemory{}

	if r.Memory > 0 {
		mem.Limit = &r.Memory
	}

	if r.MemoryReservation > 0 {
		mem.Reservation = &r.MemoryReservation
	}

	if r.MemorySwap != 0 {
		mem.Swap = &r.MemorySwap
	}

	if r.OomKillDisable != nil {
		mem.DisableOOMKiller = r.OomKillDisable
	}

	return mem
}

func buildCPUResources(r *container.Resources) *ocispec.LinuxCPU {
	if r.CPUShares <= 0 && r.CPUQuota <= 0 && r.CPUPeriod <= 0 && r.NanoCPUs <= 0 && r.CpusetCpus == "" && r.CpusetMems == "" {
		return nil
	}

	cpu := &ocispec.LinuxCPU{}

	if r.CPUShares > 0 {
		shares := uint64(r.CPUShares)
		cpu.Shares = &shares
	}

	if r.NanoCPUs > 0 {
		// Convert NanoCPUs to quota/period.
		// Docker uses 100ms period with quota = nanoCPUs * period / 1e9.
		period := uint64(100000)
		quota := r.NanoCPUs * int64(period) / 1e9

		cpu.Period = &period
		cpu.Quota = &quota
	} else {
		if r.CPUQuota > 0 {
			cpu.Quota = &r.CPUQuota
		}

		if r.CPUPeriod > 0 {
			period := uint64(r.CPUPeriod)
			cpu.Period = &period
		}
	}

	if r.CpusetCpus != "" {
		cpu.Cpus = r.CpusetCpus
	}

	if r.CpusetMems != "" {
		cpu.Mems = r.CpusetMems
	}

	return cpu
}

func defaultMounts() []ocispec.Mount {
	return []ocispec.Mount{
		{
			Destination: "/proc",
			Type:        "proc",
			Source:      "proc",
			Options:     []string{"nosuid", "noexec", "nodev"},
		},
		{
			Destination: "/dev",
			Type:        "tmpfs",
			Source:      "tmpfs",
			Options:     []string{"nosuid", "strictatime", "mode=755", "size=65536k"},
		},
		{
			Destination: "/dev/pts",
			Type:        "devpts",
			Source:      "devpts",
			Options:     []string{"nosuid", "noexec", "newinstance", "ptmxmode=0666", "mode=0620", "gid=5"},
		},
		{
			Destination: "/dev/shm",
			Type:        "tmpfs",
			Source:      "shm",
			Options:     []string{"nosuid", "noexec", "nodev", "mode=1777", "size=65536k"},
		},
		{
			Destination: "/dev/mqueue",
			Type:        "mqueue",
			Source:      "mqueue",
			Options:     []string{"nosuid", "noexec", "nodev"},
		},
		{
			Destination: "/sys",
			Type:        "sysfs",
			Source:      "sysfs",
			Options:     []string{"nosuid", "noexec", "nodev", "ro"},
		},
		{
			Destination: "/sys/fs/cgroup",
			Type:        "cgroup",
			Source:      "cgroup",
			Options:     []string{"nosuid", "noexec", "nodev", "relatime", "ro"},
		},
	}
}

func defaultNamespaces() []ocispec.LinuxNamespace {
	return []ocispec.LinuxNamespace{
		{Type: ocispec.PIDNamespace},
		{Type: ocispec.MountNamespace},
		{Type: ocispec.IPCNamespace},
		{Type: ocispec.UTSNamespace},
		// Network namespace is added in Phase 4.
		// User namespace is not used by default (matches Docker behavior).
	}
}

func isPrivileged(opts *SpecOpts) bool {
	return opts.HostConfig != nil && opts.HostConfig.Privileged
}
