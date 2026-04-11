package runtime

import (
	"errors"
	"fmt"
	"net"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

var (
	// ErrNoControlMessages is returned when no SCM control messages are received.
	ErrNoControlMessages = errors.New("no control messages received")
	// ErrNoFileDescriptors is returned when no file descriptors are received via SCM_RIGHTS.
	ErrNoFileDescriptors = errors.New("no file descriptors received")
)

// ReceivePTYMaster listens on a Unix socket and receives a PTY master file
// descriptor via SCM_RIGHTS. crun connects to this socket during `crun create`
// when --console-socket is specified, sending the PTY master fd.
func ReceivePTYMaster(socketPath string, timeout time.Duration) (*os.File, error) {
	addr := &net.UnixAddr{Name: socketPath, Net: "unix"}

	ln, err := net.ListenUnix("unix", addr)
	if err != nil {
		return nil, fmt.Errorf("console socket listen: %w", err)
	}

	defer func() {
		_ = ln.Close()
		_ = os.Remove(socketPath)
	}()

	if deadlineErr := ln.SetDeadline(time.Now().Add(timeout)); deadlineErr != nil {
		return nil, fmt.Errorf("console socket deadline: %w", deadlineErr)
	}

	conn, err := ln.AcceptUnix()
	if err != nil {
		return nil, fmt.Errorf("console socket accept: %w", err)
	}

	defer func() { _ = conn.Close() }()

	// Use Go's ReadMsgUnix which properly handles the non-blocking fd
	// through the runtime poller, avoiding EAGAIN errors.
	buf := make([]byte, 1)
	oob := make([]byte, unix.CmsgLen(4)) // space for one fd

	_, oobn, _, _, err := conn.ReadMsgUnix(buf, oob) //nolint:dogsled // ReadMsgUnix returns 5 values; only oobn and err are needed.
	if err != nil {
		return nil, fmt.Errorf("console socket recvmsg: %w", err)
	}

	msgs, err := unix.ParseSocketControlMessage(oob[:oobn])
	if err != nil {
		return nil, fmt.Errorf("parse control message: %w", err)
	}

	if len(msgs) == 0 {
		return nil, ErrNoControlMessages
	}

	fds, err := unix.ParseUnixRights(&msgs[0])
	if err != nil {
		return nil, fmt.Errorf("parse unix rights: %w", err)
	}

	if len(fds) == 0 {
		return nil, ErrNoFileDescriptors
	}

	// Close any extra fds we received.
	for _, extra := range fds[1:] {
		_ = unix.Close(extra)
	}

	return os.NewFile(uintptr(fds[0]), "pty-master"), nil //nolint:gosec // File descriptor from SCM_RIGHTS fits in uintptr.
}

// resizePTY sets the terminal window size on a PTY master fd.
func resizePTY(ptyMaster *os.File, height, width uint16) error {
	ws := &unix.Winsize{
		Row: height,
		Col: width,
	}

	if err := unix.IoctlSetWinsize(int(ptyMaster.Fd()), unix.TIOCSWINSZ, ws); err != nil { //nolint:gosec // File descriptor fits in int.
		return fmt.Errorf("set winsize: %w", err)
	}

	return nil
}
