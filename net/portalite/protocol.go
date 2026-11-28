package portalite

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"
)

// ProtocolVersion is the relay SDK protocol version implemented by Portalite.
const ProtocolVersion = "10"

// LocalTLSNamespaceV1 is explicitly negotiated before issuing identity-bound
// two-label tenant hostnames. An absent namespace uses only a friendly alias.
const LocalTLSNamespaceV1 = "local-tls-v1"

const unnamedServiceLabel = "default"

const (
	pathDomain            = "/sdk/domain"
	pathRegisterChallenge = "/sdk/register/challenge"
	pathRegister          = "/sdk/register"
	pathRenew             = "/sdk/renew"
	pathUnregister        = "/sdk/unregister"
	pathConnect           = "/sdk/connect"
	pathTenantCertificate = "/v1/tenant-certificate"

	accessTokenHeader       = "X-Portal-Access-Token"
	reverseCapabilityHeader = "X-Portal-Reverse-Capability"

	// keylessUnknownCertificateMessage is the refusal string returned by
	// /v1/sign after a relay rotates its certificate without restarting.
	keylessUnknownCertificateMessage = "relay does not hold the key of the presented certificate"

	markerKeepalive byte = 0x00
	markerRaw       byte = 0x01 // Raw TCP stream when TCPEnabled is true.
	markerTLS       byte = 0x02 // Followed by tlsBindingSize bytes of per-connection binding.

	// tlsBindingSize is the per-connection value the relay mints for each
	// reverse TLS activation frame. The tenant must present it on every
	// transcript-signing request for that connection.
	tlsBindingSize = 16

	maxControlResponseBytes int64 = 1 << 20
)

// APIError is an error returned by a relay control-plane endpoint.
// RetryAfter carries the relay-advertised backoff for throttled responses; it
// is zero when the relay sent no usable Retry-After header.
type APIError struct {
	StatusCode int
	Code       string
	Message    string
	RetryAfter time.Duration
}

func (e *APIError) Error() string {
	if e == nil {
		return ""
	}

	code := strings.TrimSpace(e.Code)
	message := strings.TrimSpace(e.Message)
	if code != "" {
		return code + ": " + message
	}
	if message != "" {
		return message
	}
	if e.StatusCode > 0 {
		return fmt.Sprintf("api request failed with status %d", e.StatusCode)
	}
	return "api request failed"
}

type apiEnvelope struct {
	Data  json.RawMessage `json:"data,omitempty"`
	Error *apiErrorBody   `json:"error,omitempty"`
	OK    bool            `json:"ok"`
}

type apiErrorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type domainResponse struct {
	ApprovalMode              string   `json:"approval_mode,omitempty"`
	SupportedProtocolVersions []string `json:"supported_protocol_versions,omitzero"`
	Replicas                  bool     `json:"replicas,omitempty"`
	Namespaces                []string `json:"namespaces,omitempty"`
	TenantCertificateIssuance *bool    `json:"tenant_certificate_issuance,omitempty"`
	ProtocolVersion           string   `json:"protocol_version"`
	ReleaseVersion            string   `json:"release_version,omitempty"`
}

// supportsProtocol separates SDK wire compatibility from feature reporting.
// When SupportedProtocolVersions is omitted (nil), it falls back to exact
// equality with ProtocolVersion for older relays. An explicit empty list
// supports nothing.
func (d domainResponse) supportsProtocol(version string) bool {
	version = strings.TrimSpace(version)
	if version == "" {
		return false
	}
	if d.SupportedProtocolVersions == nil {
		return strings.TrimSpace(d.ProtocolVersion) == version
	}
	return slices.Contains(d.SupportedProtocolVersions, version)
}

type identityRef struct {
	Name    string `json:"name,omitempty"`
	Address string `json:"address,omitempty"`
}

type challengeRequest struct {
	ProtocolVersion string      `json:"protocol_version,omitempty"`
	Namespace       string      `json:"namespace,omitempty"`
	Replica         bool        `json:"replica,omitempty"`
	Identity        identityRef `json:"identity"`
	TTL             int         `json:"ttl,omitempty"`
	UDPEnabled      bool        `json:"udp_enabled,omitempty"`
	TCPEnabled      bool        `json:"tcp_enabled,omitempty"`
}

type challengeResponse struct {
	Replica     bool      `json:"replica,omitempty"`
	ChallengeID string    `json:"challenge_id"`
	ExpiresAt   time.Time `json:"expires_at"`
	SIWEMessage string    `json:"siwe_message"`
}

type registerRequest struct {
	ChallengeID   string `json:"challenge_id"`
	SIWEMessage   string `json:"siwe_message"`
	SIWESignature string `json:"siwe_signature"`
}

// reverseEndpoint authorizes one class of operation: opening reverse streams.
// The capability is opaque to SDK callers and cannot mutate the owning lease.
type reverseEndpoint struct {
	URL        string    `json:"url"`
	Capability string    `json:"capability"`
	ExpiresAt  time.Time `json:"expires_at"`
	Overlay    bool      `json:"overlay,omitempty"`
}

type registerResponse struct {
	Namespace         string          `json:"namespace,omitempty"`
	Identity          identityRef     `json:"identity"`
	Hostname          string          `json:"hostname,omitempty"`
	CanonicalHostname string          `json:"canonical_hostname,omitempty"`
	AliasEpoch        string          `json:"alias_epoch,omitempty"`
	ExpiresAt         time.Time       `json:"expires_at"`
	AccessToken       string          `json:"access_token"`
	ReverseEndpoint   reverseEndpoint `json:"reverse_endpoint"`
	UDPAddr           string          `json:"udp_addr,omitempty"`
	UDPEnabled        bool            `json:"udp_enabled,omitempty"`
	TCPAddr           string          `json:"tcp_addr,omitempty"`
	TCPEnabled        bool            `json:"tcp_enabled,omitempty"`
}

type renewRequest struct {
	LocalTLSNotAfter time.Time `json:"local_tls_not_after,omitzero"`
	AccessToken      string    `json:"access_token"`
	TTL              int       `json:"ttl,omitempty"`
	RepinsChain      bool      `json:"repins_chain,omitempty"`
}

type renewResponse struct {
	ExpiresAt       time.Time       `json:"expires_at"`
	AccessToken     string          `json:"access_token"`
	ReverseEndpoint reverseEndpoint `json:"reverse_endpoint"`
}

type unregisterRequest struct {
	AccessToken string `json:"access_token"`
}

type tenantCertificateRequest struct {
	CSRPEM string `json:"csr_pem"`
}

type tenantCertificateResponse struct {
	CertificatePEM string `json:"certificate_pem"`
}
