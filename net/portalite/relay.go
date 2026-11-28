package portalite

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

const reverseSessionSlots = 2

const maxReverseResponseHeadBytes int64 = 1 << 20

// maxRelayRetryDelay bounds a relay-advertised Retry-After. A relay is
// entitled to throttle, but one relay must not park its supervisor for an
// unbounded time; past this bound the supervisor retries on its own schedule.
const maxRelayRetryDelay = 5 * time.Minute

// defaultChainRefreshInterval is how often a lease checks for a rotated relay
// certificate chain during renewals.
const defaultChainRefreshInterval = 10 * time.Minute

type relayReadyInfo struct {
	publicURL  string
	aliasURL   string
	aliasEpoch string
	tcpAddr    string
}
// parseRetryAfter reads the relay's Retry-After header, accepting both the
// delta-seconds and the HTTP-date forms. It returns zero when absent or
// unusable.
func parseRetryAfter(response *http.Response) time.Duration {
	if response == nil {
		return 0
	}
	raw := strings.TrimSpace(response.Header.Get("Retry-After"))
	if raw == "" {
		return 0
	}
	if seconds, err := strconv.ParseUint(raw, 10, 31); err == nil {
		return time.Duration(seconds) * time.Second
	}
	if at, err := http.ParseTime(raw); err == nil {
		if delay := time.Until(at); delay > 0 {
			return delay
		}
	}
	return 0
}

// relayRetryDelay returns the backoff to wait after err: the configured base
// delay, raised to the relay's advertised Retry-After when that is larger and
// bounded by maxRelayRetryDelay.
func relayRetryDelay(base time.Duration, err error) time.Duration {
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.RetryAfter <= base {
		return base
	}
	if apiErr.RetryAfter > maxRelayRetryDelay {
		return maxRelayRetryDelay
	}
	return apiErr.RetryAfter
}

type relayTimings struct {
	leaseTTL         time.Duration
	renewBefore      time.Duration
	retryWait        time.Duration
	dialTimeout      time.Duration
	handshakeTimeout time.Duration
	requestTimeout   time.Duration
	idleTimeout      time.Duration
	shutdownTimeout  time.Duration
	discoveryPoll    time.Duration
}

func defaultRelayTimings() relayTimings {
	return relayTimings{
		leaseTTL:         2 * time.Minute,
		renewBefore:      30 * time.Second,
		retryWait:        3 * time.Second,
		dialTimeout:      15 * time.Second,
		handshakeTimeout: 30 * time.Second,
		requestTimeout:   30 * time.Second,
		idleTimeout:      60 * time.Second,
		shutdownTimeout:  5 * time.Second,
		discoveryPoll:    discoveryPollInterval,
	}
}

func (t relayTimings) withDefaults() relayTimings {
	d := defaultRelayTimings()
	if t.leaseTTL <= 0 {
		t.leaseTTL = d.leaseTTL
	}
	if t.renewBefore <= 0 {
		t.renewBefore = d.renewBefore
	}
	if t.retryWait <= 0 {
		t.retryWait = d.retryWait
	}
	if t.dialTimeout <= 0 {
		t.dialTimeout = d.dialTimeout
	}
	if t.handshakeTimeout <= 0 {
		t.handshakeTimeout = d.handshakeTimeout
	}
	if t.requestTimeout <= 0 {
		t.requestTimeout = d.requestTimeout
	}
	if t.idleTimeout <= 0 {
		t.idleTimeout = d.idleTimeout
	}
	if t.shutdownTimeout <= 0 {
		t.shutdownTimeout = d.shutdownTimeout
	}
	if t.discoveryPoll <= 0 {
		t.discoveryPoll = d.discoveryPoll
	}

	return t
}

func (t relayTimings) ttlSeconds() int {
	seconds := int((t.leaseTTL + time.Second - 1) / time.Second)
	if seconds < 1 {
		return 1
	}
	return seconds
}

// tenantTLS is the protocol-10 tenant TLS terminator for one lease: an optional
// KeylessTerminator (for friendly aliases) and/or a localTLSManager
// (for local-tls-v1 identity-bound canonical hostnames).
type tenantTLS struct {
	keyless           KeylessTerminator
	local             *localTLSManager
	observer          CertObserver
	canonicalHostname string
}

