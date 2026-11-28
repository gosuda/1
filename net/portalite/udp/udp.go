// Package udp provides a portalite.DatagramDialer backed by
// github.com/quic-go/quic-go for UDP datagram backhaul sessions.
package udp

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"time"

	"github.com/quic-go/quic-go"

	"gosuda.org/portalite"
)

const (
	quicBackhaulALPN             = "portal-tunnel"
	quicBackhaulControlTimeout   = 10 * time.Second
	quicBackhaulControlBodyLimit = 4096
	quicBackhaulKeepAlive        = 15 * time.Second
	quicBackhaulIdleTimeout      = 60 * time.Second
)

type dialer struct{}

// NewDialer returns a DatagramDialer backed by QUIC DATAGRAM sessions.
func NewDialer() portalite.DatagramDialer {
	return dialer{}
}

type controlMessage struct {
	AccessToken string `json:"access_token"`
}

type controlResponse struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

type rejectionError struct {
	code string
}

func (e *rejectionError) Error() string {
	code := strings.TrimSpace(e.code)
	if code == "" {
		code = "rejected"
	}
	return "QUIC backhaul rejected: " + code
}

func (e *rejectionError) RejectionCode() string {
	return e.code
}

type quicDatagramConn struct {
	conn *quic.Conn
}

func (c *quicDatagramConn) SendDatagram(payload []byte) error {
	return c.conn.SendDatagram(payload)
}

func (c *quicDatagramConn) ReceiveDatagram(ctx context.Context) ([]byte, error) {
	return c.conn.ReceiveDatagram(ctx)
}

func (c *quicDatagramConn) CloseWithError(code uint64, reason string) error {
	return c.conn.CloseWithError(quic.ApplicationErrorCode(code), reason)
}

func (dialer) Dial(ctx context.Context, address string, baseTLS *tls.Config, accessToken string) (portalite.DatagramConn, error) {
	if ctx == nil {
		return nil, errors.New("QUIC backhaul context is nil")
	}
	if strings.TrimSpace(accessToken) == "" {
		return nil, errors.New("QUIC backhaul access token is required")
	}
	if _, _, err := net.SplitHostPort(address); err != nil {
		return nil, fmt.Errorf("invalid QUIC backhaul address: %w", err)
	}

	conn, err := quic.DialAddr(ctx, address, clientTLSConfig(baseTLS), clientConfig())
	if err != nil {
		return nil, fmt.Errorf("dial QUIC backhaul: %w", err)
	}
	closeWithError := func(code quic.ApplicationErrorCode, reason string) {
		_ = conn.CloseWithError(code, reason)
	}

	stream, err := conn.OpenStreamSync(ctx)
	if err != nil {
		closeWithError(1, "control stream open failed")
		return nil, fmt.Errorf("open QUIC backhaul control stream: %w", err)
	}
	_ = stream.SetDeadline(time.Now().Add(quicBackhaulControlTimeout))
	if err := json.NewEncoder(stream).Encode(controlMessage{AccessToken: accessToken}); err != nil {
		closeWithError(1, "control write failed")
		return nil, fmt.Errorf("write QUIC backhaul control message: %w", err)
	}
	var response controlResponse
	if err := json.NewDecoder(io.LimitReader(stream, quicBackhaulControlBodyLimit)).Decode(&response); err != nil {
		closeWithError(1, "control response read failed")
		return nil, fmt.Errorf("read QUIC backhaul control response: %w", err)
	}
	_ = stream.SetDeadline(time.Time{})
	_ = stream.Close()
	if !response.OK {
		rejection := &rejectionError{code: response.Error}
		closeWithError(1, rejection.Error())
		return nil, rejection
	}
	return &quicDatagramConn{conn: conn}, nil
}

func clientTLSConfig(base *tls.Config) *tls.Config {
	var config *tls.Config
	if base == nil {
		config = &tls.Config{}
	} else {
		config = base.Clone()
	}
	config.NextProtos = []string{quicBackhaulALPN}
	if config.MinVersion < tls.VersionTLS13 {
		config.MinVersion = tls.VersionTLS13
	}
	return config
}

func clientConfig() *quic.Config {
	return &quic.Config{
		EnableDatagrams:    true,
		KeepAlivePeriod:    quicBackhaulKeepAlive,
		MaxIdleTimeout:     quicBackhaulIdleTimeout,
		MaxIncomingStreams: 16,
	}
}
