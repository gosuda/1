package grt

import (
	"net"
	"net/netip"
	"runtime"
	"testing"
	"time"
)

// ecnCase is one sender/receiver socket pairing for the ECN round trip.
type ecnCase struct {
	name     string
	receiver func() (*UDPSocket, error)
	sender   func(dst netip.AddrPort) (*UDPSocket, error)
	// dst rewrites the receiver's bound address into the send target.
	dst func(bound netip.AddrPort) netip.AddrPort
}

func loopbackTarget(ip string) func(netip.AddrPort) netip.AddrPort {
	return func(bound netip.AddrPort) netip.AddrPort {
		return netip.AddrPortFrom(netip.MustParseAddr(ip), bound.Port())
	}
}

func TestUDPSocketECNRoundTrip(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skipf("ECN ancillary data is not verified on %s", runtime.GOOS)
	}
	listenOn := func(ip net.IP) func() (*UDPSocket, error) {
		return func() (*UDPSocket, error) { return ListenUDP(&net.UDPAddr{IP: ip}) }
	}
	dualStack := func(netip.AddrPort) (*UDPSocket, error) { return ListenUDP(&net.UDPAddr{}) }
	cases := []ecnCase{
		{"ipv4", listenOn(net.IPv4(127, 0, 0, 1)), DialUDPSocket, loopbackTarget("127.0.0.1")},
		{"ipv6", listenOn(net.IPv6loopback), DialUDPSocket, loopbackTarget("::1")},
		{"ipv4 into dual-stack", listenOn(nil), DialUDPSocket, loopbackTarget("127.0.0.1")},
		{"dual-stack to ipv4", listenOn(net.IPv4(127, 0, 0, 1)), dualStack, loopbackTarget("127.0.0.1")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rx, err := tc.receiver()
			if err != nil {
				t.Skipf("bind receiver: %v", err)
			}
			t.Cleanup(func() { _ = rx.Close() })
			dst := tc.dst(rx.LocalAddr())
			tx, err := tc.sender(dst)
			if err != nil {
				t.Skipf("bind sender: %v", err)
			}
			t.Cleanup(func() { _ = tx.Close() })

			for _, want := range []uint8{1, 2, 3} {
				payload := []byte{0xEC, want}
				if err := tx.Send(payload, dst, want); err != nil {
					t.Fatalf("Send(ecn=%d) to %v: %v", want, dst, err)
				}
				if err := rx.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
					t.Fatal(err)
				}
				buf := make([]byte, 64)
				n, _, gotDst, got, err := rx.Recv(buf)
				if err != nil {
					t.Fatalf("Recv after Send(ecn=%d): %v", want, err)
				}
				if n != len(payload) || buf[1] != want {
					t.Fatalf("received %x, want %x", buf[:n], payload)
				}
				if got != want {
					t.Errorf("ECN codepoint to %v: got %d, want %d", dst, got, want)
				}
				if gotDst.Addr() != dst.Addr() {
					t.Errorf("reported destination %v, want %v", gotDst, dst)
				}
			}
		})
	}
}
