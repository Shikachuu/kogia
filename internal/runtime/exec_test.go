package runtime

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/moby/moby/api/types/container"
	ocispec "github.com/opencontainers/runtime-spec/specs-go"
)

func TestWriteExecProcessSpec(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	// Create a minimal container OCI spec.
	containerSpec := ocispec.Spec{
		Process: &ocispec.Process{
			Terminal: false,
			Args:     []string{"/bin/sh"},
			Env:      []string{"PATH=/usr/bin:/bin", "HOME=/root"},
			Cwd:      "/",
			User:     ocispec.User{UID: 0, GID: 0},
			Capabilities: &ocispec.LinuxCapabilities{
				Bounding:  []string{"CAP_CHOWN"},
				Effective: []string{"CAP_CHOWN"},
			},
		},
	}

	specData, err := json.Marshal(containerSpec)
	if err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(dir, "config.json"), specData, 0o644); err != nil {
		t.Fatal(err)
	}

	m := &Manager{
		execSessions: make(map[string]*ExecSession),
	}

	session := &ExecSession{
		ID:          "exec123456789012345678901234567890123456789012345678901234567890",
		ContainerID: "cont123456789012345678901234567890123456789012345678901234567890",
		Config: container.ExecCreateRequest{
			Cmd:        []string{"ls", "-la"},
			Env:        []string{"FOO=bar"},
			Tty:        true,
			WorkingDir: "/tmp",
		},
	}

	ac := &activeContainer{
		bundleDir: dir,
	}

	processFile := filepath.Join(dir, "exec-test.json")

	if err := m.writeExecProcessSpec(processFile, session, ac); err != nil {
		t.Fatal(err)
	}

	// Read and verify the process spec.
	data, err := os.ReadFile(processFile)
	if err != nil {
		t.Fatal(err)
	}

	var proc ocispec.Process
	if err := json.Unmarshal(data, &proc); err != nil {
		t.Fatal(err)
	}

	if !proc.Terminal {
		t.Error("expected terminal=true")
	}

	if len(proc.Args) != 2 || proc.Args[0] != "ls" || proc.Args[1] != "-la" {
		t.Errorf("args: got %v, want [ls -la]", proc.Args)
	}

	if proc.Cwd != "/tmp" {
		t.Errorf("cwd: got %q, want /tmp", proc.Cwd)
	}

	// Should inherit container env + exec env.
	hasPath := false
	hasFoo := false

	for _, e := range proc.Env {
		if e == "PATH=/usr/bin:/bin" {
			hasPath = true
		}

		if e == "FOO=bar" {
			hasFoo = true
		}
	}

	if !hasPath {
		t.Error("expected PATH from container spec")
	}

	if !hasFoo {
		t.Error("expected FOO=bar from exec config")
	}

	// Should inherit container capabilities.
	if proc.Capabilities == nil {
		t.Fatal("expected capabilities from container spec")
	}

	if len(proc.Capabilities.Bounding) == 0 {
		t.Error("expected bounding capabilities from container spec")
	}
}

func TestExecInspect_NotFound(t *testing.T) {
	t.Parallel()

	m := &Manager{
		execSessions: make(map[string]*ExecSession),
	}

	_, err := m.ExecInspect(nil, "nonexistent")
	if err == nil {
		t.Error("expected error for nonexistent exec session")
	}
}

func TestExecInspect_SessionState(t *testing.T) {
	t.Parallel()

	m := &Manager{
		execSessions: make(map[string]*ExecSession),
	}

	exitCode := 42
	session := &ExecSession{
		ID:          "exec123456789012345678901234567890123456789012345678901234567890",
		ContainerID: "cont123456789012345678901234567890123456789012345678901234567890",
		Config: container.ExecCreateRequest{
			Cmd:          []string{"ls", "-la"},
			Tty:          true,
			AttachStdout: true,
			AttachStderr: true,
			User:         "testuser",
		},
		Running:  false,
		ExitCode: &exitCode,
		Pid:      12345,
	}

	m.execMu.Lock()
	m.execSessions[session.ID] = session
	m.execMu.Unlock()

	resp, err := m.ExecInspect(nil, session.ID)
	if err != nil {
		t.Fatal(err)
	}

	if resp.ID != session.ID {
		t.Errorf("ID: got %q, want %q", resp.ID, session.ID)
	}

	if resp.ContainerID != session.ContainerID {
		t.Errorf("ContainerID: got %q, want %q", resp.ContainerID, session.ContainerID)
	}

	if resp.Running {
		t.Error("expected Running=false")
	}

	if resp.ExitCode == nil || *resp.ExitCode != 42 {
		t.Errorf("ExitCode: got %v, want 42", resp.ExitCode)
	}

	if resp.Pid != 12345 {
		t.Errorf("Pid: got %d, want 12345", resp.Pid)
	}

	if resp.ProcessConfig == nil {
		t.Fatal("expected ProcessConfig")
	}

	if !resp.ProcessConfig.Tty {
		t.Error("expected Tty=true in ProcessConfig")
	}

	if resp.ProcessConfig.Entrypoint != "ls" {
		t.Errorf("Entrypoint: got %q, want %q", resp.ProcessConfig.Entrypoint, "ls")
	}

	if resp.ProcessConfig.User != "testuser" {
		t.Errorf("User: got %q, want %q", resp.ProcessConfig.User, "testuser")
	}
}

func TestCleanupExecSessions(t *testing.T) {
	t.Parallel()

	m := &Manager{
		execSessions: make(map[string]*ExecSession),
	}

	containerID := "cont123456789012345678901234567890123456789012345678901234567890"

	s1 := &ExecSession{
		ID:          "exec1",
		ContainerID: containerID,
		Running:     true,
	}

	s2 := &ExecSession{
		ID:          "exec2",
		ContainerID: containerID,
		Running:     true,
	}

	s3 := &ExecSession{
		ID:          "exec3",
		ContainerID: "other12345678901234567890123456789012345678901234567890123456789",
		Running:     true,
	}

	m.execSessions["exec1"] = s1
	m.execSessions["exec2"] = s2
	m.execSessions["exec3"] = s3

	m.cleanupExecSessions(containerID)

	if s1.Running {
		t.Error("s1 should be not running after cleanup")
	}

	if s1.ExitCode == nil || *s1.ExitCode != -1 {
		t.Errorf("s1 exit code: got %v, want -1", s1.ExitCode)
	}

	if s2.Running {
		t.Error("s2 should be not running after cleanup")
	}

	// s3 belongs to a different container, should be unaffected.
	if !s3.Running {
		t.Error("s3 should still be running (different container)")
	}
}
