//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd

package grt

import (
	"encoding/binary"
	"net"
	"net/netip"
	"unsafe"

	"golang.org/x/sys/unix"
)

// sockOpt is one integer socket option applied at bind time.
type sockOpt struct {
	level, name, value int
}

// configureSocket applies the platform's DF/PMTU and ECN-receive options.
// Each option is best effort: a kernel that rejects one (for example an
// IPv4-level option on a darwin dual-stack socket) still yields a usable
// socket, only without that capability.
func configureSocket(conn *net.UDPConn, ipv4 bool) {
	opts := socketOptions6
	if ipv4 {
		opts = socketOptions4
	}
	rc, err := conn.SyscallConn()
	if err != nil {
		return
	}
	_ = rc.Control(func(fd uintptr) {
		for _, o := range opts {
			_ = unix.SetsockoptInt(int(fd), o.level, o.name, o.value)
		}
	})
}

// putCmsg writes a control-message header at oob[off:] and returns the
// header's data slice and the offset of the next message.
func putCmsg(oob []byte, off int, level, typ int32, dataLen int) (data []byte, next int) {
	cmsg := (*unix.Cmsghdr)(unsafe.Pointer(&oob[off]))
	cmsg.Level = level
	cmsg.Type = typ
	cmsg.SetLen(unix.CmsgLen(dataLen))
	start := off + unix.CmsgLen(0)
	data = oob[start : start+dataLen]
	clear(data)
	return data, off + unix.CmsgSpace(dataLen)
}

// buildSendOOB encodes the source-address and ECN control messages for
// one datagram into oob and returns the encoded length. oob must hold
// sendOOBSize bytes. IPv4 and IPv4-mapped destinations carry ECN in
// IP_TOS (also honoured for mapped destinations on Linux, darwin and
// FreeBSD dual-stack sockets); native IPv6 destinations use IPV6_TCLASS.
func buildSendOOB(oob []byte, src netip.Addr, dst netip.AddrPort, ecn uint8) int {
	off := 0
	if src.IsValid() && !src.IsUnspecified() {
		switch {
		case src.Is4() && ipv4SrcLen > 0:
			var data []byte
			data, off = putCmsg(oob, off, unix.IPPROTO_IP, ipv4SrcType, ipv4SrcLen)
			ip4 := src.As4()
			copy(data[ipv4SrcOffset:], ip4[:])
		case src.Is6() && !src.Is4In6():
			var data []byte
			data, off = putCmsg(oob, off, unix.IPPROTO_IPV6, unix.IPV6_PKTINFO, 20)
			ip6 := src.As16()
			copy(data, ip6[:])
		}
	}
	if ecn == 0 {
		return off
	}
	ecn &= 3
	switch d := dst.Addr(); {
	case d.Is4() || d.Is4In6():
		if ipv4TOSSendLen == 0 {
			return off
		}
		var data []byte
		data, off = putCmsg(oob, off, unix.IPPROTO_IP, unix.IP_TOS, ipv4TOSSendLen)
		if ipv4TOSSendLen == 4 {
			binary.NativeEndian.PutUint32(data, uint32(ecn))
		} else {
			data[0] = ecn
		}
	case d.Is6():
		var data []byte
		data, off = putCmsg(oob, off, unix.IPPROTO_IPV6, unix.IPV6_TCLASS, 4)
		binary.NativeEndian.PutUint32(data, uint32(ecn))
	}
	return off
}

// parseRecvOOB extracts the destination address and ECN codepoint from
// received control messages. Unknown messages are ignored.
func parseRecvOOB(oob []byte) (dst netip.Addr, ecn uint8) {
	rem := oob
	for len(rem) > 0 {
		hdr, data, next, err := unix.ParseOneSocketControlMessage(rem)
		if err != nil {
			return dst, ecn
		}
		rem = next
		switch hdr.Level {
		case unix.IPPROTO_IP:
			parseIPv4Cmsg(hdr.Type, data, &dst, &ecn)
		case unix.IPPROTO_IPV6:
			switch hdr.Type {
			case unix.IPV6_PKTINFO:
				if len(data) >= 16 {
					dst = netip.AddrFrom16([16]byte(data[:16]))
				}
			case unix.IPV6_TCLASS:
				if len(data) >= 4 {
					ecn = uint8(binary.NativeEndian.Uint32(data[:4]) & 3)
				}
			}
		}
	}
	return dst, ecn
}

// parseIPv4TOS reads the ECN bits of a received IPv4 TOS control
// message, which kernels deliver as a single byte (some as an int).
func parseIPv4TOS(data []byte, ecn *uint8) {
	switch len(data) {
	case 0:
	case 4:
		*ecn = uint8(binary.NativeEndian.Uint32(data) & 3)
	default:
		*ecn = data[0] & 3
	}
}