func (t *tenantTLS) close() error {
	if t == nil {
		return nil
	}
	var errs []error
	if t.observer != nil {
		t.observer.Close()
	}
	if t.keyless != nil {
		if err := t.keyless.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	if t.local != nil {
		if err := t.local.close(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
type relayLease struct {
	token             string
	expiresAt         time.Time
	publicURL         string
	aliasURL          string
	aliasEpoch        string
	canonicalHostname string
	aliasHostname     string
	udpAddr           string
	tcpAddr           string
	reverse           reverseEndpoint
	tenant            *tenantTLS
	generation        uint64
	chainCheckedAt    time.Time
	renewing          bool
	tokenUncertain    bool
}

type relaySupervisor struct {
	relayURL        string
	parsedRelayURL  *url.URL
	authority       string
	hostname        string
	relayPort       string
	dialAddress     string
	publicHostname  string
	identity        Identity
	timings         relayTimings
	udpEnabled      bool
	tcpEnabled      bool
	replica         bool
	tlsCertFile     string
	tlsKeyFile      string
	automaticTLS    bool
	tlsStateDir     string
	tlsRequireLocal bool
	keyless         KeylessProvider
	datagram        DatagramDialer
	certObserver    CertObserverFactory
	domainReplicas         bool
	domainNamespaces       []string
	domainIssuanceDisabled bool

	controlTLS       *tls.Config
	certificateChain []byte
	controlTransport *http.Transport
	controlClient    *http.Client
	chainRefreshMu   sync.Mutex

	leaseMu             sync.RWMutex
	lease               relayLease
	nextLeaseGeneration uint64

	datagramMu   sync.RWMutex
	datagramConn DatagramConn
}

func (s *relaySupervisor) useLocalTLS() bool {
	return s.automaticTLS || s.tlsCertFile != ""
}

type terminalRelayError struct{ err error }

func (e *terminalRelayError) Error() string { return e.err.Error() }
func (e *terminalRelayError) Unwrap() error { return e.err }

var errLeaseRefresh = errors.New("relay lease must be refreshed")

type malformedRelayResponseError struct{ err error }

var errControlResponseTooLarge = errors.New("relay control response body exceeds limit")

func (e *malformedRelayResponseError) Error() string { return e.err.Error() }
func (e *malformedRelayResponseError) Unwrap() error { return e.err }

var errReverseResponseHeadTooLarge = errors.New("reverse upgrade response head exceeds 1 MiB")

type reverseUpgradeError struct {
	capability string
	generation uint64
	err        error
}

func (e *reverseUpgradeError) Error() string { return e.err.Error() }
func (e *reverseUpgradeError) Unwrap() error { return e.err }

type tenantSignerError struct {
	err        error
	generation uint64
}

func (e *tenantSignerError) Error() string { return fmt.Sprintf("tenant TLS signer: %v", e.err) }
func (e *tenantSignerError) Unwrap() error { return e.err }

type reverseResponseHeadReader struct {
	reader    io.Reader
	remaining int64
	bounded   bool
}

func (r *reverseResponseHeadReader) Read(p []byte) (int, error) {
	if !r.bounded {
		return r.reader.Read(p)
	}
	if r.remaining == 0 {
		return 0, errReverseResponseHeadTooLarge
	}
	if int64(len(p)) > r.remaining {
		p = p[:r.remaining]
	}
	n, err := r.reader.Read(p)
	r.remaining -= int64(n)
	return n, err
}

func (r *reverseResponseHeadReader) disableBound() { r.bounded = false }

func newRelaySupervisor(relayURL string, identity Identity, timings relayTimings) (*relaySupervisor, error) {
	parsed, err := url.Parse(relayURL)
	if err != nil {
		return nil, fmt.Errorf("parse relay URL: %w", err)
	}
	if parsed.Host == "" || parsed.Hostname() == "" {
		return nil, errors.New("relay URL has no hostname")
	}

	port := parsed.Port()
	if port == "" {
		port = "443"
	}
	return &relaySupervisor{
		relayURL:       relayURL,
		parsedRelayURL: parsed,
		authority:      parsed.Host,
		hostname:       parsed.Hostname(),
		relayPort:      parsed.Port(),
		dialAddress:    net.JoinHostPort(parsed.Hostname(), port),
		identity:       identity,
		timings:        timings.withDefaults(),
	}, nil
}

// run owns the complete lifecycle of one relay. Its two results deliberately
// separate a terminal relay failure from best-effort shutdown failures.
func (s *relaySupervisor) run(
	ctx context.Context,
	ready func(relayReadyInfo),
	datagramReady func(string),
	approval func(RelayApproval),
	certErr func(error),
	offer func(context.Context, net.Conn) bool,
	offerDatagram func(context.Context, DatagramFrame) bool,
) (runErr, cleanupErr error) {
	defer func() {
		cleanupErr = s.shutdown()
	}()

	for {
		if err := ctx.Err(); err != nil {
			return err, nil
		}

		if err := s.initializeControl(ctx); err != nil {
			s.resetControl()
			if ctx.Err() != nil {
				return ctx.Err(), nil
			}
			if isTerminalRelayFailure(err) {
				return unwrapTerminalRelayError(err), nil
			}
			if !waitForRelayRetry(ctx, relayRetryDelay(s.timings.retryWait, err)) {
				return ctx.Err(), nil
			}
			continue
		}

		if err := s.registerLeaseWithRetry(ctx, ready, approval, certErr); err != nil {
			if ctx.Err() != nil {
				return ctx.Err(), nil
			}
			if isTerminalRelayFailure(err) {
				return unwrapTerminalRelayError(err), nil
			}
			return err, nil
		}

		err := s.runLease(ctx, ready, datagramReady, approval, offer, offerDatagram)
		if ctx.Err() != nil {
			return ctx.Err(), nil
		}
		if errors.Is(err, errLeaseRefresh) {
			s.discardLostLease()
			s.resetControl()
			continue
		}
		if isTerminalRelayFailure(err) {
			return unwrapTerminalRelayError(err), nil
		}
		if err != nil {
			return err, nil
		}
	}
}

func (s *relaySupervisor) initializeControl(ctx context.Context) error {
	tlsConfig, chain, leaf, err := s.bootstrapTLS(ctx)
	if err != nil {
		return classifyRelayError(err, false)
	}

	transport := &http.Transport{
		Proxy:               http.ProxyFromEnvironment,
		DialContext:         (&net.Dialer{Timeout: s.timings.dialTimeout, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:   false,
		TLSClientConfig:     tlsConfig,
		TLSHandshakeTimeout: s.timings.handshakeTimeout,
		TLSNextProto:        make(map[string]func(string, *tls.Conn) http.RoundTripper),
	}
	client := &http.Client{
		Transport: transport,
		Timeout:   s.timings.requestTimeout,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	s.controlTLS = tlsConfig
	s.certificateChain = chain
	s.controlTransport = transport
	s.controlClient = client

	var domain domainResponse
	if err := s.doAPI(ctx, http.MethodGet, pathDomain, nil, &domain); err != nil {
		return classifyRelayError(err, false)
	}
	if !domain.supportsProtocol(ProtocolVersion) {
		return terminalRelay(fmt.Errorf("unsupported relay protocol version %q (supported=%v, want %q)", domain.ProtocolVersion, domain.SupportedProtocolVersions, ProtocolVersion))
	}
	s.domainReplicas = domain.Replicas
	s.domainNamespaces = slices.Clone(domain.Namespaces)
	s.domainIssuanceDisabled = domain.TenantCertificateIssuance != nil && !*domain.TenantCertificateIssuance
	if s.replica && !s.domainReplicas {
		return terminalRelay(errors.New("relay does not support HTTP replicas"))
	}
	if s.useLocalTLS() && !slices.Contains(s.domainNamespaces, LocalTLSNamespaceV1) {
		return terminalRelay(fmt.Errorf("relay does not support namespace %q", LocalTLSNamespaceV1))
	}

	if !s.useLocalTLS() && !s.tcpEnabled && s.identity.Name() != "" {
		s.publicHostname = s.identity.Name() + "." + s.hostname
		if err := leaf.VerifyHostname(s.publicHostname); err != nil {
			return terminalRelay(fmt.Errorf("relay certificate does not cover public hostname %q: %w", s.publicHostname, err))
		}
	}
	return nil
}

func (s *relaySupervisor) bootstrapTLS(ctx context.Context) (*tls.Config, []byte, *x509.Certificate, error) {
	dialer := &net.Dialer{Timeout: s.timings.dialTimeout}
	raw, err := dialer.DialContext(ctx, "tcp", s.dialAddress)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("dial relay: %w", err)
	}
	defer raw.Close()

	local := isLocalRelayHostname(s.hostname)
	fetchConfig := &tls.Config{
		MinVersion: tls.VersionTLS12,
		ServerName: s.hostname,
		NextProtos: []string{"http/1.1"},
		// A local development relay is trusted only for this initial fetch;
		// its exact presented chain becomes the root set for every later call.
		InsecureSkipVerify: local, //nolint:gosec
	}
	conn := tls.Client(raw, fetchConfig)
	handshakeCtx, cancel := context.WithTimeout(ctx, s.timings.handshakeTimeout)
	err = conn.HandshakeContext(handshakeCtx)
	cancel()
	if err != nil {
		if isCertificateValidationError(err) {
			return nil, nil, nil, terminalRelay(fmt.Errorf("verify relay certificate: %w", err))
		}
		return nil, nil, nil, fmt.Errorf("TLS handshake with relay: %w", err)
	}

	peerCertificates := conn.ConnectionState().PeerCertificates
	if len(peerCertificates) == 0 {
		return nil, nil, nil, terminalRelay(errors.New("relay returned no certificates"))
	}
	var chain bytes.Buffer
	for _, certificate := range peerCertificates {
		if err := pem.Encode(&chain, &pem.Block{Type: "CERTIFICATE", Bytes: certificate.Raw}); err != nil {
			return nil, nil, nil, terminalRelay(fmt.Errorf("encode relay certificate: %w", err))
		}
	}

	controlConfig := &tls.Config{
		MinVersion: tls.VersionTLS12,
		ServerName: s.hostname,
		NextProtos: []string{"http/1.1"},
	}
	if local {
		roots := x509.NewCertPool()
		if !roots.AppendCertsFromPEM(chain.Bytes()) {
			return nil, nil, nil, terminalRelay(errors.New("build local relay certificate pool"))
		}
		intermediates := x509.NewCertPool()
		for _, certificate := range peerCertificates[1:] {
			intermediates.AddCert(certificate)
		}
		if _, err := peerCertificates[0].Verify(x509.VerifyOptions{
			DNSName:       s.hostname,
			Roots:         roots,
			Intermediates: intermediates,
		}); err != nil {
			return nil, nil, nil, terminalRelay(fmt.Errorf("verify pinned local relay certificate: %w", err))
		}
		controlConfig.RootCAs = roots
	}

	return controlConfig, append([]byte(nil), chain.Bytes()...), peerCertificates[0], nil
}

func isLocalRelayHostname(hostname string) bool {
	if strings.EqualFold(strings.TrimSuffix(hostname, "."), "localhost") {
		return true
	}
	ip := net.ParseIP(hostname)
	return ip != nil && ip.IsLoopback()
}

// registerAttempt is one signed registration challenge. It is owned by the
// retry loop that created it, so a signed challenge can never outlive the
// identity or relay generation it was signed for.
type registerAttempt struct {
	challengeID string
	siweMessage string
	signature   string
	expiresAt   time.Time
}

func (a *registerAttempt) valid(now time.Time) bool {
	return a != nil && a.challengeID != "" && now.Before(a.expiresAt)
}

// registerLeaseWithRetry registers the lease, retrying transient failures.
// The signed challenge stays in this loop's scope: a throttled relay is never
// charged for a second challenge, and a challenge signed for one identity can
// never be reused after an identity or relay generation change.
func (s *relaySupervisor) registerLeaseWithRetry(
	ctx context.Context,
	ready func(relayReadyInfo),
	approval func(RelayApproval),
	certErr func(error),
) error {
	var attempt *registerAttempt
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !attempt.valid(time.Now()) {
			fresh, err := s.requestRegisterChallenge(ctx)
			if err != nil {
				if app := relayApprovalFromError(err); app != "" && approval != nil {
					approval(app)
				}
				if isTerminalRelayFailure(err) {
					return err
				}
				if !waitForRelayRetry(ctx, relayRetryDelay(s.timings.retryWait, err)) {
					return ctx.Err()
				}
				continue
			}
			attempt = fresh
		}
		err := s.submitRegistration(ctx, attempt, ready, certErr)
		if err == nil {
			return nil
		}
		if app := relayApprovalFromError(err); app != "" && approval != nil {
			approval(app)
		}
		if isTerminalRelayFailure(err) {
			return err
		}
		if !waitForRelayRetry(ctx, relayRetryDelay(s.timings.retryWait, err)) {
			return ctx.Err()
		}
	}
}

// requestRegisterChallenge obtains, validates, and signs a fresh registration challenge.
func (s *relaySupervisor) requestRegisterChallenge(ctx context.Context) (*registerAttempt, error) {
	requestIdentity := identityRef{Name: s.identity.Name(), Address: s.identity.Address()}
	req := challengeRequest{
		ProtocolVersion: ProtocolVersion,
		Replica:         s.replica,
		Identity:        requestIdentity,
		TTL:             s.timings.ttlSeconds(),
		UDPEnabled:      s.udpEnabled,
		TCPEnabled:      s.tcpEnabled,
	}
	if s.useLocalTLS() {
		req.Namespace = LocalTLSNamespaceV1
	}
	var challenge challengeResponse
	if err := s.doAPI(ctx, http.MethodPost, pathRegisterChallenge, req, &challenge); err != nil {
		return nil, classifyRelayError(err, false)
	}
	if s.replica && !challenge.Replica {
		return nil, terminalRelay(errors.New("relay did not confirm replica registration"))
	}
	if strings.TrimSpace(challenge.ChallengeID) == "" {
		return nil, terminalRelay(errors.New("register challenge has an empty challenge_id"))
	}
	if challenge.SIWEMessage == "" {
		return nil, terminalRelay(errors.New("register challenge has an empty siwe_message"))
	}
	if !challenge.ExpiresAt.After(time.Now()) {
		return nil, terminalRelay(errors.New("register challenge is expired"))
	}
	if err := validateRegisterSIWEChallenge(challenge, s.parsedRelayURL, s.identity.Address(), time.Now()); err != nil {
		return nil, terminalRelay(err)
	}

	signature, err := s.identity.signEthereumPersonalMessage(challenge.SIWEMessage)
	if err != nil {
		return nil, terminalRelay(fmt.Errorf("sign register challenge: %w", err))
	}
	return &registerAttempt{
		challengeID: challenge.ChallengeID,
		siweMessage: challenge.SIWEMessage,
		signature:   signature,
		expiresAt:   challenge.ExpiresAt,
	}, nil
}

// submitRegistration exchanges one signed challenge for a lease and installs it.
func (s *relaySupervisor) submitRegistration(
	ctx context.Context,
	attempt *registerAttempt,
	ready func(relayReadyInfo),
	certErr func(error),
) error {
	requestIdentity := identityRef{Name: s.identity.Name(), Address: s.identity.Address()}
	requestedNamespace := ""
	if s.useLocalTLS() {
		requestedNamespace = LocalTLSNamespaceV1
	}
	var response registerResponse
	if err := s.doAPI(ctx, http.MethodPost, pathRegister, registerRequest{
		ChallengeID:   attempt.challengeID,
		SIWEMessage:   attempt.siweMessage,
		SIWESignature: attempt.signature,
	}, &response); err != nil {
		return classifyRelayError(err, false)
	}

	responseToken := strings.TrimSpace(response.AccessToken)
	invalid := func(err error) error {
		if responseToken != "" {
			unregisterCtx, cancel := context.WithTimeout(context.Background(), s.timings.shutdownTimeout)
			unregisterErr := s.unregisterToken(unregisterCtx, responseToken)
			cancel()
			if unregisterErr != nil {
				err = errors.Join(err, fmt.Errorf("unregister invalid lease: %w", unregisterErr))
			}
		}
		return terminalRelay(err)
	}
	if responseToken == "" {
		return invalid(errors.New("register response has an empty access_token"))
	}
	if !response.ExpiresAt.After(time.Now()) {
		return invalid(errors.New("register response lease is expired"))
	}
	reverse, err := s.validateReverseEndpoint(response.ReverseEndpoint, response.ExpiresAt)
	if err != nil {
		return invalid(err)
	}
	var responseName string
	if strings.TrimSpace(response.Identity.Name) != "" {
		responseName, err = normalizeIdentityName(response.Identity.Name)
		if err != nil {
			return invalid(fmt.Errorf("register response has invalid identity name: %w", err))
		}
	}
	responseAddress, err := normalizeEthereumAddress(response.Identity.Address)
	if err != nil {
		return invalid(fmt.Errorf("register response has invalid identity address: %w", err))
	}
	if responseName != requestIdentity.Name || responseAddress != requestIdentity.Address {
		return invalid(errors.New("register response identity does not match requested identity"))
	}
	if response.Namespace != requestedNamespace {
		return invalid(fmt.Errorf("register response namespace %q does not match requested %q", response.Namespace, requestedNamespace))
	}

	canonicalHost := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(response.CanonicalHostname), "."))
	if requestedNamespace == LocalTLSNamespaceV1 {
		expectedCanonical, err := LocalTLSHostname(s.identity.Name(), s.identity.Address(), s.hostname)
		if err != nil || canonicalHost != expectedCanonical {
			return invalid(fmt.Errorf("register response canonical_hostname %q does not match expected %q", canonicalHost, expectedCanonical))
		}
	} else {
		canonicalHost = ""
	}

	aliasHost := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(response.Hostname), "."))
	if aliasHost != "" && !matchesAlias(aliasHost, s.identity.Name(), s.hostname) {
		aliasHost = ""
	}
	if requestedNamespace == "" && !s.tcpEnabled && !s.udpEnabled && aliasHost == "" {
		return invalid(errors.New("register response did not grant a public hostname"))
	}

	udpAddr := ""
	if s.udpEnabled {
		if !response.UDPEnabled {
			return invalid(errors.New("register response did not enable UDP"))
		}
		udpAddr, err = NormalizeTarget(response.UDPAddr)
		if err != nil {
			return invalid(fmt.Errorf("register response has invalid udp_addr: %w", err))
		}
	}
	tcpAddr := ""
	if s.tcpEnabled {
		if !response.TCPEnabled {
			return invalid(errors.New("register response did not enable TCP"))
		}
		tcpAddr, err = NormalizeTarget(response.TCPAddr)
		if err != nil {
			return invalid(fmt.Errorf("register response has invalid tcp_addr: %w", err))
		}
	}

	aliasURL := s.urlForHostname(aliasHost)
	publicURL := aliasURL
	if canonicalHost != "" {
		publicURL = s.urlForHostname(canonicalHost)
	}

	s.leaseMu.Lock()
	s.nextLeaseGeneration++
	generation := s.nextLeaseGeneration
	s.leaseMu.Unlock()

	onLocalReady := func() {
		if ready != nil {
			ready(s.currentReadyInfo())
		}
	}
	tenant, err := s.buildTenantTLS(responseToken, canonicalHost, aliasHost, onLocalReady, certErr)
	if err != nil {
		unregisterCtx, cancel := context.WithTimeout(context.Background(), s.timings.shutdownTimeout)
		unregisterErr := s.unregisterToken(unregisterCtx, responseToken)
		cancel()
		if unregisterErr != nil {
			err = errors.Join(err, fmt.Errorf("unregister unusable lease: %w", unregisterErr))
		}
		return terminalRelay(fmt.Errorf("configure tenant TLS: %w", err))
	}

	s.leaseMu.Lock()
	s.lease = relayLease{
		token:             responseToken,
		expiresAt:         response.ExpiresAt,
		publicURL:         publicURL,
		aliasURL:          aliasURL,
		aliasEpoch:        strings.TrimSpace(response.AliasEpoch),
		canonicalHostname: canonicalHost,
		aliasHostname:     aliasHost,
		udpAddr:           udpAddr,
		tcpAddr:           tcpAddr,
		reverse:           reverse,
		tenant:            tenant,
		generation:        generation,
		chainCheckedAt:    time.Now(),
	}
	s.leaseMu.Unlock()
	return nil
}

func (s *relaySupervisor) urlForHostname(host string) string {
	if host == "" {
		return ""
	}
	if s.relayPort != "" && s.relayPort != "443" {
		return "https://" + net.JoinHostPort(host, s.relayPort)
	}
	return "https://" + host
}

// validateReverseEndpoint checks that a relay-issued reverse endpoint is
// well-formed and directly reachable at the relay origin. Overlay endpoints
// are rejected because Portalite does not implement the overlay transport.
func (s *relaySupervisor) validateReverseEndpoint(endpoint reverseEndpoint, leaseExpiresAt time.Time) (reverseEndpoint, error) {
	endpoint.URL = strings.TrimSpace(endpoint.URL)
	endpoint.Capability = strings.TrimSpace(endpoint.Capability)
	if endpoint.URL == "" || endpoint.Capability == "" {
		return reverseEndpoint{}, errors.New("relay returned an incomplete reverse endpoint")
	}
	parsed, err := url.Parse(endpoint.URL)
	if err != nil || parsed.Host == "" || !strings.EqualFold(parsed.Scheme, "https") {
		return reverseEndpoint{}, errors.New("relay returned an invalid reverse endpoint URL")
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" || parsed.EscapedPath() != pathConnect {
		return reverseEndpoint{}, errors.New("relay returned an invalid reverse endpoint target")
	}
	if endpoint.Overlay {
		return reverseEndpoint{}, errors.New("relay returned an overlay reverse endpoint but overlay is unsupported")
	}
	if !strings.EqualFold(parsed.Host, s.authority) {
		return reverseEndpoint{}, fmt.Errorf("relay reverse endpoint %s does not match the relay URL; align the relay's public URL with the address clients dial", endpoint.URL)
	}
	if !endpoint.ExpiresAt.After(time.Now()) || endpoint.ExpiresAt.After(leaseExpiresAt) {
		return reverseEndpoint{}, errors.New("relay returned an invalid reverse endpoint expiry")
	}
	endpoint.URL = parsed.String()
	return endpoint, nil
}

// buildTenantTLS creates the lease-scoped tenant TLS terminator. Protocol 10
// signs every TLS 1.3 CertificateVerify through the relay's transcript-bound
// /v1/sign endpoint, so no digest-signing probe is possible or needed: key
// possession is proven on each handshake. The signer reads the current access
// token per request, so one terminator serves every lease generation.
func (s *relaySupervisor) buildTenantTLS(
	initialToken string,
	canonicalHostname string,
	aliasHostname string,
	onLocalReady func(),
	onCertErr func(error),
) (*tenantTLS, error) {
	tenant := &tenantTLS{
		canonicalHostname: canonicalHostname,
	}
	monitoredHost := canonicalHostname
	if monitoredHost == "" {
		monitoredHost = aliasHostname
	}
	if s.certObserver != nil && monitoredHost != "" {
		obs, err := s.certObserver.Start(monitoredHost)
		if err != nil {
			return nil, fmt.Errorf("start certificate observer: %w", err)
		}
		tenant.observer = obs
	}
	var onLeaf func([]byte)
	if tenant.observer != nil {
		onLeaf = tenant.observer.RecordLocalLeafDER
	}
	if canonicalHostname != "" {
		tokenFunc := func() string {
			if token := s.currentToken(); token != "" {
				return token
			}
			return initialToken
		}
		local, err := newLocalTLSManager(
			canonicalHostname,
			s.tlsCertFile,
			s.tlsKeyFile,
			s.automaticTLS,
			s.tlsStateDir,
			s.relayURL,
			s.controlClient,
			tokenFunc,
			onLocalReady,
			onCertErr,
			onLeaf,
		)
		if err != nil {
			_ = tenant.close()
			return nil, err
		}
		tenant.local = local
	}

	needKeyless := !s.tcpEnabled && (aliasHostname != "" || canonicalHostname == "")
	if needKeyless && s.keyless != nil {
		terminator, err := s.buildKeylessTerminator(initialToken, s.certificateChain)
		if err != nil {
			_ = tenant.close()
			return nil, err
		}
		tenant.keyless = terminator
	} else if needKeyless && canonicalHostname == "" && !s.udpEnabled {
		_ = tenant.close()
		return nil, errors.New("keyless TLS provider is not configured")
	}
	return tenant, nil
}

func (s *relaySupervisor) buildKeylessTerminator(initialToken string, chainPEM []byte) (KeylessTerminator, error) {
	if s.keyless == nil {
		return nil, errors.New("keyless TLS provider is not configured")
	}
	return s.keyless.NewTerminator(KeylessConfig{
		RelayURL:          s.relayURL,
		ServerName:        s.hostname,
		ChainPEM:          append([]byte(nil), chainPEM...),
		RequestTimeout:    s.timings.requestTimeout,
		AccessTokenHeader: accessTokenHeader,
		AccessToken: func() string {
			if token := s.currentToken(); token != "" {
				return token
			}
			return initialToken
		},
	})
}

// refreshRelayCertificateChain re-pins the relay's TLS certificate chain after
// a zero-downtime relay certificate rotation and swaps the active Keyless
// terminator without dropping existing connections.
func (s *relaySupervisor) refreshRelayCertificateChain(ctx context.Context) (bool, error) {
	s.chainRefreshMu.Lock()
	defer s.chainRefreshMu.Unlock()

	tlsConfig, newChain, _, err := s.bootstrapTLS(ctx)
	if err != nil {
		return false, err
	}
	if bytes.Equal(newChain, s.certificateChain) {
		return false, nil
	}
	token := s.currentToken()
	if token == "" {
		return false, nil
	}
	var nextKeyless KeylessTerminator
	if s.keyless != nil {
		nextKeyless, err = s.buildKeylessTerminator(token, newChain)
		if err != nil {
			return false, err
		}
	}

	var oldKeyless KeylessTerminator
	s.leaseMu.Lock()
	s.controlTLS = tlsConfig
	s.certificateChain = newChain
	if s.lease.tenant != nil && s.lease.tenant.keyless != nil && nextKeyless != nil {
		oldKeyless = s.lease.tenant.keyless
		s.lease.tenant.keyless = nextKeyless
	} else {
		oldKeyless = nextKeyless
	}
	s.leaseMu.Unlock()
	if oldKeyless != nil {
		_ = oldKeyless.Close()
	}
	return true, nil
}

func (s *relaySupervisor) runLease(
	ctx context.Context,
	ready func(relayReadyInfo),
	datagramReady func(string),
	approval func(RelayApproval),
	offer func(context.Context, net.Conn) bool,
	offerDatagram func(context.Context, DatagramFrame) bool,
) error {
	leaseCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	workerCount := reverseSessionSlots + 1
	if s.udpEnabled {
		workerCount++
	}
	results := make(chan error, workerCount)
	var wg sync.WaitGroup
	startWorker := func(run func() error) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results <- run()
		}()
	}

	var readyOnce sync.Once
	announceReady := func() {
		info := s.currentReadyInfo()
		if s.useLocalTLS() && s.tlsRequireLocal && !s.isLocalTLSReady() {
			return
		}
		if info.publicURL == "" && info.aliasURL == "" && info.tcpAddr == "" && !s.udpEnabled {
			return
		}
		readyOnce.Do(func() { ready(info) })
	}
	for range reverseSessionSlots {
		startWorker(func() error {
			return s.runReverseSlot(leaseCtx, announceReady, approval, offer)
		})
	}
	startWorker(func() error { return s.runRenewLoop(leaseCtx) })
	if s.udpEnabled {
		startWorker(func() error {
			return s.runDatagramLoop(leaseCtx, datagramReady, offerDatagram)
		})
	}
	var firstResult error
	haveFirstResult := false
	select {
	case <-ctx.Done():
	case firstResult = <-results:
		haveFirstResult = true
	}
	cancel()
	wg.Wait()

	workerResults := make([]error, 0, workerCount)
	if haveFirstResult {
		workerResults = append(workerResults, firstResult)
	}
	for len(workerResults) < workerCount {
		workerResults = append(workerResults, <-results)
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	var transient error
	refresh := false
	for _, result := range workerResults {
		if isTerminalRelayFailure(result) {
			return result
		}
		if errors.Is(result, errLeaseRefresh) {
			refresh = true
			continue
		}
		if result != nil && !errors.Is(result, context.Canceled) && transient == nil {
			transient = result
		}
	}
	if refresh {
		return errLeaseRefresh
	}
	if transient != nil {
		return transient
	}
	return context.Canceled
}

