// Package grt implements the GRT/1.1 transport protocol: a reliable,
// authenticated, post-quantum-secured datagram transport with ordered
// streams, unreliable datagrams, connection migration, and mobile
// parking. This is the platform-neutral public API; it binds ordinary
// UDP sockets and exposes the spec's application operations (section
// 35) with Go-idiomatic stream objects.
//
// Wire format, cryptography, and the connection engine live in the
// internal packages; this package is the only supported surface.
package grt

import (
	gc "gosuda.org/grt/internal/crypto"
	"gosuda.org/grt/internal/engine"
	"gosuda.org/grt/internal/wire"
)

// Version is the implemented protocol version.
const Version = wire.Version

// Params is the negotiated transport-parameter structure (spec section
// 10). Use DefaultParams and adjust fields before use.
type Params = wire.Params

// DefaultParams returns the Compact-profile defaults (spec sections 3
// and 36).
func DefaultParams() Params { return wire.DefaultParams() }

// Preset identifies a standardized transport profile.
type Preset = wire.Preset

const (
	// PresetLowResource enforces the Section 36 Compact session profile (zero initial credit, ~3KB footprint).
	PresetLowResource = wire.PresetLowResource
	// PresetLowLatency pre-funds initial credit (64KB) for zero-credit-RTT immediate request/response.
	PresetLowLatency = wire.PresetLowLatency
	// PresetStandard1RTT disables early-data replay exposure and funds 1MB credit for strict PFS.
	PresetStandard1RTT = wire.PresetStandard1RTT
	// PresetHighThroughput configures 4KB UDP payloads and 16MB credit for gigabit streaming.
	PresetHighThroughput = wire.PresetHighThroughput
)

// LowResourceParams returns the Compact-profile defaults (spec sections 3 and 36).
func LowResourceParams() Params { return wire.LowResourceParams() }

// LowLatencyParams returns parameters pre-funded with 64KB credit and stream windows for immediate transmission.
func LowLatencyParams() Params { return wire.LowLatencyParams() }

// Standard1RTTParams returns parameters for safe 1-RTT operation with 1MB credit and early data disabled.
func Standard1RTTParams() Params { return wire.Standard1RTTParams() }

// HighThroughputParams returns parameters configured for 4KB UDP payloads and 16MB credit for line-rate streaming.
func HighThroughputParams() Params { return wire.HighThroughputParams() }

// Negotiable feature flags (spec section 10).
const (
	FeatureDatagram  = wire.FeatureDatagram
	FeatureEarly     = wire.FeatureEarly
	FeatureECN       = wire.FeatureECN
	FeatureMigration = wire.FeatureMigration
	FeaturePark      = wire.FeaturePark
)

// Authentication modes for the client (spec section 9).
const (
	// AuthPinned requires Config.ExpectedKey to equal the server's IK_S.
	AuthPinned uint8 = 0
	// AuthChain verifies the credential chain against TrustAnchors.
	AuthChain uint8 = 1
)

// Cipher identifiers (spec section 4.1).
const (
	CipherXChaCha20Poly1305 uint16 = wire.CipherXChaCha20Poly1305
	CipherAES256GCM         uint16 = wire.CipherAES256GCM
)

// State is the public connection-lifecycle snapshot (spec section 22).
type State = engine.State

const (
	StateNew         = engine.StateNew
	StateCBuild      = engine.StateCBuild
	StateCWaitReply  = engine.StateCWaitReply
	StateCVerify     = engine.StateCVerify
	StateCFinishing  = engine.StateCFinishing
	StateSCollect    = engine.StateSCollect
	StateSBuild      = engine.StateSBuild
	StateSFinishing  = engine.StateSFinishing
	StateEstablished = engine.StateEstablished
	StateClosing     = engine.StateClosing
	StateDraining    = engine.StateDraining
	StateQuiescing   = engine.StateQuiescing
	StateDead        = engine.StateDead
)

// CloseCode is the section-24 transport error registry.
type CloseCode = engine.CloseCode

// TransportError is the close error surfaced to the application. A
// Remote error carries the peer's code; a local error is ours.
type TransportError = engine.TransportError

