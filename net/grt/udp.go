package grt

import (
	"fmt"
	"net"
	"net/netip"
	"sync/atomic"
	"time"

	"golang.org/x/net/ipv4"
	"golang.org/x/net/ipv6"

	"gosuda.org/grt/internal/engine"
)

// UDPSocket is the portable socket-profile adapter (spec section 29.1).
// It wraps a UDP socket with per-packet destination-address delivery
// (IP_PKTINFO / IPV6_PKTINFO) so the engine always sees the canonical
// local endpoint of section 6.2 — the actual destination address and
// port, not the wildcard bind address.
//
// ECN: inbound markings are read from IPV6_TCLASS where the platform
// supplies them; outbound marking is applied per packet on IPv6 via
// IPV6_TCLASS. The IPv4 control path carries no portable per-packet TOS
// through this interface, so IPv4 sends leave ECN unmarked — safe
// because ECT marking starts only after negotiated capability probing
// (spec section 15.4).
type UDPSocket struct {
	conn        *net.UDPConn
	p4          *ipv4.PacketConn // non-nil when bound to an IPv4 endpoint
	p6          *ipv6.PacketConn // non-nil when bound to IPv6 or dual-stack
	addr        netip.AddrPort
	gsoDisabled atomic.Bool
}

// ListenUDP binds a UDP socket for the transport. A nil or wildcard
// addr binds an ephemeral port; an IPv6 wildcard gets dual-stack where
// the platform permits. The returned socket implements
// engine.PacketSocket; callers normally use Listen or Dial instead of
// driving it directly.
func ListenUDP(addr *net.UDPAddr) (*UDPSocket, error) {
	return listenUDP("udp", addr)
}

func listenUDP(network string, addr *net.UDPAddr) (*UDPSocket, error) {
	conn, err := net.ListenUDP(network, addr)
	if err != nil {
		return nil, fmt.Errorf("grt: udp listen: %w", err)
	}
	s := &UDPSocket{conn: conn}
	s.addr, _ = netip.ParseAddrPort(conn.LocalAddr().String())
	la := conn.LocalAddr().(*net.UDPAddr)
	if la.IP.To4() != nil {
		p4 := ipv4.NewPacketConn(conn)
		// Destination address per datagram; failures degrade to the
		// bound address which is still canonical for non-wildcard binds.
		_ = p4.SetControlMessage(ipv4.FlagDst, true)
		s.p4 = p4
	} else {
		p6 := ipv6.NewPacketConn(conn)
		_ = p6.SetControlMessage(ipv6.FlagDst|ipv6.FlagTrafficClass, true)
		s.p6 = p6
	}
	return s, nil
}

// DialUDPSocket binds an unconnected UDP socket suitable for a client
// connection to remote, matching the remote's address family. The
// socket stays unconnected so later local tuples (migration, rebinding)
// remain observable.
func DialUDPSocket(remote netip.AddrPort) (*UDPSocket, error) {
	if remote.Addr().Is4() {
		return listenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero})
	}
	return listenUDP("udp6", nil)
}

// Send publishes one datagram to dst with ECN codepoint ecn (0–3).
// A nil return commits the packet to EMITTED (spec section 29.3).
func (s *UDPSocket) Send(payload []byte, dst netip.AddrPort, ecn uint8) error {
	if ecn != 0 && dst.Addr().Is6() && !dst.Addr().Is4In6() {
		var oob [64]byte
		n := buildSendOOB(oob[:], netip.Addr{}, dst, ecn)
		if n > 0 {
			_, _, err := s.conn.WriteMsgUDPAddrPort(payload, oob[:n], dst)
			return err
		}
	}
	_, err := s.conn.WriteToUDPAddrPort(payload, dst)
	return err
}

// SendFrom publishes one datagram from local endpoint src to dst — the
// engine's optional SourceSocket capability (spec section 30: a send is
// bound to its authorized path tuple). Only the source address is
// selectable: a UDP socket's port is fixed at bind, so a same-address
// or unexpressible src degrades to Send on the bound tuple.
func (s *UDPSocket) SendFrom(payload []byte, src, dst netip.AddrPort, ecn uint8) error {
	ip := src.Addr().Unmap()
	if src != s.addr && ip.IsValid() && !ip.IsUnspecified() && ip != s.addr.Addr().Unmap() {
		var oob [64]byte
		n := buildSendOOB(oob[:], ip, dst, ecn)
		if n > 0 {
			_, _, err := s.conn.WriteMsgUDPAddrPort(payload, oob[:n], dst)
			return err
		}
	}
	return s.Send(payload, dst, ecn)
}

// SendGSO delivers an aggregate payload consisting of uniform segments
// of segmentSize (the final segment may be smaller) to dst.
func (s *UDPSocket) SendGSO(payload []byte, segmentSize uint16, dst netip.AddrPort, ecn uint8) error {
	return s.SendFromGSO(payload, segmentSize, s.addr, dst, ecn)
}

// SendFromGSO delivers an aggregate payload from src to dst.
func (s *UDPSocket) SendFromGSO(payload []byte, segmentSize uint16, src, dst netip.AddrPort, ecn uint8) error {
	if segmentSize == 0 || len(payload) <= int(segmentSize) {
		return s.SendFrom(payload, src, dst, ecn)
	}
	if !s.gsoDisabled.Load() {
		ok, err := s.sendGSOPlatform(payload, segmentSize, src, dst, ecn)
		if ok {
			return err
		}
	}
	// Fallback to individual chunk sends if GSO is unsupported or syscall failed.
	sze := int(segmentSize)
	for len(payload) > 0 {
		chunk := payload
		if len(chunk) > sze {
			chunk = chunk[:sze]
		}
		if err := s.SendFrom(chunk, src, dst, ecn); err != nil {
			return err
		}
		payload = payload[len(chunk):]
	}
	return nil
}

// Recv reads one datagram, reporting the canonical source and
// destination endpoints plus the received ECN codepoint.
func (s *UDPSocket) Recv(buf []byte) (n int, src, dst netip.AddrPort, ecn uint8, err error) {
	var oob [128]byte
	var oobn int
	n, oobn, _, src, err = s.conn.ReadMsgUDPAddrPort(buf, oob[:])
	if err != nil {
		return 0, src, dst, 0, err
	}
	if oobn > 0 {
		dstIP, parsedECN := parseRecvOOB(oob[:oobn])
		if dstIP.IsValid() {
			dst = netip.AddrPortFrom(dstIP.Unmap(), s.addr.Port())
		} else {
			dst = s.addr
		}
		ecn = parsedECN
	} else {
		dst = s.addr
	}
	return n, src, dst, ecn, nil
}

// LocalAddr returns the bound endpoint.
func (s *UDPSocket) LocalAddr() netip.AddrPort { return s.addr }

// SetReadDeadline sets the receive deadline, primarily for pump
// cancellation in custom drivers.
func (s *UDPSocket) SetReadDeadline(t time.Time) error {
	return s.conn.SetReadDeadline(t)
}

// Close releases the socket.
func (s *UDPSocket) Close() error { return s.conn.Close() }

// UDPSocket implements the engine's packet-socket contract, including
// the optional multi-source send capability.
var (
	_ engine.PacketSocket    = (*UDPSocket)(nil)
	_ engine.SourceSocket    = (*UDPSocket)(nil)
	_ engine.GSOSocket       = (*UDPSocket)(nil)
	_ engine.SourceGSOSocket = (*UDPSocket)(nil)
)