func (s *relaySupervisor) runDatagramLoop(
	ctx context.Context,
	ready func(string),
	offer func(context.Context, DatagramFrame) bool,
) error {
	for {
		if err := ctx.Err(); err != nil {
			s.clearDatagramConn(nil, "lease stopped")
			return err
		}
		token, generation, udpAddr, tlsConfig, ok := s.currentDatagramLease()
		if !ok {
			return errLeaseRefresh
		}
		if s.datagram == nil {
			return terminalRelay(errors.New("portalite: UDP datagram dialer is not configured"))
		}
		conn, err := s.datagram.Dial(ctx, s.dialAddress, tlsConfig, token)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			var rejection DatagramRejection
			if errors.As(err, &rejection) {
				code := strings.ToLower(strings.TrimSpace(rejection.RejectionCode()))
				switch code {
				case "lease_not_found", "unauthorized":
					if s.datagramTokenStale(token, generation) {
						continue
					}
					return errLeaseRefresh
				case "lease_denied", "transport_mismatch":
					return terminalRelay(err)
				}
			}
			if !waitForRelayRetry(ctx, relayRetryDelay(s.timings.retryWait, err)) {
				return ctx.Err()
			}
			continue
		}

		s.installDatagramConn(conn)
		if ready != nil {
			ready(udpAddr)
		}
		for {
			data, receiveErr := conn.ReceiveDatagram(ctx)
			if receiveErr != nil {
				s.clearDatagramConn(conn, "backhaul disconnected")
				if ready != nil {
					ready("")
				}
				if ctx.Err() != nil {
					return ctx.Err()
				}
				break
			}
			frame, decodeErr := decodeDatagram(data)
			if decodeErr != nil {
				continue
			}
			frame.RelayURL = s.relayURL
			frame.UDPAddr = udpAddr
			if offer != nil {
				offer(ctx, frame)
			}
		}
		if !waitForRelayRetry(ctx, s.timings.retryWait) {
			return ctx.Err()
		}
	}
}

