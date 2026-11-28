//go:build !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd

package grt

import (
	"net"
	"net/netip"
)

// Platforms without a control-message implementation (Windows, Plan 9,
// js/wasip1, ...) send without source selection or ECN marking, leave
// the kernel's default fragmentation policy in place, and report the
// bound address as the datagram destination.

func configureSocket(*net.UDPConn, bool) {}

func parseRecvOOB([]byte) (dst netip.Addr, ecn uint8) {
	return netip.Addr{}, 0
}

func buildSendOOB([]byte, netip.Addr, netip.AddrPort, uint8) int {
	return 0
}
