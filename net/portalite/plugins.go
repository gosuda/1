package portalite

import (
	"context"
	"crypto/tls"
	"net"
	"time"
)

// KeylessConfig configures a relay-backed keyless TLS terminator for friendly aliases.
type KeylessConfig struct {
	RelayURL          string
	ServerName        string
	ChainPEM          []byte
	RequestTimeout    time.Duration
	AccessTokenHeader string
	AccessToken       func() string
}

// KeylessTerminator terminates tenant TLS handshakes using a remote signing backend
// (e.g. gosuda.org/portalite/keyless). Keeping this behind an interface allows callers
// that only use Local TLS or raw TCP to compile without github.com/gosuda/keyless_tls.
type KeylessTerminator interface {
	Handshake(ctx context.Context, conn net.Conn, binding []byte) (net.Conn, error)
	IsSignerError(err error) bool
	Close() error
}

// KeylessProvider constructs a KeylessTerminator for a relay lease.
type KeylessProvider interface {
	NewTerminator(cfg KeylessConfig) (KeylessTerminator, error)
}

// DatagramConn is a relay-authenticated datagram backhaul connection
// (e.g. QUIC datagrams provided by gosuda.org/portalite/udp).
type DatagramConn interface {
	SendDatagram(payload []byte) error
	ReceiveDatagram(ctx context.Context) ([]byte, error)
	CloseWithError(code uint64, reason string) error
}

// DatagramRejection is an error returned by DatagramDialer.Dial when the relay
// explicitly rejects the datagram backhaul with a protocol error code.
type DatagramRejection interface {
	error
	RejectionCode() string
}

// DatagramDialer establishes an authenticated datagram backhaul connection to a relay.
type DatagramDialer interface {
	Dial(ctx context.Context, addr string, baseTLS *tls.Config, accessToken string) (DatagramConn, error)
}

// CertObserver receives public leaf certificate installation events for a monitored
// hostname (e.g. gosuda.org/portalite/certmon) without touching private keys or TLS.
type CertObserver interface {
	RecordLocalLeafDER(der []byte)
	Close()
}

// CertObserverFactory starts a lease-scoped certificate observer for a hostname.
type CertObserverFactory interface {
	Start(hostname string) (CertObserver, error)
}
