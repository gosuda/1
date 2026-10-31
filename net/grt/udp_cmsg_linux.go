//go:build linux

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
	// Linux accepts an int or a byte for IP_TOS ancillary data.
	ipv4TOSSendLen = 4
)

// IP_PMTUDISC_PROBE sets DF on every datagram while ignoring the kernel's
// cached path MTU, so PMTU probes (spec section 17.2) test the real path
// instead of being refused or fragmented locally. Dual-stack sockets also
// take the IPv4-level options for IPv4-mapped traffic.
var (
	socketOptions4 = []sockOpt{
		{unix.IPPROTO_IP, unix.IP_MTU_DISCOVER, unix.IP_PMTUDISC_PROBE},
		{unix.IPPROTO_IP, unix.IP_RECVTOS, 1},
	}
	socketOptions6 = []sockOpt{
		{unix.IPPROTO_IPV6, unix.IPV6_MTU_DISCOVER, unix.IPV6_PMTUDISC_PROBE},
		{unix.IPPROTO_IP, unix.IP_MTU_DISCOVER, unix.IP_PMTUDISC_PROBE},
		{unix.IPPROTO_IP, unix.IP_RECVTOS, 1},
	}
)

func parseIPv4Cmsg(typ int32, data []byte, dst *netip.Addr, ecn *uint8) {
	switch typ {
	case unix.IP_PKTINFO:
		if len(data) >= 12 {
			*dst = netip.AddrFrom4([4]byte(data[8:12]))
		}
	case unix.IP_TOS:
		parseIPv4TOS(data, ecn)
	}
}