func (s *relaySupervisor) currentDatagramLease() (
	token string,
	generation uint64,
	udpAddr string,
	tlsConfig *tls.Config,
	ok bool,
) {
	s.leaseMu.RLock()
	token = s.lease.token
	generation = s.lease.generation
	udpAddr = s.lease.udpAddr
	ok = s.udpEnabled && token != "" && udpAddr != "" && s.controlTLS != nil
	if ok {
		tlsConfig = s.controlTLS.Clone()
	}
	s.leaseMu.RUnlock()
	return
}

func (s *relaySupervisor) installDatagramConn(conn DatagramConn) {
	s.datagramMu.Lock()
	old := s.datagramConn
	s.datagramConn = conn
	s.datagramMu.Unlock()
	if old != nil && old != conn {
		_ = old.CloseWithError(0, "replaced")
	}
}

func (s *relaySupervisor) clearDatagramConn(expected DatagramConn, reason string) {
	s.datagramMu.Lock()
	conn := s.datagramConn
	if expected == nil || conn == expected {
		s.datagramConn = nil
	} else {
		conn = nil
	}
	s.datagramMu.Unlock()
	if conn != nil {
		_ = conn.CloseWithError(0, reason)
	}
}

func (s *relaySupervisor) sendDatagram(frame DatagramFrame) error {
	if !s.udpEnabled {
		return errors.New("portalite: UDP is not enabled for relay")
	}
	encoded, err := encodeDatagram(frame.FlowID, frame.Payload)
	if err != nil {
		return err
	}
	s.datagramMu.RLock()
	conn := s.datagramConn
	s.datagramMu.RUnlock()
	if conn == nil {
		return errDatagramNotReady
	}
	if err := conn.SendDatagram(encoded); err != nil {
		return fmt.Errorf("send QUIC datagram: %w", err)
	}
	return nil
}

