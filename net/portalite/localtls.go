package portalite

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// ErrLocalCertificate identifies an invalid local tenant certificate configuration
// or a certificate that does not cover the assigned canonical hostname.
var ErrLocalCertificate = errors.New("portalite: invalid local tenant certificate")

const (
	maxTenantCertResponseBytes int64 = 128 << 10
	automaticTLSRequestTimeout       = 2 * time.Minute
)

type localCertPin struct {
	certPEM     []byte
	certificate tls.Certificate
	leaf        *x509.Certificate
}

func validateLocalCertificateFiles(certFile, keyFile string) error {
	if _, err := loadLocalCertPin(certFile, keyFile, ""); err != nil {
		return fmt.Errorf("%w: %w", ErrLocalCertificate, err)
	}
	return nil
}

func loadLocalCertPin(certFile, keyFile, hostname string) (*localCertPin, error) {
	certPEM, err := os.ReadFile(certFile)
	if err != nil {
		return nil, fmt.Errorf("read tenant certificate: %w", err)
	}
	keyPEM, err := os.ReadFile(keyFile)
	if err != nil {
		return nil, fmt.Errorf("read tenant key: %w", err)
	}
	return newLocalCertPin(certPEM, keyPEM, hostname)
}

func newLocalCertPin(certPEM, keyPEM []byte, hostname string) (*localCertPin, error) {
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("load tenant certificate/key: %w", err)
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return nil, err
	}
	roots := x509.NewCertPool()
	intermediates := x509.NewCertPool()
	for i, der := range pair.Certificate {
		cert, err := x509.ParseCertificate(der)
		if err != nil {
			return nil, err
		}
		if i == len(pair.Certificate)-1 {
			roots.AddCert(cert)
		} else if i > 0 {
			intermediates.AddCert(cert)
		}
	}
	if hostname != "" {
		exactDNSName := len(leaf.DNSNames) == 1 && strings.EqualFold(leaf.DNSNames[0], hostname)
		hasOtherNames := len(leaf.IPAddresses) != 0 || len(leaf.EmailAddresses) != 0 || len(leaf.URIs) != 0
		if !exactDNSName || hasOtherNames {
			if err := leaf.VerifyHostname(hostname); err != nil {
				return nil, fmt.Errorf("%w: %w", ErrLocalCertificate, err)
			}
			return nil, fmt.Errorf("%w: certificate must contain only the exact service DNS name %q", ErrLocalCertificate, hostname)
		}
	}
	if _, err := leaf.Verify(x509.VerifyOptions{
		DNSName:       hostname,
		Roots:         roots,
		Intermediates: intermediates,
	}); err != nil {
		return nil, fmt.Errorf("validate tenant certificate: %w", err)
	}
	pair.Leaf = leaf
	return &localCertPin{
		certPEM:     append([]byte(nil), certPEM...),
		certificate: pair,
		leaf:        leaf,
	}, nil
}

type localCertBundle struct {
	Hostname       string `json:"hostname"`
	KeyPEM         string `json:"key_pem"`
	CertificatePEM string `json:"certificate_pem,omitempty"`
}

// localTLSManager owns a locally terminated canonical certificate (manual files
// or automatic CSR issuance via POST /v1/tenant-certificate).
type localTLSManager struct {
	hostname string
	certFile string
	keyFile  string
	stateDir string

	relayURL      string
	controlClient *http.Client
	tokenFunc     func() string
	onReady       func()
	onError       func(error)
	onLeaf        func([]byte)
	mu       sync.RWMutex
	pin      *localCertPin
	lastErr  error
	closed   bool
	cancel   context.CancelFunc
	done     chan struct{}
	reloadMu sync.Mutex
}

func newLocalTLSManager(
	hostname string,
	certFile, keyFile string,
	automatic bool,
	stateDir string,
	relayURL string,
	controlClient *http.Client,
	tokenFunc func() string,
	onReady func(),
	onError func(error),
	onLeaf func([]byte),
) (*localTLSManager, error) {
	m := &localTLSManager{
		hostname:      hostname,
		certFile:      certFile,
		keyFile:       keyFile,
		stateDir:      stateDir,
		relayURL:      relayURL,
		controlClient: controlClient,
		tokenFunc:     tokenFunc,
		onReady:       onReady,
		onError:       onError,
		onLeaf:        onLeaf,
	}
	if certFile != "" {
		pin, err := loadLocalCertPin(certFile, keyFile, hostname)
		if err != nil {
			return nil, err
		}
		m.pin = pin
		if m.onLeaf != nil && pin.leaf != nil {
			m.onLeaf(append([]byte(nil), pin.leaf.Raw...))
		}
		return m, nil
	}
	if automatic {
		if err := os.MkdirAll(stateDir, 0o700); err != nil {
			return nil, fmt.Errorf("create automatic TLS directory: %w", err)
		}
		if bundle, err := m.readBundle(); err == nil && bundle.CertificatePEM != "" {
			if pin, pinErr := newLocalCertPin([]byte(bundle.CertificatePEM), []byte(bundle.KeyPEM), hostname); pinErr == nil {
				m.pin = pin
				if m.onLeaf != nil && pin.leaf != nil {
					m.onLeaf(append([]byte(nil), pin.leaf.Raw...))
				}
			}
		} else if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		workerCtx, cancel := context.WithCancel(context.Background())
		m.cancel = cancel
		m.done = make(chan struct{})
		go m.maintainAutomatic(workerCtx)
	}
	return m, nil
}

