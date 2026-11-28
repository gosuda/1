// Package keyless provides a portalite.KeylessProvider backed by
// github.com/gosuda/keyless_tls for friendly alias TLS termination.
package keyless

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"

	kls "github.com/gosuda/keyless_tls/keyless"
	"github.com/gosuda/keyless_tls/keyless/t13server"
	"github.com/gosuda/keyless_tls/relay/signrpc"

	"gosuda.org/portalite"
)

type provider struct{}

// NewProvider returns a KeylessProvider that terminates tenant TLS 1.3 through
// github.com/gosuda/keyless_tls and the relay's transcript-bound /v1/sign endpoint.
func NewProvider() portalite.KeylessProvider {
	return provider{}
}

func (provider) NewTerminator(cfg portalite.KeylessConfig) (portalite.KeylessTerminator, error) {
	headerName := cfg.AccessTokenHeader
	if headerName == "" {
		headerName = "X-Portal-Access-Token"
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

	signer := &leaseTranscriptSigner{signer: remoteSigner}
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
	return &terminator{
		server: server,
		signer: signer,
	}, nil
}

type signerError struct {
	err error
}

func (e *signerError) Error() string { return e.err.Error() }
func (e *signerError) Unwrap() error { return e.err }

type terminator struct {
	server *t13server.Server
	signer *leaseTranscriptSigner
}

func (t *terminator) Handshake(ctx context.Context, conn net.Conn, binding []byte) (net.Conn, error) {
	tlsConn := t.server.NewConn(conn, binding)
	if err := tlsConn.HandshakeContext(ctx); err != nil {
		if signerErr := t.signer.takeError(binding); signerErr != nil {
			return nil, &signerError{err: signerErr}
		}
		return nil, fmt.Errorf("tenant TLS handshake: %w", err)
	}
	return tlsConn, nil
}

func (t *terminator) IsSignerError(err error) bool {
	var target *signerError
	return errors.As(err, &target)
}

func (t *terminator) Close() error {
	if t == nil || t.signer == nil || t.signer.signer == nil {
		return nil
	}
	return t.signer.signer.Close()
}

type leaseTranscriptSigner struct {
	signer *kls.RemoteSigner

	mu   sync.Mutex
	errs map[string]error
}

func (s *leaseTranscriptSigner) SignTranscript(ctx context.Context, request *signrpc.TranscriptSignRequest) (*signrpc.TranscriptSignResponse, error) {
	response, err := s.signer.SignTranscript(ctx, request)
	if err != nil && ctx.Err() == nil {
		s.mu.Lock()
		if s.errs == nil {
			s.errs = make(map[string]error)
		}
		binding := string(request.Binding)
		if _, exists := s.errs[binding]; !exists {
			s.errs[binding] = err
		}
		s.mu.Unlock()
	}
	return response, err
}

func (s *leaseTranscriptSigner) takeError(binding []byte) error {
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
