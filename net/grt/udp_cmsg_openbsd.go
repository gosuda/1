//go:build openbsd

package grt

import (
	"net/netip"

	"golang.org/x/sys/unix"
)

// OpenBSD has no IPv4 don't-fragment or TOS-receive socket option and
// no IP_TOS ancillary send, so IPv4 datagrams carry no ECN marking.
const (
	// IP_SENDSRCADDR carries a bare struct in_addr.
	ipv4SrcType    = unix.IP_SENDSRCADDR
	ipv4SrcLen     = 4
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