func (s *relaySupervisor) runReverseSlot(ctx context.Context, ready func(), approval func(RelayApproval), offer func(context.Context, net.Conn) bool) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		conn, err := s.openReverseSession(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if app := relayApprovalFromError(err); app != "" && approval != nil {
				approval(app)
			}
			var upgradeErr *reverseUpgradeError
			if errors.As(err, &upgradeErr) && isLostLeaseError(err) &&
				s.reverseConnectRequestStale(upgradeErr.capability, upgradeErr.generation) {
				if !waitForRelayRetry(ctx, relayRetryDelay(s.timings.retryWait, err)) {
					return ctx.Err()
				}
				continue
			}
			err = classifyRelayError(err, true)
			if errors.Is(err, errLeaseRefresh) || isTerminalRelayFailure(err) {
				return err
			}
			if !waitForRelayRetry(ctx, relayRetryDelay(s.timings.retryWait, err)) {
				return ctx.Err()
			}
			continue
		}

		ready()
		transferred, claimed, sessionErr := s.runReverseSession(ctx, conn, offer)
		if !transferred {
			_ = conn.Close()
		}
		if sessionErr == nil {
			continue
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var signerErr *tenantSignerError
		if claimed && !errors.As(sessionErr, &signerErr) {
			// A public client's TLS failure is not a relay failure and should be
			// replenished immediately rather than consuming the retry interval.
			continue
		}
		if signerErr != nil {
			if strings.Contains(signerErr.Error(), keylessUnknownCertificateMessage) {
				refreshCtx, cancel := context.WithTimeout(ctx, s.timings.requestTimeout)
				_, _ = s.refreshRelayCertificateChain(refreshCtx)
				cancel()
				continue
			}
			if s.signerGenerationStale(signerErr.generation) {
				if !waitForRelayRetry(ctx, s.timings.retryWait) {
					return ctx.Err()
				}
				continue
			}
		}
		sessionErr = classifyRelayError(sessionErr, true)
		if errors.Is(sessionErr, errLeaseRefresh) || isTerminalRelayFailure(sessionErr) {
			return sessionErr
		}
		if !waitForRelayRetry(ctx, relayRetryDelay(s.timings.retryWait, sessionErr)) {
			return ctx.Err()
		}
	}
}

