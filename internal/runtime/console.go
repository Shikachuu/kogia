package runtime

import (
	"fmt"
	"net"
	"os"
	"time"

	"golang.org/x/sys/unix"
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

	if err := ln.SetDeadline(time.Now().Add(timeout)); err != nil {
		return nil, fmt.Errorf("console socket deadline: %w", err)
	}

	conn, err := ln.AcceptUnix()
	if err != nil {
		return nil, fmt.Errorf("console socket accept: %w", err)
	}
	defer conn.Close()

	// Use Go's ReadMsgUnix which properly handles the non-blocking fd
	// through the runtime poller, avoiding EAGAIN errors.
	buf := make([]byte, 1)
	oob := make([]byte, unix.CmsgLen(4)) // space for one fd

	_, oobn, _, _, err := conn.ReadMsgUnix(buf, oob)
	if err != nil {
		return nil, fmt.Errorf("console socket recvmsg: %w", err)
	}

	msgs, err := unix.ParseSocketControlMessage(oob[:oobn])
	if err != nil {
		return nil, fmt.Errorf("parse control message: %w", err)
	}

	if len(msgs) == 0 {
		return nil, fmt.Errorf("no control messages received")
	}

	fds, err := unix.ParseUnixRights(&msgs[0])
	if err != nil {
		return nil, fmt.Errorf("parse unix rights: %w", err)
	}

	if len(fds) == 0 {
		return nil, fmt.Errorf("no file descriptors received")
	}

	// Close any extra fds we received.
	for _, extra := range fds[1:] {
		unix.Close(extra)
	}

	return os.NewFile(uintptr(fds[0]), "pty-master"), nil
}

// resizePTY sets the terminal window size on a PTY master fd.
func resizePTY(ptyMaster *os.File, height, width uint16) error {
	ws := &unix.Winsize{
		Row: height,
		Col: width,
	}

	return unix.IoctlSetWinsize(int(ptyMaster.Fd()), unix.TIOCSWINSZ, ws)
}
