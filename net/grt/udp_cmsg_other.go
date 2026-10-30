//go:build !linux && !darwin && !freebsd && !netbsd && !openbsd

package grt

import "net/netip"

func parseRecvOOB(oob []byte) (dst netip.Addr, ecn uint8) {
	return netip.Addr{}, 0
}

func buildSendOOB(oob []byte, src netip.Addr, dst netip.AddrPort, ecn uint8) int {
	return 0
}
