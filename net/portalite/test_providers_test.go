package portalite

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	kls "github.com/gosuda/keyless_tls/keyless"
	"github.com/gosuda/keyless_tls/keyless/t13server"
	"github.com/gosuda/keyless_tls/relay/signrpc"
	"github.com/quic-go/quic-go"
)

const (
	quicBackhaulALPN             = "portal-tunnel"
	quicBackhaulControlBodyLimit = 4096
)

type quicBackhaulControlMessage struct {
	AccessToken string `json:"access_token"`
}

type quicBackhaulControlResponse struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

func quicClientConfig() *quic.Config {
	return &quic.Config{
		EnableDatagrams:    true,
		KeepAlivePeriod:    15 * time.Second,
		MaxIdleTimeout:     60 * time.Second,
		MaxIncomingStreams: 16,
	}
}

// testKeylessProvider mirrors gosuda.org/portalite/keyless for white-box tests
// inside package portalite (avoiding an import cycle while keeping production
// package portalite free of keyless_tls).
type testKeylessProvider struct{}

func (testKeylessProvider) NewTerminator(cfg KeylessConfig) (KeylessTerminator, error) {
	headerName := cfg.AccessTokenHeader
	if headerName == "" {
		headerName = accessTokenHeader
	}
	remoteSigner, err := kls.NewRemoteSigner(kls.RemoteSignerConfig{
		Endpoint:   cfg.RelayURL,
		ServerName: cfg.ServerName,
		KeyID:      "relay-cert",
		RootCAPEM:  append([]byte(nil), cfg.ChainPEM...),
		Timeout:    cfg.RequestTimeout,
		Headers: func() http.Header {
			header := make(http.Header, 1)
			if cfg.AccessToken != nil {
				if token := cfg.AccessToken(); token != "" {
					header.Set(headerName, token)
				}
			}
			return header
		},
	})
	if err != nil {
		return nil, err
	}
	signer := &testTranscriptSigner{signer: remoteSigner}
	server, err := t13server.NewServer(t13server.Config{
		CertPEM:          append([]byte(nil), cfg.ChainPEM...),
		NextProtos:       []string{"http/1.1"},
		KeyID:            "relay-cert",
		TranscriptSigner: signer,
	})
	if err != nil {
		_ = remoteSigner.Close()
		return nil, err
	}
	return &testKeylessTerminator{server: server, signer: signer}, nil
}

type testSignerErr struct{ err error }

func (e *testSignerErr) Error() string { return e.err.Error() }
func (e *testSignerErr) Unwrap() error { return e.err }

type testKeylessTerminator struct {
	server *t13server.Server
	signer *testTranscriptSigner
}

func (t *testKeylessTerminator) Handshake(ctx context.Context, conn net.Conn, binding []byte) (net.Conn, error) {
	tlsConn := t.server.NewConn(conn, binding)
	if err := tlsConn.HandshakeContext(ctx); err != nil {
		if signerErr := t.signer.takeError(binding); signerErr != nil {
			return nil, &testSignerErr{err: signerErr}
		}
		return nil, fmt.Errorf("tenant TLS handshake: %w", err)
	}
	return tlsConn, nil
}

func (t *testKeylessTerminator) IsSignerError(err error) bool {
	var target *testSignerErr
	return errors.As(err, &target)
}

func (t *testKeylessTerminator) Close() error {
	if t == nil || t.signer == nil || t.signer.signer == nil {
		return nil
	}
	return t.signer.signer.Close()
}

type testTranscriptSigner struct {
	signer *kls.RemoteSigner
	mu     sync.Mutex
	errs   map[string]error
}

func (s *testTranscriptSigner) SignTranscript(ctx context.Context, req *signrpc.TranscriptSignRequest) (*signrpc.TranscriptSignResponse, error) {
	resp, err := s.signer.SignTranscript(ctx, req)
	if err != nil && ctx.Err() == nil {
		s.mu.Lock()
		if s.errs == nil {
			s.errs = make(map[string]error)
		}
		key := string(req.Binding)
		if _, exists := s.errs[key]; !exists {
			s.errs[key] = err
		}
		s.mu.Unlock()
	}
	return resp, err
}

func (s *testTranscriptSigner) takeError(binding []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.errs == nil {
		return nil
	}
	key := string(binding)
	err := s.errs[key]
	delete(s.errs, key)
	return err
}

// testDatagramDialer mirrors gosuda.org/portalite/udp for white-box tests.
type testDatagramDialer struct{}

type testDatagramConn struct{ conn *quic.Conn }

func (c *testDatagramConn) SendDatagram(p []byte) error               { return c.conn.SendDatagram(p) }
func (c *testDatagramConn) ReceiveDatagram(ctx context.Context) ([]byte, error) { return c.conn.ReceiveDatagram(ctx) }
func (c *testDatagramConn) CloseWithError(code uint64, reason string) error {
	return c.conn.CloseWithError(quic.ApplicationErrorCode(code), reason)
}

type testDatagramRejection struct{ code string }

func (e *testDatagramRejection) Error() string        { return "QUIC backhaul rejected: " + e.code }
func (e *testDatagramRejection) RejectionCode() string { return e.code }

func (testDatagramDialer) Dial(ctx context.Context, addr string, baseTLS *tls.Config, accessToken string) (DatagramConn, error) {
	var cfg *tls.Config
	if baseTLS == nil {
		cfg = &tls.Config{}
	} else {
		cfg = baseTLS.Clone()
	}
	cfg.NextProtos = []string{"portal-tunnel"}
	if cfg.MinVersion < tls.VersionTLS13 {
		cfg.MinVersion = tls.VersionTLS13
	}
	conn, err := quic.DialAddr(ctx, addr, cfg, &quic.Config{
		EnableDatagrams:    true,
		KeepAlivePeriod:    15 * time.Second,
		MaxIdleTimeout:     60 * time.Second,
		MaxIncomingStreams: 16,
	})
	if err != nil {
		return nil, err
	}
	stream, err := conn.OpenStreamSync(ctx)
	if err != nil {
		_ = conn.CloseWithError(1, "control stream open failed")
		return nil, err
	}
	_ = stream.SetDeadline(time.Now().Add(10 * time.Second))
	if err := json.NewEncoder(stream).Encode(struct {
		AccessToken string `json:"access_token"`
	}{AccessToken: accessToken}); err != nil {
		_ = conn.CloseWithError(1, "control write failed")
		return nil, err
	}
	var resp struct {
		OK    bool   `json:"ok"`
		Error string `json:"error,omitempty"`
	}
	if err := json.NewDecoder(io.LimitReader(stream, 4096)).Decode(&resp); err != nil {
		_ = conn.CloseWithError(1, "control response read failed")
		return nil, err
	}
	_ = stream.SetDeadline(time.Time{})
	_ = stream.Close()
	if !resp.OK {
		rej := &testDatagramRejection{code: strings.TrimSpace(resp.Error)}
		_ = conn.CloseWithError(1, rej.Error())
		return nil, rej
	}
	return &testDatagramConn{conn: conn}, nil
}

func exposeWithTestProviders(ctx context.Context, cfg ExposeConfig, timings relayTimings) (*Exposure, error) {
	useLocal := cfg.AutomaticTLS || cfg.TLSCertFile != "" || cfg.TLSKeyFile != ""
	if cfg.Keyless == nil && !cfg.TCPEnabled && !useLocal {
		cfg.Keyless = testKeylessProvider{}
	}
	if cfg.UDPEnabled && cfg.Datagram == nil {
		cfg.Datagram = testDatagramDialer{}
	}
	return exposeWithTimings(ctx, cfg, timings)
}