func (m *localTLSManager) ready() bool {
	if m == nil {
		return false
	}
	m.mu.RLock()
	pin := m.pin
	m.mu.RUnlock()
	return pin != nil && time.Now().Before(pin.leaf.NotAfter)
}

func (m *localTLSManager) certificateExpiry() time.Time {
	if m == nil {
		return time.Time{}
	}
	m.mu.RLock()
	pin := m.pin
	m.mu.RUnlock()
	if pin == nil {
		return time.Time{}
	}
	return pin.leaf.NotAfter
}

func (m *localTLSManager) tlsConfig() (*tls.Config, error) {
	if m == nil {
		return nil, errors.New("local TLS manager is unavailable")
	}
	m.mu.RLock()
	pin := m.pin
	m.mu.RUnlock()
	if pin == nil {
		return nil, errors.New("local tenant certificate is not ready yet")
	}
	return &tls.Config{
		MinVersion:   tls.VersionTLS12,
		Certificates: []tls.Certificate{pin.certificate},
		NextProtos:   []string{"http/1.1"},
	}, nil
}

func (m *localTLSManager) reloadManual() (bool, error) {
	if m == nil || m.certFile == "" {
		return false, nil
	}
	m.reloadMu.Lock()
	defer m.reloadMu.Unlock()
	next, err := loadLocalCertPin(m.certFile, m.keyFile, m.hostname)
	if err != nil {
		return false, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return false, nil
	}
	if m.pin != nil && bytes.Equal(m.pin.certPEM, next.certPEM) {
		return false, nil
	}
	m.pin = next
	if m.onLeaf != nil && next.leaf != nil {
		m.onLeaf(append([]byte(nil), next.leaf.Raw...))
	}
	return true, nil
}

func (m *localTLSManager) close() error {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	m.closed = true
	cancel := m.cancel
	done := m.done
	m.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if done != nil {
		<-done
	}
	return nil
}

func (m *localTLSManager) bundlePath() string {
	sum := sha256.Sum256([]byte(m.hostname))
	return filepath.Join(m.stateDir, hex.EncodeToString(sum[:])+".json")
}

func (m *localTLSManager) readBundle() (localCertBundle, error) {
	data, err := os.ReadFile(m.bundlePath())
	if err != nil {
		return localCertBundle{}, err
	}
	var bundle localCertBundle
	if err := json.Unmarshal(data, &bundle); err != nil {
		return localCertBundle{}, fmt.Errorf("parse automatic TLS bundle: %w", err)
	}
	if bundle.Hostname != m.hostname || bundle.KeyPEM == "" {
		return localCertBundle{}, errors.New("invalid automatic TLS bundle")
	}
	block, _ := pem.Decode([]byte(bundle.KeyPEM))
	if block == nil {
		return localCertBundle{}, errors.New("invalid automatic TLS key PEM")
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return localCertBundle{}, err
	}
	ecKey, ok := key.(*ecdsa.PrivateKey)
	if !ok || ecKey.Curve != elliptic.P256() {
		return localCertBundle{}, errors.New("automatic TLS key must be ECDSA P-256")
	}
	return bundle, nil
}

func (m *localTLSManager) saveBundle(bundle localCertBundle) error {
	data, err := json.Marshal(bundle)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(m.stateDir, ".tenant-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, m.bundlePath())
}

func (m *localTLSManager) maintainAutomatic(ctx context.Context) {
	defer close(m.done)
	delay := time.Duration(0)
	backoff := 2 * time.Second
	for {
		if delay > 0 {
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
		}
		if ctx.Err() != nil {
			return
		}
		issueCtx, cancel := context.WithTimeout(ctx, automaticTLSRequestTimeout)
		pin, err := m.acquireAutomatic(issueCtx)
		cancel()
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			m.mu.Lock()
			m.lastErr = err
			m.mu.Unlock()
			if m.onError != nil {
				m.onError(err)
			}
			delay = backoff
			var apiErr *APIError
			if errors.As(err, &apiErr) && apiErr.RetryAfter > delay {
				delay = min(apiErr.RetryAfter, time.Hour)
			}
			backoff = min(backoff*2, time.Hour)
			continue
		}
		wasReady := m.ready()
		m.mu.Lock()
		m.pin = pin
		m.lastErr = nil
		m.mu.Unlock()
		if m.onLeaf != nil && pin.leaf != nil {
			m.onLeaf(append([]byte(nil), pin.leaf.Raw...))
		}
		if !wasReady && m.onReady != nil {
			m.onReady()
		}
		backoff = 2 * time.Second
		untilRenewal := time.Until(pin.leaf.NotAfter) - pin.leaf.NotAfter.Sub(pin.leaf.NotBefore)/3
		delay = min(time.Hour, max(time.Second, untilRenewal))
	}
}

