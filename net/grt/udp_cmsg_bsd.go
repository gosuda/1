//go:build dragonfly || netbsd

package grt

import (
	"net/netip"

	"golang.org/x/sys/unix"
)

// These kernels expose no IPv4 source-selection, don't-fragment, or TOS
// ancillary interface through golang.org/x/sys/unix: IPv4 sends use the
// bound address and carry no ECN marking.
const (
	ipv4SrcType    = 0
	ipv4SrcLen     = 0
	ipv4SrcOffset  = 0
	ipv4TOSSendLen = 0
)

var (
	socketOptions4 = []sockOpt(nil)
	socketOptions6 = []sockOpt{
		{unix.IPPROTO_IPV6, unix.IPV6_DONTFRAG, 1},
	}
)

func parseIPv4Cmsg(typ int32, data []byte, dst *netip.Addr, _ *uint8) {
	if typ == unix.IP_RECVDSTADDR && len(data) >= 4 {
		*dst = netip.AddrFrom4([4]byte(data[:4]))
	}
}
