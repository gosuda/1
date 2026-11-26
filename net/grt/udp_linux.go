//go:build linux

package grt

import (
	"encoding/binary"
	"errors"
	"net"
	"net/netip"
	"os"
	"unsafe"

	"golang.org/x/sys/unix"
)

func (s *UDPSocket) sendGSOPlatform(payload []byte, segmentSize uint16, src, dst netip.AddrPort, ecn uint8) (bool, error) {
	var oob [sendOOBSize]byte
	ip := src.Addr().Unmap()
	var sendSrc netip.Addr
	if src != s.addr && ip.IsValid() && !ip.IsUnspecified() && ip != s.addr.Addr().Unmap() {
		sendSrc = ip
	}
	off := buildSendOOB(oob[:], sendSrc, dst, ecn)

	cmsgBuf := oob[off:]
	cmsg := (*unix.Cmsghdr)(unsafe.Pointer(&cmsgBuf[0]))
	cmsg.Level = unix.SOL_UDP
	cmsg.Type = unix.UDP_SEGMENT
	cmsg.SetLen(unix.CmsgLen(2))
	binary.NativeEndian.PutUint16(cmsgBuf[unix.SizeofCmsghdr:], segmentSize)
	off += unix.CmsgSpace(2)

	_, _, err := s.conn.WriteMsgUDPAddrPort(payload, oob[:off], dst)
	if err != nil {
		if isGSOUnsupported(err) {
			s.gsoDisabled.Store(true)
			return false, nil
		}
		return true, err
	}
	return true, nil
}

func isGSOUnsupported(err error) bool {
	var se *net.OpError
	if errors.As(err, &se) {
		err = se.Err
	}
	var scErr *os.SyscallError
	if errors.As(err, &scErr) {
		err = scErr.Err
	}
	return errors.Is(err, unix.EINVAL) ||
		errors.Is(err, unix.EOPNOTSUPP) ||
		errors.Is(err, unix.ENOPROTOOPT) ||
		errors.Is(err, unix.ENOSYS)
}
