package runtime

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/moby/moby/api/types/container"
)

var (
	// ErrInvalidStatFormat is returned when /proc/{pid}/stat cannot be parsed.
	ErrInvalidStatFormat = errors.New("invalid stat format")
	// ErrStatTooFewFields is returned when /proc/{pid}/stat has too few fields.
	ErrStatTooFewFields = errors.New("stat has too few fields")
)

// defaultPSTitles are the column headers for container top output.
var defaultPSTitles = []string{"UID", "PID", "PPID", "C", "STIME", "TTY", "TIME", "CMD"}

// readContainerProcesses reads process info from /proc for all PIDs in the
// container's cgroup.
func readContainerProcesses(cgroupPath string) (*container.TopResponse, error) {
	// Read all PIDs from the cgroup.
	procsPath := cgroupPath + "/cgroup.procs"

	data, err := os.ReadFile(procsPath) //nolint:gosec // Path constructed from resolved cgroup path.
	if err != nil {
		return nil, fmt.Errorf("read cgroup.procs: %w", err)
	}

	lines := strings.Split(strings.TrimSpace(string(data)), "\n")

	var processes [][]string

	for _, line := range lines {
		pid := strings.TrimSpace(line)
		if pid == "" {
			continue
		}

		proc, procErr := readProcInfo(pid)
		if procErr != nil {
			continue // process may have exited
		}

		processes = append(processes, proc)
	}

	return &container.TopResponse{
		Titles:    defaultPSTitles,
		Processes: processes,
	}, nil
}

// readProcInfo reads process info from /proc/{pid}/stat and /proc/{pid}/cmdline.
// Returns fields matching defaultPSTitles: UID, PID, PPID, C, STIME, TTY, TIME, CMD.
func readProcInfo(pid string) ([]string, error) {
	statPath := "/proc/" + pid + "/stat"

	statData, err := os.ReadFile(statPath) //nolint:gosec // PID from cgroup.procs.
	if err != nil {
		return nil, fmt.Errorf("read proc stat: %w", err)
	}

	// Parse /proc/{pid}/stat. The format is:
	// pid (comm) state ppid pgrp session tty_nr ...
	// The comm field can contain spaces and parens, so find the last ')'.
	statStr := string(statData)
	lastParen := strings.LastIndex(statStr, ")")

	if lastParen < 0 {
		return nil, ErrInvalidStatFormat
	}

	// Fields after the closing paren.
	afterComm := strings.Fields(statStr[lastParen+2:])
	if len(afterComm) < 20 {
		return nil, ErrStatTooFewFields
	}

	ppid := afterComm[1]  // field 4 (0-indexed: state=0, ppid=1)
	ttyNr := afterComm[4] // field 7

	// Read UID from /proc/{pid}/status.
	uid := readProcUID(pid)

	// Read cmdline.
	cmdline := readProcCmdline(pid)

	// TTY: convert tty_nr to string. 0 = no tty.
	tty := "?"
	if ttyNr == "0" {
		tty = "?"
	}

	return []string{
		uid,
		pid,
		ppid,
		"0",     // C (CPU usage) — simplified
		"?",     // STIME — would need /proc/stat + boot time
		tty,     // TTY
		"0:00",  // TIME — would need utime+stime calculation
		cmdline, // CMD
	}, nil
}

// readProcUID reads the UID from /proc/{pid}/status.
func readProcUID(pid string) string {
	data, err := os.ReadFile("/proc/" + pid + "/status") //nolint:gosec // PID from cgroup.procs.
	if err != nil {
		return "?"
	}

	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "Uid:") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				// Convert numeric UID to string; could resolve via /etc/passwd but keep it simple.
				return fields[1]
			}
		}
	}

	return "?"
}

// readProcCmdline reads /proc/{pid}/cmdline and returns it as a single string.
func readProcCmdline(pid string) string {
	data, err := os.ReadFile("/proc/" + pid + "/cmdline") //nolint:gosec // PID from cgroup.procs.
	if err != nil || len(data) == 0 {
		return "[" + readProcComm(pid) + "]"
	}

	// cmdline is NUL-separated. Replace NULs with spaces.
	cmdline := strings.ReplaceAll(string(data), "\x00", " ")

	return strings.TrimSpace(cmdline)
}

// readProcComm reads /proc/{pid}/comm as a fallback when cmdline is empty.
func readProcComm(pid string) string {
	data, err := os.ReadFile("/proc/" + pid + "/comm") //nolint:gosec // PID from cgroup.procs.
	if err != nil {
		return pid
	}

	return strings.TrimSpace(string(data))
}