func (s *relaySupervisor) openReverseSession(ctx context.Context) (net.Conn, error) {
	reverse, generation, ok := s.currentReverseEndpoint()
	if !ok {
		return nil, errLeaseRefresh
	}
	if !reverse.ExpiresAt.After(time.Now()) {
		return nil, errLeaseRefresh
	}
	reverseURL, err := url.Parse(reverse.URL)
	if err != nil {
		return nil, fmt.Errorf("parse reverse endpoint: %w", err)
	}

	dialer := &net.Dialer{Timeout: s.timings.dialTimeout, KeepAlive: 30 * time.Second}
	raw, err := dialer.DialContext(ctx, "tcp", s.dialAddress)
	if err != nil {
		return nil, fmt.Errorf("dial reverse session: %w", err)
	}
	conn := tls.Client(raw, s.controlTLS.Clone())
	closeOnError := true
	defer func() {
		if closeOnError {
			_ = conn.Close()
		}
	}()
	stopContextClose := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stopContextClose()

	handshakeCtx, cancel := context.WithTimeout(ctx, s.timings.handshakeTimeout)
	err = conn.HandshakeContext(handshakeCtx)
	cancel()
	if err != nil {
		return nil, fmt.Errorf("reverse TLS handshake: %w", err)
	}

	if err := conn.SetDeadline(time.Now().Add(s.timings.handshakeTimeout)); err != nil {
		return nil, fmt.Errorf("set reverse upgrade deadline: %w", err)
	}
	request := &http.Request{
		Method:     http.MethodGet,
		URL:        reverseURL,
		Host:       reverseURL.Host,
		Proto:      "HTTP/1.1",
		ProtoMajor: 1,
		ProtoMinor: 1,
		Header:     make(http.Header),
	}
	request.Header.Set("Connection", "Upgrade")
	request.Header.Set("Upgrade", "raw")
	request.Header.Set(reverseCapabilityHeader, reverse.Capability)
	if err := request.Write(conn); err != nil {
		return nil, fmt.Errorf("write reverse upgrade request: %w", err)
	}

	headReader := &reverseResponseHeadReader{
		reader:    conn,
		remaining: maxReverseResponseHeadBytes,
		bounded:   true,
	}
	reader := bufio.NewReader(headReader)
	response, err := http.ReadResponse(reader, request)
	if err != nil {
		return nil, fmt.Errorf("read reverse upgrade response: %w", err)
	}
	headReader.disableBound()
	if response.StatusCode != http.StatusSwitchingProtocols {
		apiErr := decodeHTTPAPIError(response)
		return nil, &reverseUpgradeError{capability: reverse.Capability, generation: generation, err: apiErr}
	}
	if response.Body != nil {
		_ = response.Body.Close()
	}
	if !headerHasToken(response.Header, "Connection", "upgrade") {
		return nil, terminalRelay(errors.New("reverse upgrade response is missing Connection: Upgrade"))
	}
	if !strings.EqualFold(strings.TrimSpace(response.Header.Get("Upgrade")), "raw") {
		return nil, terminalRelay(errors.New("reverse upgrade response is missing Upgrade: raw"))
	}
	if err := conn.SetDeadline(time.Time{}); err != nil {
		return nil, fmt.Errorf("clear reverse upgrade deadline: %w", err)
	}

	if !stopContextClose() && ctx.Err() != nil {
		return nil, ctx.Err()
	}
	closeOnError = false
	if reader.Buffered() != 0 {
		return &bufferedNetConn{Conn: conn, reader: reader}, nil
	}
	return conn, nil
}

func headerHasToken(header http.Header, name, token string) bool {
	for _, value := range header.Values(name) {
		for _, part := range strings.Split(value, ",") {
			if strings.EqualFold(strings.TrimSpace(part), token) {
				return true
			}
		}
	}
	return false
}

type bufferedNetConn struct {
	net.Conn
	reader *bufio.Reader
}

func (c *bufferedNetConn) Read(p []byte) (int, error) { return c.reader.Read(p) }

func (s *relaySupervisor) runReverseSession(ctx context.Context, conn net.Conn, offer func(context.Context, net.Conn) bool) (transferred, claimed bool, err error) {
	stopClose := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer func() {
		if !transferred {
			stopClose()
		}
	}()

	for {
		if err := conn.SetReadDeadline(time.Now().Add(s.timings.idleTimeout)); err != nil {
			return false, false, fmt.Errorf("set reverse marker deadline: %w", err)
		}
		var marker [1]byte
		if _, err := io.ReadFull(conn, marker[:]); err != nil {
			return false, false, fmt.Errorf("read reverse stream marker: %w", err)
		}
		switch marker[0] {
		case markerKeepalive:
			continue
		case markerRaw:
			if !s.tcpEnabled {
				return false, false, terminalRelay(errors.New("relay requested unsupported raw stream marker 0x01"))
			}
			if err := conn.SetDeadline(time.Time{}); err != nil {
				return false, true, fmt.Errorf("clear reverse marker deadline: %w", err)
			}
			if !stopClose() && ctx.Err() != nil {
				_ = conn.Close()
				return false, true, ctx.Err()
			}
			if !offer(ctx, conn) {
				_ = conn.Close()
				return false, true, ctx.Err()
			}
			return true, true, nil
		case markerTLS:
			var binding [tlsBindingSize]byte
			if _, err := io.ReadFull(conn, binding[:]); err != nil {
				return false, false, fmt.Errorf("read reverse TLS binding: %w", err)
			}
			if err := conn.SetDeadline(time.Time{}); err != nil {
				return false, true, fmt.Errorf("clear reverse marker deadline: %w", err)
			}
			tenant, generation, err := s.currentTenant()
			if err != nil {
				return false, true, err
			}
			tlsConn, err := s.handshakeTenantTLS(ctx, conn, binding[:], tenant, generation)
			if err != nil {
				return false, true, err
			}
			if !stopClose() && ctx.Err() != nil {
				_ = tlsConn.Close()
				return false, true, ctx.Err()
			}
			if !offer(ctx, tlsConn) {
				_ = tlsConn.Close()
				return false, true, ctx.Err()
			}
			return true, true, nil
		default:
			return false, false, terminalRelay(fmt.Errorf("relay sent unknown stream marker 0x%02x", marker[0]))
		}
	}
}

func (s *relaySupervisor) handshakeTenantTLS(
	ctx context.Context,
	conn net.Conn,
	binding []byte,
	tenant *tenantTLS,
	generation uint64,
) (net.Conn, error) {
	useLocal := false
	targetConn := conn
	switch {
	case tenant.local != nil && tenant.keyless == nil:
		useLocal = true
	case tenant.local != nil && tenant.keyless != nil:
		sni, replayed, err := peekClientHelloSNI(conn, s.timings.handshakeTimeout)
		if err != nil {
			return nil, fmt.Errorf("peek tenant TLS SNI: %w", err)
		}
		targetConn = replayed
		useLocal = sni == tenant.canonicalHostname
	}

	handshakeCtx, cancel := context.WithTimeout(ctx, s.timings.handshakeTimeout)
	defer cancel()

	if useLocal {
		cfg, err := tenant.local.tlsConfig()
		if err != nil {
			return nil, fmt.Errorf("local tenant TLS: %w", err)
		}
		tlsConn := tls.Server(targetConn, cfg)
		if err := tlsConn.HandshakeContext(handshakeCtx); err != nil {
			return nil, fmt.Errorf("local tenant TLS handshake: %w", err)
		}
		return tlsConn, nil
	}
	if tenant.keyless == nil {
		return nil, errors.New("tenant TLS configuration is unavailable")
	}
	tlsConn, err := tenant.keyless.Handshake(handshakeCtx, targetConn, binding)
	if err != nil {
		if tenant.keyless.IsSignerError(err) {
			return nil, &tenantSignerError{
				err:        err,
				generation: generation,
			}
		}
		return nil, err
	}
	return tlsConn, nil
}

