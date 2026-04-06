package runtime

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCheckOOMKill_Positive(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	eventsPath := filepath.Join(dir, "memory.events")

	content := "low 0\nhigh 0\nmax 0\noom 1\noom_kill 3\noom_group_kill 0\n"
	if err := os.WriteFile(eventsPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	if !checkOOMKill(eventsPath) {
		t.Error("expected OOM kill detected")
	}
}

func TestCheckOOMKill_Zero(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	eventsPath := filepath.Join(dir, "memory.events")

	content := "low 0\nhigh 0\nmax 0\noom 0\noom_kill 0\n"
	if err := os.WriteFile(eventsPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	if checkOOMKill(eventsPath) {
		t.Error("expected no OOM kill")
	}
}

func TestCheckOOMKill_MissingFile(t *testing.T) {
	t.Parallel()

	if checkOOMKill("/nonexistent/memory.events") {
		t.Error("expected false for missing file")
	}
}

func TestStartOOMWatch_MissingFile(t *testing.T) {
	t.Parallel()

	m := &Manager{
		active: make(map[string]*activeContainer),
		pidMap: make(map[int]string),
	}

	// Non-existent path should return a no-op cancel.
	cancel := m.startOOMWatch("abcdef123456abcdef123456abcdef123456abcdef123456abcdef123456abcdef12", "/nonexistent/cgroup")

	// Should not panic.
	cancel()
}

func TestStartOOMWatch_Cancel(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	eventsPath := filepath.Join(dir, "memory.events")

	if err := os.WriteFile(eventsPath, []byte("oom_kill 0\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	m := &Manager{
		active: make(map[string]*activeContainer),
		pidMap: make(map[int]string),
	}

	cancel := m.startOOMWatch("abcdef123456abcdef123456abcdef123456abcdef123456abcdef123456abcdef12", dir)

	// Give goroutine time to start.
	time.Sleep(50 * time.Millisecond)

	// Cancel should not block or panic.
	cancel()

	// Give goroutine time to exit.
	time.Sleep(50 * time.Millisecond)
}