// ServerIdentity is the server's long-term signing identity.
type ServerIdentity = engine.ServerIdentity

// ClientIdentity is an optional stable client identity for PQXDH.
// A nil identity uses a fresh ephemeral key pair per connection.
type ClientIdentity = engine.ClientIdentity

// ClientTicket is a stored resumption ticket (spec section 9.1).
type ClientTicket = engine.ClientTicket

// ReplayLedger durably consumes an early-data ticket binding. The
// Consume call must make the binding durable before returning true;
// false denies early-data exposure (the handshake still completes).
type ReplayLedger = engine.ReplayLedger

// ListenerStats is an immutable snapshot of listener occupancy and
// Section 37.7 abuse/admission telemetry counters.
type ListenerStats = engine.ListenerStats

// CredentialBytes is the fixed size of one credential-chain record
// (spec section 9.2). ServerIdentity.Chain holds [CredentialBytes]byte
// records, leaf first.
const CredentialBytes = wire.CredentialBytes

// Config supplies client and server settings. Dial uses the client
// fields; Listen uses the server fields. Fields a role does not use are
// ignored.
type Config struct {
	// Params is the advertised transport-parameter set. Zero value uses
	// DefaultParams.
	Params Params
	// AppID names the application domain (spec section 8).
	AppID [16]byte
	// NowUnix supplies wall-clock time for credential and ticket
	// expiry checks; nil uses the system clock.
	NowUnix func() int64
	// ResetSecret derives stateless-reset tokens. Optional.
	ResetSecret [32]byte
	// MaxLiveSent bounds in-flight sent-packet records (0 = default).
	MaxLiveSent int
	// MaxPendingCtrl bounds queued control items (0 = spec default).
	MaxPendingCtrl int
	// MaxAttempts bounds the listener's live server handshake attempts
	// (spec sections 20.1/37.6). A slot is held from attempt admission
	// until the attempt is confirmed or dies, covering the attempt's
	// goroutine plus its handshake and crypto state; attempts beyond
	// the bound are dropped at admission. 0 uses the default of 256.
	MaxAttempts int
	// MaxConnections bounds total live server connections on a listener
	// (spec section 37.6); 0 means no explicit connection ceiling beyond
	// system resources.
	MaxConnections int
	// SourceAttemptRate and SourceAttemptBurst bound per-source-IP
	// handshake admissions per second (spec section 37.6); 0 uses
	// default of 64/s burst 64.
	SourceAttemptRate  float64
	SourceAttemptBurst float64
	// AdmissionMode selects the listener's section-20.1 new-attempt
	// policy (default AdmissionRetry).
	AdmissionMode AdmissionMode

	// --- Client ---
	// ServerName is the authority string "host:port" (required for
	// Dial). It is bound into the transcript and credential checks.
	ServerName string
	// AuthMode selects AuthPinned or AuthChain (default AuthPinned).
	AuthMode uint8
	// ExpectedKey is the pinned server IK_S for AuthPinned.
	ExpectedKey [32]byte
	// TrustAnchors are the CHAIN verification roots for AuthChain.
	TrustAnchors [][32]byte
	// Identity is an optional stable client key pair.
	Identity *ClientIdentity
	// Ticket is an optional stored resumption ticket. Early data is
	// opt-in per spec section 9.1; offering a ticket does not itself
	// expose early bytes.
	Ticket *ClientTicket
	// EarlyData is an opt-in replayable application batch carried as
	// 0-RTT on the first client bidi stream (spec section 9.2). It is
	// sent only when a valid Ticket is also configured, bounded by the
	// ticket's advertised early plaintext limit, and replayed under the
	// server's replay-ledger policy — the application must declare the
	// batch safe for replay exposure.
	EarlyData []byte
	// AddressToken is a stored REUSE token from a prior connection's
	// NEW_TOKEN; presenting it can skip the stateless Retry round trip.
	AddressToken    [64]byte
	HasAddressToken bool

	// CipherPreference is an ordered list of 1 or 2 distinct cipher IDs
	// (spec section 4.1). Must contain CipherXChaCha20Poly1305 exactly once.
	// E.g. []uint16{CipherAES256GCM, CipherXChaCha20Poly1305} prefers AES.
	// If nil or empty, defaults to [CipherXChaCha20Poly1305, CipherAES256GCM].
	CipherPreference []uint16

	// --- Server ---
	// ServerIdentity is the server's signing identity (required for
	// Listen).
	ServerIdentity *ServerIdentity
	// DisableAES disables AES-256-GCM in the server's authenticated
	// cipher support mask (spec section 4.1). Bit 0 (XChaCha) remains mandatory.
	DisableAES bool
	// Domain is the listener's stable 32-byte token domain separating
	// tenants that may share address validation. Nil uses AppID.
	Domain []byte
	// TicketKey is the 32-byte master key from which per-period and
	// per-2^24-issuance ticket-envelope keys are derived (spec section 9.1);
	// required to issue resumption tickets.
	TicketKey [32]byte
	// TicketKeyID is the master-key generation mixed into derived ticket
	// keys; bump it when rotating TicketKey so tickets minted under an old
	// master are rejected cleanly.
	TicketKeyID uint32
	// TicketIssuer returns a ticket-ID prefix and counter per issue.
	TicketIssuer func() (prefix [16]byte, counter uint64)
	// ReplayLedger, when set, gates early-data exposure.
	ReplayLedger ReplayLedger
	// PolicyHash binds issued tickets to the server's current
	// authentication-policy digest (spec section 9.1). Tickets minted
	// under a different policy digest fail validation and the attempt
	// falls back to a full handshake; rotate this value to invalidate
	// outstanding tickets.
	PolicyHash [32]byte
	// ReplayDomain is the listener's locally authorized replay domain
	// (spec section 9.1). It is stamped into issued tickets, verified on
	// resumption, and scopes ReplayLedger consumption of early data.
	ReplayDomain [16]byte
}