func (s *relaySupervisor) runRenewLoop(ctx context.Context) error {
	for {
		token, expiresAt, generation := s.currentTokenExpiryAndGeneration()
		if token == "" || !expiresAt.After(time.Now()) {
			return errLeaseRefresh
		}
		renewAt := expiresAt.Add(-s.timings.renewBefore)
		if delay := time.Until(renewAt); delay > 0 {
			if !waitForRelayRetry(ctx, delay) {
				return ctx.Err()
			}
		}

		if !s.beginRenewal(token, generation) {
			return errLeaseRefresh
		}
		var localTLSNotAfter time.Time
		if expiry := s.localTLSCertificateExpiry(); !expiry.IsZero() {
			localTLSNotAfter = expiry
		}
		var response renewResponse
		err := s.doAPI(ctx, http.MethodPost, pathRenew, renewRequest{
			LocalTLSNotAfter: localTLSNotAfter,
			AccessToken:      token,
			TTL:              s.timings.ttlSeconds(),
			RepinsChain:      true,
		}, &response)
		if err != nil {
			s.endRenewal(generation)
			err = classifyRelayError(err, true)
			if errors.Is(err, errLeaseRefresh) || isTerminalRelayFailure(err) {
				return err
			}
			if !time.Now().Before(expiresAt) {
				return errLeaseRefresh
			}
			delay := relayRetryDelay(s.timings.retryWait, err)
			if remaining := time.Until(expiresAt); remaining < delay {
				delay = remaining
			}
			if delay <= 0 || !waitForRelayRetry(ctx, delay) {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				return errLeaseRefresh
			}
			continue
		}

		newToken := strings.TrimSpace(response.AccessToken)
		if newToken == "" {
			return terminalRelay(errors.New("renew response has an empty access_token"))
		}
		if !response.ExpiresAt.After(time.Now()) {
			return terminalRelay(errors.New("renew response lease is expired"))
		}
		reverse, err := s.validateReverseEndpoint(response.ReverseEndpoint, response.ExpiresAt)
		if err != nil {
			return terminalRelay(err)
		}
		s.leaseMu.Lock()
		if s.lease.token != token || s.lease.generation != generation {
			s.leaseMu.Unlock()
			return errLeaseRefresh
		}
		s.nextLeaseGeneration++
		s.lease.token = newToken
		s.lease.expiresAt = response.ExpiresAt
		s.lease.reverse = reverse
		s.lease.renewing = false
		s.lease.tokenUncertain = false
		s.lease.generation = s.nextLeaseGeneration
		shouldCheckChain := time.Since(s.lease.chainCheckedAt) >= defaultChainRefreshInterval
		tenant := s.lease.tenant
		s.leaseMu.Unlock()
		s.clearDatagramConn(nil, "lease renewed")
		if shouldCheckChain {
			refreshCtx, cancel := context.WithTimeout(ctx, s.timings.requestTimeout)
			if _, err := s.refreshRelayCertificateChain(refreshCtx); err == nil {
				s.leaseMu.Lock()
				s.lease.chainCheckedAt = time.Now()
				s.leaseMu.Unlock()
			}
			cancel()
		}
		if tenant != nil && tenant.local != nil {
			_, _ = tenant.local.reloadManual()
		}
	}
}

func (s *relaySupervisor) doAPI(ctx context.Context, method, path string, body, output any) error {
	if s.controlClient == nil {
		return errors.New("relay control client is not initialized")
	}
	var requestBody io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encode relay request: %w", err)
		}
		requestBody = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, s.relayURL+path, requestBody)
	if err != nil {
		return fmt.Errorf("build relay request: %w", err)
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	request.Header.Set("Accept", "application/json")

	response, err := s.controlClient.Do(request)
	if err != nil {
		return fmt.Errorf("relay request: %w", err)
	}
	defer response.Body.Close()
	payload, err := readBoundedControlBody(response.Body)
	if err != nil {
		if response.StatusCode >= 200 && response.StatusCode < 300 {
			if errors.Is(err, errControlResponseTooLarge) {
				return &malformedRelayResponseError{err: err}
			}
			// A body read that did not complete is a transport failure, not a
			// completed malformed success response.
			return err
		}
		return &APIError{StatusCode: response.StatusCode, RetryAfter: parseRetryAfter(response)}
	}

	var envelope apiEnvelope
	decodeErr := json.Unmarshal(payload, &envelope)
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		apiErr := &APIError{StatusCode: response.StatusCode, RetryAfter: parseRetryAfter(response)}
		if decodeErr == nil && !envelope.OK && envelope.Error != nil {
			apiErr.Code = envelope.Error.Code
			apiErr.Message = envelope.Error.Message
		}
		return apiErr
	}
	if decodeErr != nil {
		return &malformedRelayResponseError{err: fmt.Errorf("decode successful relay response: %w", decodeErr)}
	}
	if !envelope.OK {
		apiErr := &APIError{StatusCode: response.StatusCode, RetryAfter: parseRetryAfter(response)}
		if envelope.Error != nil {
			apiErr.Code = envelope.Error.Code
			apiErr.Message = envelope.Error.Message
		}
		return apiErr
	}
	if output != nil {
		if len(envelope.Data) == 0 || bytes.Equal(bytes.TrimSpace(envelope.Data), []byte("null")) {
			return &malformedRelayResponseError{err: errors.New("successful relay response has no data")}
		}
		if err := json.Unmarshal(envelope.Data, output); err != nil {
			return &malformedRelayResponseError{err: fmt.Errorf("decode successful relay data: %w", err)}
		}
	}
	return nil
}

