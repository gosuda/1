package grt

import (
	"testing"

	"golang.org/x/sys/unix"
)

func TestUDPSocketSetsPMTUProbeMode(t *testing.T) {
	cases := []struct {
		name         string
		bind         func() (*UDPSocket, error)
		level, opt   int
		want         int
		wantOptLabel string
	}{
		{"ipv4", func() (*UDPSocket, error) { return listenUDP("udp4", nil) }, unix.IPPROTO_IP, unix.IP_MTU_DISCOVER, unix.IP_PMTUDISC_PROBE, "IP_MTU_DISCOVER"},
		{"ipv6", func() (*UDPSocket, error) { return listenUDP("udp6", nil) }, unix.IPPROTO_IPV6, unix.IPV6_MTU_DISCOVER, unix.IPV6_PMTUDISC_PROBE, "IPV6_MTU_DISCOVER"},
		{"dual-stack ipv4-mapped", func() (*UDPSocket, error) { return listenUDP("udp", nil) }, unix.IPPROTO_IP, unix.IP_MTU_DISCOVER, unix.IP_PMTUDISC_PROBE, "IP_MTU_DISCOVER"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, err := tc.bind()
			if err != nil {
				t.Skipf("bind: %v", err)
			}
			t.Cleanup(func() { _ = s.Close() })
			if got := getsockoptInt(t, s, tc.level, tc.opt); got != tc.want {
				t.Fatalf("%s = %d, want %d", tc.wantOptLabel, got, tc.want)
			}
		})
	}
}