// AdmissionMode selects the listener's section-20.1 policy for a new
// client attempt.
type AdmissionMode uint8

const (
	// AdmissionRetry is REQUIRED_RETRY: a tokenless first flight always
	// earns one rate-limited stateless RETRY and no per-attempt state.
	AdmissionRetry AdmissionMode = iota
	// AdmissionTokenless is bounded ALLOW_TOKENLESS: a small number of
	// tokenless flights may be spooled and answered within the received
	// anti-amplification budget; excess attempts are dropped.
	AdmissionTokenless
	// AdmissionShedding silently drops every new attempt.
	AdmissionShedding
)

// engineConfig projects the public config into the engine's view.
func (c *Config) engineConfig() *engine.Config {
	ec := &engine.Config{
		Params:             c.Params,
		ServerName:         c.ServerName,
		AppID:              c.AppID,
		AuthMode:           c.AuthMode,
		ExpectedKey:        c.ExpectedKey,
		ServerCert:         c.ServerIdentity,
		TrustAnchors:       c.TrustAnchors,
		NowUnix:            c.NowUnix,
		ReplayLedger:       c.ReplayLedger,
		TicketKey:          c.TicketKey,
		TicketKeyID:        c.TicketKeyID,
		TicketIssuer:       c.TicketIssuer,
		PolicyHash:         c.PolicyHash,
		ReplayDomain:       c.ReplayDomain,
		Ticket:             c.Ticket,
		EarlyData:          c.EarlyData,
		AddressToken:       c.AddressToken,
		HasAddressToken:    c.HasAddressToken,
		MaxLiveSent:        c.MaxLiveSent,
		MaxPendingCtrl:     c.MaxPendingCtrl,
		MaxAttempts:        c.MaxAttempts,
		MaxConnections:     c.MaxConnections,
		SourceAttemptRate:  c.SourceAttemptRate,
		SourceAttemptBurst: c.SourceAttemptBurst,
		AdmissionMode:      uint8(c.AdmissionMode),
		ResetSecret:        c.ResetSecret,
		CipherPreference:   c.CipherPreference,
		DisableAES:         c.DisableAES,
	}
	if ec.Params == (Params{}) {
		ec.Params = DefaultParams()
	}
	return ec
}