func (m *localTLSManager) acquireAutomatic(ctx context.Context) (*localCertPin, error) {
	m.reloadMu.Lock()
	defer m.reloadMu.Unlock()

	bundle, err := m.readBundle()
	if errors.Is(err, os.ErrNotExist) {
		key, genErr := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if genErr != nil {
			return nil, genErr
		}
		der, marshalErr := x509.MarshalPKCS8PrivateKey(key)
		if marshalErr != nil {
			return nil, marshalErr
		}
		bundle = localCertBundle{
			Hostname: m.hostname,
			KeyPEM:   string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})),
		}
		if saveErr := m.saveBundle(bundle); saveErr != nil {
			return nil, saveErr
		}
	} else if err != nil {
		return nil, err
	}

	if bundle.CertificatePEM != "" {
		if pin, pinErr := newLocalCertPin([]byte(bundle.CertificatePEM), []byte(bundle.KeyPEM), m.hostname); pinErr == nil {
			renewBefore := pin.leaf.NotAfter.Sub(pin.leaf.NotBefore) / 3
			if time.Until(pin.leaf.NotAfter) > renewBefore {
				return pin, nil
			}
		}
	}

	block, _ := pem.Decode([]byte(bundle.KeyPEM))
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	csrDER, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		DNSNames: []string{m.hostname},
	}, key)
	if err != nil {
		return nil, err
	}
	reqBody, err := json.Marshal(tenantCertificateRequest{
		CSRPEM: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csrDER})),
	})
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.relayURL+pathTenantCertificate, bytes.NewReader(reqBody))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if m.tokenFunc != nil {
		if token := m.tokenFunc(); token != "" {
			req.Header.Set(accessTokenHeader, token)
		}
	}

	resp, err := m.controlClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, decodeHTTPAPIError(resp)
	}
	payload, err := io.ReadAll(io.LimitReader(resp.Body, maxTenantCertResponseBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(payload)) > maxTenantCertResponseBytes {
		return nil, errors.New("tenant certificate response exceeds limit")
	}
	var envelope apiEnvelope
	if err := json.Unmarshal(payload, &envelope); err != nil {
		return nil, fmt.Errorf("decode tenant certificate envelope: %w", err)
	}
	if !envelope.OK {
		apiErr := &APIError{StatusCode: resp.StatusCode, RetryAfter: parseRetryAfter(resp)}
		if envelope.Error != nil {
			apiErr.Code = envelope.Error.Code
			apiErr.Message = envelope.Error.Message
		}
		return nil, apiErr
	}
	var certResp tenantCertificateResponse
	if err := json.Unmarshal(envelope.Data, &certResp); err != nil {
		return nil, fmt.Errorf("decode tenant certificate response: %w", err)
	}
	if strings.TrimSpace(certResp.CertificatePEM) == "" {
		return nil, errors.New("relay returned empty tenant certificate")
	}
	pin, err := newLocalCertPin([]byte(certResp.CertificatePEM), []byte(bundle.KeyPEM), m.hostname)
	if err != nil {
		return nil, err
	}
	bundle.CertificatePEM = certResp.CertificatePEM
	if err := m.saveBundle(bundle); err != nil {
		return nil, err
	}
	return pin, nil
}

// peekClientHelloSNI peeks the TLS ClientHello SNI without consuming bytes
// from the returned net.Conn, allowing dual-host dispatch between Local TLS
// (canonical hostname) and Keyless TLS (friendly alias).
func peekClientHelloSNI(conn net.Conn, timeout time.Duration) (string, net.Conn, error) {
	if err := conn.SetReadDeadline(time.Now().Add(timeout)); err != nil {
		return "", conn, err
	}
	defer func() { _ = conn.SetReadDeadline(time.Time{}) }()

	var buf bytes.Buffer
	var sni string
	errSentinel := errors.New("sni peek complete")
	peekConn := &sniPeekConn{Conn: conn, buf: &buf}
	server := tls.Server(peekConn, &tls.Config{
		GetConfigForClient: func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
			sni = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(hello.ServerName), "."))
			return nil, errSentinel
		},
	})
	err := server.Handshake()
	if !errors.Is(err, errSentinel) && err != nil {
		return "", &bufferedNetConn{Conn: conn, reader: bufio.NewReader(io.MultiReader(bytes.NewReader(buf.Bytes()), conn))}, err
	}
	replayed := &bufferedNetConn{
		Conn:   conn,
		reader: bufio.NewReader(io.MultiReader(bytes.NewReader(buf.Bytes()), conn)),
	}
	return sni, replayed, nil
}

type sniPeekConn struct {
	net.Conn
	buf *bytes.Buffer
}

func (c *sniPeekConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if n > 0 {
		_, _ = c.buf.Write(p[:n])
	}
	return n, err
}

func (c *sniPeekConn) Write(p []byte) (int, error) {
	return 0, io.ErrClosedPipe
}
