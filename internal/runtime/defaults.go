package runtime

import "time"

// Container lifecycle defaults.
const (
	// DefaultStopTimeout is the default time to wait for a container to stop before sending SIGKILL.
	DefaultStopTimeout = 10

	// DefaultStopSignal is the signal sent to gracefully stop a container.
	DefaultStopSignal = "SIGTERM"

	// DefaultKillSignal is the signal sent to forcefully kill a container.
	DefaultKillSignal = "SIGKILL"

	// RestartBackoffBase is the initial backoff delay for container restart.
	RestartBackoffBase = 100 * time.Millisecond

	// RestartBackoffMultiplier is the factor by which the backoff delay increases on each retry.
	RestartBackoffMultiplier = 2

	// RestartBackoffMax is the maximum backoff delay for container restart.
	RestartBackoffMax = time.Minute

	// WaitPollInterval is the polling interval for Wait() when the container is not yet running.
	WaitPollInterval = 100 * time.Millisecond

	// CrunOperationTimeout is the timeout for individual crun operations (create, start, kill, delete).
	CrunOperationTimeout = 30 * time.Second

	// DefaultPathEnv is the default PATH environment variable value for containers.
	DefaultPathEnv = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

	// ContainerIDBytes is the number of random bytes used to generate a container ID.
	// The resulting hex string is twice this length (64 hex chars).
	ContainerIDBytes = 32
)
