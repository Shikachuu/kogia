package main

import (
	"fmt"
	"os"

	"github.com/containers/storage/pkg/reexec"
)

// handleReexec handles containers/storage subprocess re-execution.
//
// containers/storage re-executes the current binary for chroot layer operations
// (apply, tar, untar). It sets cmd.Args[0] to handler names like "storage-applyLayer",
// but in some environments (Go 1.26+, Lima VMs) os.Args[0] resolves to the binary
// path instead of the handler name. This function detects reexec invocations via
// environmental signals and patches os.Args[0] before calling reexec.Init().
//
// MUST be called at the very start of main(), before cobra or any other argument
// parsing — reexec subprocesses receive non-cobra args (e.g. layer paths) that
// would cause cobra to error with "unknown command".
//
// Returns true if this invocation was a reexec subprocess (caller should return
// from main immediately).
func handleReexec() bool {
	fixReexecArgs()

	return reexec.Init()
}

// fixReexecArgs detects containers/storage reexec subprocess invocations where
// os.Args[0] was not set to the handler name and patches it so reexec.Init()
// can match the registered handler.
func fixReexecArgs() {
	// storage-applyLayer: called with [binary, dest] and OPT env var.
	if len(os.Args) == 2 && os.Getenv("OPT") != "" {
		os.Args[0] = "storage-applyLayer"

		return
	}

	// storage-untar: called with [binary, dest, root] and extra file descriptors (fd 3, 4).
	if len(os.Args) == 3 && fileDescriptorExists(3) {
		os.Args[0] = "storage-untar"

		return
	}

	// storage-tar: called with [binary, src, root] and stdin is a pipe (JSON options).
	// Shares the same arg count as untar but without extra fds.
	// We also verify stdin is a pipe — when launched from a terminal (e.g.
	// "kogia daemon --help"), stdin is a TTY and must not be treated as reexec.
	if len(os.Args) == 3 && !fileDescriptorExists(3) && stdinIsPipe() {
		os.Args[0] = "storage-tar"

		return
	}
}

// fileDescriptorExists checks if a file descriptor is open.
func fileDescriptorExists(fd int) bool {
	_, err := os.Stat(fmt.Sprintf("/proc/self/fd/%d", fd))

	return err == nil
}

// stdinIsPipe returns true if stdin is a pipe or regular file (not a terminal).
// Used to distinguish real storage-tar reexec (stdin is a pipe carrying JSON
// options) from normal CLI invocations where stdin is a TTY.
func stdinIsPipe() bool {
	fi, err := os.Stdin.Stat()
	if err != nil {
		return false
	}

	return fi.Mode()&os.ModeCharDevice == 0
}
