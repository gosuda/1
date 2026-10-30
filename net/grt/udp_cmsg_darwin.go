//go:build darwin || freebsd || netbsd || openbsd

package grt

import (
	"encoding/binary"
	"net/netip"
	"unsafe"

	"golang.org/x/sys/unix"
)

func parseRecvOOB(oob []byte) (dst netip.Addr, ecn uint8) {
	rem := oob
	for len(rem) > 0 {
		hdr, data, next, err := unix.ParseOneSocketControlMessage(rem)
		if err != nil {
			return
		}
		rem = next
		switch hdr.Level {
		case unix.IPPROTO_IP:
			switch hdr.Type {
			case unix.IP_PKTINFO:
				if len(data) >= 12 {
					dst = netip.AddrFrom4(*(*[4]byte)(data[8:12]))
				}
			case unix.IP_RECVDSTADDR:
				if len(data) >= 4 {
					dst = netip.AddrFrom4(*(*[4]byte)(data[:4]))
				}
			}
		case unix.IPPROTO_IPV6:
			switch hdr.Type {
			case unix.IPV6_PKTINFO:
				if len(data) >= 16 {
					dst = netip.AddrFrom16(*(*[16]byte)(data[:16]))
				}
			case unix.IPV6_TCLASS:
				if len(data) >= 4 {
					ecn = uint8(binary.NativeEndian.Uint32(data[:4]) & 3)
				}
			}
		}
	}
	return
}

func buildSendOOB(oob []byte, src netip.Addr, dst netip.AddrPort, ecn uint8) int {
	off := 0
	if src.IsValid() && !src.IsUnspecified() {
		if src.Is4() {
			cmsg := (*unix.Cmsghdr)(unsafe.Pointer(&oob[off]))
			cmsg.Level = unix.IPPROTO_IP
			cmsg.Type = unix.IP_PKTINFO
			cmsg.SetLen(unix.CmsgLen(12))
			ip4 := src.As4()
			copy(oob[off+unix.SizeofCmsghdr+4:off+unix.SizeofCmsghdr+8], ip4[:])
			off += unix.CmsgSpace(12)
		} else if src.Is6() && !src.Is4In6() {
			cmsg := (*unix.Cmsghdr)(unsafe.Pointer(&oob[off]))
			cmsg.Level = unix.IPPROTO_IPV6
			cmsg.Type = unix.IPV6_PKTINFO
			cmsg.SetLen(unix.CmsgLen(20))
			ip6 := src.As16()
			copy(oob[off+unix.SizeofCmsghdr:off+unix.SizeofCmsghdr+16], ip6[:])
			off += unix.CmsgSpace(20)
		}
	}
	if ecn != 0 && dst.Addr().Is6() && !dst.Addr().Is4In6() {
		cmsg := (*unix.Cmsghdr)(unsafe.Pointer(&oob[off]))
		cmsg.Level = unix.IPPROTO_IPV6
		cmsg.Type = unix.IPV6_TCLASS
		cmsg.SetLen(unix.CmsgLen(4))
		binary.NativeEndian.PutUint32(oob[off+unix.SizeofCmsghdr:off+unix.SizeofCmsghdr+4], uint32(ecn))
		off += unix.CmsgSpace(4)
	}
	return off
}