func readBoundedControlBody(reader io.Reader) ([]byte, error) {
	payload, err := io.ReadAll(io.LimitReader(reader, maxControlResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read relay response: %w", err)
	}
	if int64(len(payload)) > maxControlResponseBytes {
		return nil, fmt.Errorf("%w: exceeds %d bytes", errControlResponseTooLarge, maxControlResponseBytes)
	}
	return payload, nil
}

func decodeHTTPAPIError(response *http.Response) error {
	defer response.Body.Close()
	payload, err := readBoundedControlBody(response.Body)
	apiErr := &APIError{StatusCode: response.StatusCode, RetryAfter: parseRetryAfter(response)}
	if err != nil {
		return apiErr
	}
	var envelope apiEnvelope
	if json.Unmarshal(payload, &envelope) == nil && !envelope.OK && envelope.Error != nil {
		apiErr.Code = envelope.Error.Code
		apiErr.Message = envelope.Error.Message
	}
	return apiErr
}

func (s *relaySupervisor) unregisterToken(ctx context.Context, token string) error {
	if token == "" || s.controlClient == nil {
		return nil
	}
	return s.doAPI(ctx, http.MethodPost, pathUnregister, unregisterRequest{AccessToken: token}, nil)
}

func (s *relaySupervisor) shutdown() error {
	s.clearDatagramConn(nil, "relay shutting down")
	token := s.currentToken()
	var errs []error
	if token != "" {
		ctx, cancel := context.WithTimeout(context.Background(), s.timings.shutdownTimeout)
		if err := s.unregisterToken(ctx, token); err != nil {
			errs = append(errs, fmt.Errorf("unregister relay lease: %w", err))
		}
		cancel()
	}
	if tenant := s.clearLease(); tenant != nil {
		if err := tenant.close(); err != nil {
			errs = append(errs, fmt.Errorf("close keyless signer: %w", err))
		}
	}
	s.resetControl()
	return errors.Join(errs...)
}

func (s *relaySupervisor) discardLostLease() {
	s.clearDatagramConn(nil, "lease lost")
	if tenant := s.clearLease(); tenant != nil {
		_ = tenant.close()
	}
}

func (s *relaySupervisor) clearLease() *tenantTLS {
	s.leaseMu.Lock()
	tenant := s.lease.tenant
	s.lease = relayLease{}
	s.leaseMu.Unlock()
	return tenant
}

func (s *relaySupervisor) resetControl() {
	if s.controlTransport != nil {
		s.controlTransport.CloseIdleConnections()
	}
	s.controlClient = nil
	s.controlTransport = nil
	s.controlTLS = nil
	s.certificateChain = nil
	s.publicHostname = ""
}

func (s *relaySupervisor) currentToken() string {
	s.leaseMu.RLock()
	token := s.lease.token
	s.leaseMu.RUnlock()
	return token
}

func (s *relaySupervisor) currentTokenExpiryAndGeneration() (string, time.Time, uint64) {
	s.leaseMu.RLock()
	token, expiry, generation := s.lease.token, s.lease.expiresAt, s.lease.generation
	s.leaseMu.RUnlock()
	return token, expiry, generation
}

func (s *relaySupervisor) currentReverseEndpoint() (reverseEndpoint, uint64, bool) {
	s.leaseMu.RLock()
	reverse, generation := s.lease.reverse, s.lease.generation
	ok := s.lease.token != "" && reverse.Capability != ""
	s.leaseMu.RUnlock()
	return reverse, generation, ok
}

func (s *relaySupervisor) currentPublicURL() string {
	s.leaseMu.RLock()
	publicURL := s.lease.publicURL
	s.leaseMu.RUnlock()
	return publicURL
}

func (s *relaySupervisor) beginRenewal(token string, generation uint64) bool {
	s.leaseMu.Lock()
	defer s.leaseMu.Unlock()
	if s.lease.token != token || s.lease.generation != generation || s.lease.renewing {
		return false
	}
	s.lease.tokenUncertain = true
	s.lease.renewing = true
	return true
}

func (s *relaySupervisor) endRenewal(generation uint64) {
	s.leaseMu.Lock()
	if s.lease.generation == generation {
		s.lease.renewing = false
	}
	s.leaseMu.Unlock()
}

func (s *relaySupervisor) reverseConnectRequestStale(capability string, generation uint64) bool {
	s.leaseMu.RLock()
	stale := s.lease.reverse.Capability != capability || s.lease.generation != generation || s.lease.renewing
	s.leaseMu.RUnlock()
	return stale
}

func (s *relaySupervisor) datagramTokenStale(token string, generation uint64) bool {
	s.leaseMu.RLock()
	stale := s.lease.token != token || s.lease.generation != generation || s.lease.renewing
	s.leaseMu.RUnlock()
	return stale
}

func (s *relaySupervisor) signerGenerationStale(generation uint64) bool {
	s.leaseMu.RLock()
	stale := s.lease.generation != generation || s.lease.renewing || s.lease.tokenUncertain
	s.leaseMu.RUnlock()
	return stale
}

func (s *relaySupervisor) currentTenant() (*tenantTLS, uint64, error) {
	s.leaseMu.RLock()
	tenant := s.lease.tenant
	generation := s.lease.generation
	s.leaseMu.RUnlock()
	if tenant == nil || (tenant.keyless == nil && tenant.local == nil) {
		return nil, 0, errors.New("tenant TLS configuration is unavailable")
	}
	return tenant, generation, nil
}

func (s *relaySupervisor) currentReadyInfo() relayReadyInfo {
	s.leaseMu.RLock()
	defer s.leaseMu.RUnlock()
	publicURL := ""
	if s.lease.tenant != nil && s.lease.tenant.keyless != nil {
		publicURL = s.lease.aliasURL
	}
	if s.lease.canonicalHostname != "" && s.lease.tenant != nil && s.lease.tenant.local != nil && s.lease.tenant.local.ready() {
		publicURL = s.lease.publicURL
	}
	return relayReadyInfo{
		publicURL:  publicURL,
		aliasURL:   s.lease.aliasURL,
		aliasEpoch: s.lease.aliasEpoch,
		tcpAddr:    s.lease.tcpAddr,
	}
}

func (s *relaySupervisor) isLocalTLSReady() bool {
	s.leaseMu.RLock()
	defer s.leaseMu.RUnlock()
	return s.lease.tenant != nil && s.lease.tenant.local != nil && s.lease.tenant.local.ready()
}

func (s *relaySupervisor) localTLSCertificateExpiry() time.Time {
	s.leaseMu.RLock()
	defer s.leaseMu.RUnlock()
	if s.lease.tenant != nil && s.lease.tenant.local != nil && s.lease.tenant.local.ready() {
		return s.lease.tenant.local.certificateExpiry()
	}
	return time.Time{}
}

// terminalAPIErrorCodes are the relay responses that permanently reject this
// lease or configuration.
var terminalAPIErrorCodes = map[string]struct{}{
	"feature_unavailable":   {},
	"hostname_conflict":     {},
	"http11_only":           {},
	"lease_denied":          {},
	"protocol_incompatible": {},
	"source_not_allowed":    {},
	"tcp_port_disabled":     {},
	"transport_mismatch":    {},
	"udp_disabled":          {},
}

func isTerminalAPIErrorCode(code string) bool {
	_, terminal := terminalAPIErrorCodes[strings.ToLower(strings.TrimSpace(code))]
	return terminal
}

func isRetryableAPIErrorCode(code string) bool {
	switch strings.ToLower(strings.TrimSpace(code)) {
	case "replica_conflict", "lease_rejected", "approval_queue_full":
		return true
	default:
		return false
	}
}

func relayApprovalFromError(err error) RelayApproval {
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		return ""
	}
	switch strings.ToLower(strings.TrimSpace(apiErr.Code)) {
	case "lease_rejected":
		return RelayApprovalPending
	case "lease_denied":
		return RelayApprovalRefused
	default:
		return ""
	}
}

// isRateLimitedAPIError identifies a throttled response, which arrives either
// as HTTP 429 or as a rate_limited code.
func isRateLimitedAPIError(err *APIError) bool {
	if err == nil {
		return false
	}
	return err.StatusCode == http.StatusTooManyRequests ||
		strings.EqualFold(strings.TrimSpace(err.Code), "rate_limited")
}

func classifyRelayError(err error, allowLeaseRefresh bool) error {
	if err == nil || isTerminalRelayFailure(err) || errors.Is(err, errLeaseRefresh) {
		return err
	}
	if allowLeaseRefresh && isLostLeaseError(err) {
		return errLeaseRefresh
	}
	var malformed *malformedRelayResponseError
	if errors.As(err, &malformed) {
		return terminalRelay(err)
	}
	if isCertificateValidationError(err) || errors.Is(err, ErrLocalCertificate) {
		return terminalRelay(err)
	}
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		if isTerminalAPIErrorCode(apiErr.Code) {
			return terminalRelay(err)
		}
		if isRateLimitedAPIError(apiErr) || isRetryableAPIErrorCode(apiErr.Code) || apiErr.StatusCode >= 500 {
			return err
		}
		return terminalRelay(err)
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	var networkError net.Error
	if errors.As(err, &networkError) {
		return err
	}
	return terminalRelay(err)
}

func isLostLeaseError(err error) bool {
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	code := strings.ToLower(strings.TrimSpace(apiErr.Code))
	return code == "lease_not_found" || code == "unauthorized"
}
func isCertificateValidationError(err error) bool {
	var verificationError *tls.CertificateVerificationError
	if errors.As(err, &verificationError) {
		return true
	}
	var hostnameError x509.HostnameError
	if errors.As(err, &hostnameError) {
		return true
	}
	var authorityError x509.UnknownAuthorityError
	if errors.As(err, &authorityError) {
		return true
	}
	var invalidError x509.CertificateInvalidError
	return errors.As(err, &invalidError)
}

func terminalRelay(err error) error {
	if err == nil || isTerminalRelayFailure(err) {
		return err
	}
	return &terminalRelayError{err: err}
}

func isTerminalRelayFailure(err error) bool {
	var terminal *terminalRelayError
	return errors.As(err, &terminal)
}

func unwrapTerminalRelayError(err error) error {
	var terminal *terminalRelayError
	if errors.As(err, &terminal) {
		return terminal.err
	}
	return err
}

func waitForRelayRetry(ctx context.Context, delay time.Duration) bool {
	if delay <= 0 {
		return ctx.Err() == nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
