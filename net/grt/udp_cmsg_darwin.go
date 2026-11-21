//go:build darwin

package grt

import (
	"net/netip"

	"golang.org/x/sys/unix"
)

const (
	// IP_PKTINFO carries struct in_pktinfo; ipi_spec_dst selects the source.
	ipv4SrcType   = unix.IP_PKTINFO
	ipv4SrcLen    = 12
	ipv4SrcOffset = 4
	// darwin ignores a one-byte IP_TOS ancillary value; it needs an int.
	ipv4TOSSendLen = 4
)

// darwin rejects IPv4-level options on dual-stack sockets and reports the
// TOS of IPv4-mapped traffic through IPV6_TCLASS instead, so the IPv6
// list carries only IPv6-level options.
var (
	socketOptions4 = []sockOpt{
		{unix.IPPROTO_IP, unix.IP_DONTFRAG, 1},
		{unix.IPPROTO_IP, unix.IP_RECVTOS, 1},
	}
	socketOptions6 = []sockOpt{
		{unix.IPPROTO_IPV6, unix.IPV6_DONTFRAG, 1},
	}
)

func parseIPv4Cmsg(typ int32, data []byte, dst *netip.Addr, ecn *uint8) {
	switch typ {
	case unix.IP_PKTINFO:
		if len(data) >= 12 {
			*dst = netip.AddrFrom4([4]byte(data[8:12]))
		}
	case unix.IP_RECVDSTADDR:
		if len(data) >= 4 {
			*dst = netip.AddrFrom4([4]byte(data[:4]))
		}
	case unix.IP_RECVTOS, unix.IP_TOS:
		parseIPv4TOS(data, ecn)
	}
}
