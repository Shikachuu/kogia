package runtime

import (
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestReceivePTYMaster(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	sockPath := filepath.Join(dir, "test.sock")

	// Start receiver in a goroutine.
	type result struct {
		file *os.File
		err  error
	}

	ch := make(chan result, 1)

	go func() {
		f, err := ReceivePTYMaster(sockPath, 5*time.Second)
		ch <- result{f, err}
	}()

	// Give the listener time to start.
	time.Sleep(50 * time.Millisecond)

	// Open a real PTY master to send.
	ptyMaster, err := os.OpenFile("/dev/ptmx", os.O_RDWR, 0)
	if err != nil {
		t.Skipf("cannot open /dev/ptmx: %v", err)
	}
	defer ptyMaster.Close()

	// Connect to the socket and send the fd via SCM_RIGHTS.
	conn, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: sockPath, Net: "unix"})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	rawConn, err := conn.SyscallConn()
	if err != nil {
		t.Fatalf("syscall conn: %v", err)
	}

	var sendErr error

	err = rawConn.Control(func(fd uintptr) {
		rights := unix.UnixRights(int(ptyMaster.Fd()))
		sendErr = unix.Sendmsg(int(fd), []byte{0}, rights, nil, 0)
	})
	if err != nil {
		t.Fatalf("control: %v", err)
	}

	if sendErr != nil {
		t.Fatalf("sendmsg: %v", sendErr)
	}

	// Receive the result.
	res := <-ch
	if res.err != nil {
		t.Fatalf("ReceivePTYMaster: %v", res.err)
	}

	defer res.file.Close()

	// Verify we got a valid fd by checking it's not the same as the original.
	if res.file.Fd() == ptyMaster.Fd() {
		t.Error("received fd should be different from original (it's a dup)")
	}

	// Verify the socket file was cleaned up.
	if _, statErr := os.Stat(sockPath); statErr == nil {
		t.Error("socket file should have been cleaned up")
	}
}

func TestReceivePTYMaster_Timeout(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	sockPath := filepath.Join(dir, "timeout.sock")

	// No client connects — should timeout.
	_, err := ReceivePTYMaster(sockPath, 100*time.Millisecond)
	if err == nil {
		t.Error("expected timeout error")
	}

	// Socket should be cleaned up.
	if _, statErr := os.Stat(sockPath); statErr == nil {
		t.Error("socket file should have been cleaned up after timeout")
	}
}

func TestResizePTY(t *testing.T) {
	t.Parallel()

	pty, err := os.OpenFile("/dev/ptmx", os.O_RDWR, 0)
	if err != nil {
		t.Skipf("cannot open /dev/ptmx: %v", err)
	}
	defer pty.Close()

	// Resize to specific dimensions.
	if resizeErr := resizePTY(pty, 40, 120); resizeErr != nil {
		t.Fatalf("resizePTY: %v", resizeErr)
	}

	// Read back the window size.
	ws, err := unix.IoctlGetWinsize(int(pty.Fd()), unix.TIOCGWINSZ)
	if err != nil {
		t.Fatalf("get winsize: %v", err)
	}

	if ws.Row != 40 {
		t.Errorf("height: got %d, want 40", ws.Row)
	}

	if ws.Col != 120 {
		t.Errorf("width: got %d, want 120", ws.Col)
	}
}
