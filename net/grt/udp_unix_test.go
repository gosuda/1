//go:build darwin || linux

package grt

import (
	"testing"

	"golang.org/x/sys/unix"
)

func getsockoptInt(t *testing.T, s *UDPSocket, level, opt int) int {
	t.Helper()
	rc, err := s.conn.SyscallConn()
	if err != nil {
		t.Fatalf("SyscallConn: %v", err)
	}
	var v int
	var gerr error
	if err := rc.Control(func(fd uintptr) { v, gerr = unix.GetsockoptInt(int(fd), level, opt) }); err != nil {
		t.Fatalf("Control: %v", err)
	}
	if gerr != nil {
		t.Fatalf("getsockopt(level=%d, opt=%d): %v", level, opt, gerr)
	}
	return v
}
