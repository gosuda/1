//go:build freebsd

package grt

import (
	"net/netip"

	"golang.org/x/sys/unix"
)

const (
	// IP_SENDSRCADDR carries a bare struct in_addr.
	ipv4SrcType   = unix.IP_SENDSRCADDR
	ipv4SrcLen    = 4
	ipv4SrcOffset = 0
	// FreeBSD's udp_output accepts IP_TOS ancillary data only as a byte.
	ipv4TOSSendLen = 1
)

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
	case unix.IP_RECVDSTADDR:
		if len(data) >= 4 {
			*dst = netip.AddrFrom4([4]byte(data[:4]))
		}
	case unix.IP_RECVTOS, unix.IP_TOS:
		parseIPv4TOS(data, ecn)
	}
}
