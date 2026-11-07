//go:build !linux

package grt

import "net/netip"

func (s *UDPSocket) sendGSOPlatform(payload []byte, segmentSize uint16, src, dst netip.AddrPort, ecn uint8) (bool, error) {
	return false, nil
}