// GenerateServerIdentity creates a fresh server signing identity bound
// to authority ("host:port") and appID.
func GenerateServerIdentity(authority string, appID [16]byte) (*ServerIdentity, error) {
	kp, err := gc.GenerateX25519()
	if err != nil {
		return nil, err
	}
	return &ServerIdentity{
		IdentityPub:  kp.Pub,
		IdentityPriv: append([]byte(nil), kp.Priv[:]...),
		ServiceHash:  gc.ServiceHash(authority, appID),
	}, nil
}

// Option configures optional parameters on a Config.
type Option func(*Config)

// WithAdmissionMode sets the listener's attempt admission policy.
func WithAdmissionMode(mode AdmissionMode) Option {
	return func(c *Config) {
		c.AdmissionMode = mode
	}
}

// WithAuthMode sets client authentication mode (AuthPinned or AuthChain).
func WithAuthMode(mode uint8) Option {
	return func(c *Config) {
		c.AuthMode = mode
	}
}

// WithExpectedKey sets the pinned server public key for AuthPinned.
func WithExpectedKey(key [32]byte) Option {
	return func(c *Config) {
		c.ExpectedKey = key
	}
}

// WithTrustAnchors sets the trust anchors for AuthChain.
func WithTrustAnchors(anchors [][32]byte) Option {
	return func(c *Config) {
		c.TrustAnchors = anchors
	}
}

// WithCipherPreference sets the client's cipher preference order.
func WithCipherPreference(ciphers ...uint16) Option {
	return func(c *Config) {
		c.CipherPreference = ciphers
	}
}

// WithDisableAES sets whether AES-256-GCM is disabled.
func WithDisableAES(disable bool) Option {
	return func(c *Config) {
		c.DisableAES = disable
	}
}

// WithMaxLiveSent sets the maximum in-flight sent-packet records.
func WithMaxLiveSent(limit int) Option {
	return func(c *Config) {
		c.MaxLiveSent = limit
	}
}

// WithMaxAttempts sets the maximum concurrent in-flight server handshakes.
func WithMaxAttempts(limit int) Option {
	return func(c *Config) {
		c.MaxAttempts = limit
	}
}

// WithMaxConnections sets the maximum concurrent live server sessions on a listener.
func WithMaxConnections(limit int) Option {
	return func(c *Config) {
		c.MaxConnections = limit
	}
}

// WithSourceAttemptRate configures per-source-IP handshake admission rate and burst limits.
func WithSourceAttemptRate(rate, burst float64) Option {
	return func(c *Config) {
		c.SourceAttemptRate = rate
		c.SourceAttemptBurst = burst
	}
}

// WithNowUnix sets the time source for expiry checks.
func WithNowUnix(now func() int64) Option {
	return func(c *Config) {
		c.NowUnix = now
	}
}

// WithServerName sets the authority string on a server or client config.
func WithServerName(name string) Option {
	return func(c *Config) {
		c.ServerName = name
	}
}

// NewClientConfig creates a client configuration configured with the given preset.
func NewClientConfig(serverName string, appID [16]byte, preset Preset, opts ...Option) *Config {
	cfg := &Config{
		ServerName: serverName,
		AppID:      appID,
		Params:     preset.Params(),
		AuthMode:   AuthPinned,
	}
	for _, opt := range opts {
		opt(cfg)
	}
	return cfg
}

// NewServerConfig creates a server configuration configured with the given preset.
// For PresetStandard1RTT, PresetLowLatency, and PresetHighThroughput, AdmissionMode defaults
// to AdmissionTokenless (ALLOW_TOKENLESS) to enable direct 1-RTT establishment without stateless
// retry cookies. For PresetLowResource, it defaults to AdmissionRetry (REQUIRED_RETRY, 2-RTT).
func NewServerConfig(srv *ServerIdentity, appID [16]byte, preset Preset, opts ...Option) *Config {
	var adm AdmissionMode
	switch preset {
	case PresetStandard1RTT, PresetLowLatency, PresetHighThroughput:
		adm = AdmissionTokenless
	case PresetLowResource:
		fallthrough
	default:
		adm = AdmissionRetry
	}

	cfg := &Config{
		ServerIdentity: srv,
		AppID:          appID,
		Params:         preset.Params(),
		AdmissionMode:  adm,
	}
	for _, opt := range opts {
		opt(cfg)
	}
	return cfg
}
