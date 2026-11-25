// Package x402http gates net/http handlers behind x402 payments.
//
// The gate is chain-blind and application-blind: it parses the x402 payment
// headers, settles paid requests through a Facilitator before the resource
// runs, publishes the settlement receipt, and strips all payment traffic
// before the application handler sees the request. Chain specifics live in
// the facilitator implementation, not here.
//
// Supported wire contract (phase 1):
//
//   - inbound payment header: canonical v2 PAYMENT-SIGNATURE, with the
//     legacy X-PAYMENT accepted as a fallback when the canonical header is
//     absent;
//   - payment flow: the canonical upfront flow only (PaymentFlowUpfront).
//     The gate normalizes the advertised requirements to
//     extra.paymentFlow = "upfront" — omitted, clients would misread them
//     as the default "authorization" flow — and rejects a conflicting
//     configured flow. authorization/escrow orchestration is out of scope
//     for this phase;
//   - the 402 challenge always carries resource metadata: Config.Resource
//     (with URL) is required.
//
// Which routes or HTTP methods are paid is application policy: wrap exactly
// the handlers that should be paid.
package x402http

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"time"

	"github.com/gosuda/x402-facilitator/types"
)

const (
	// defaultRequestTimeout bounds a settle call when Config.RequestTimeout
	// is unset. The bound holds for facilitators that honor context
	// cancellation (expected of all implementations); the gate cannot
	// preempt one that ignores its context.
	defaultRequestTimeout = 30 * time.Second

	// defaultMaxTimeoutSeconds is published as maxTimeoutSeconds when the
	// configured requirements leave it unset.
	defaultMaxTimeoutSeconds = 60
)

// The only challenge reasons written to the 402 body.
const (
	reasonPaymentRequired      = "payment required"
	reasonInvalidPayment       = "invalid payment payload"
	reasonRequirementsMismatch = "payment requirements mismatch"
	reasonSettleFailed         = "payment settlement failed"
)

// PaymentFlowUpfront is the canonical x402 payment flow this gate
// implements: the payment is fully settled (upfront) before the resource
// handler runs. The gate normalizes its accepted requirements to
// extra.paymentFlow = "upfront" so clients build upfront payments instead of
// misreading the requirements as the default "authorization" flow. The
// remaining canonical flows ("authorization", "escrow") are out of scope for
// this phase; a configured extra.paymentFlow that conflicts with "upfront"
// is rejected at construction time, so supporting them later is additive.
const PaymentFlowUpfront = "upfront"

// Facilitator is the settlement backend the gate drives. Both the local
// facilitators and the remote api/client.Client satisfy it.
type Facilitator interface {
	Verify(ctx context.Context, payment *types.PaymentPayload, req *types.PaymentRequirements) (*types.PaymentVerifyResponse, error)
	Settle(ctx context.Context, payment *types.PaymentPayload, req *types.PaymentRequirements) (*types.PaymentSettleResponse, error)
}

// Config owns one x402 payment contract and its facilitator runtime.
type Config struct {
	// Requirements is the accepted payment contract: advertised in the 402
	// accepts list and passed to Settle. Scheme, Network, Asset, Amount, and
	// PayTo are required; a non-positive MaxTimeoutSeconds defaults to 60.
	Requirements types.PaymentRequirements

	// Facilitator settles accepted payments. Required.
	Facilitator Facilitator

	// Resource describes the paid resource and is embedded into every 402
	// challenge; a URL is required. This is deliberately stricter than the
	// wire DTO (types.ResourceInfo is optional there): the gate always
	// produces a complete v2 payment-required challenge, so it refuses
	// configurations that cannot. Use the public URL a paying client can
	// reach.
	Resource *types.ResourceInfo

	// RequestTimeout bounds each Settle call for facilitators that honor
	// context cancellation, as all implementations are expected to; the
	// gate cannot preempt one that ignores its context. Zero defaults to
	// 30s; a negative value is a configuration error.
	RequestTimeout time.Duration
}

// Gate wraps HTTP handlers behind the configured payment contract.
type Gate struct {
	facilitator    Facilitator
	requirements   types.PaymentRequirements
	resource       types.ResourceInfo
	requestTimeout time.Duration
}

// New validates cfg and returns a Gate. cfg is not mutated; the gate keeps a
// normalized copy of the requirements with extra.paymentFlow pinned to the
// upfront flow.
func New(cfg Config) (*Gate, error) {
	if cfg.Facilitator == nil {
		return nil, errors.New("x402http: facilitator is required")
	}
	requirements := cfg.Requirements
	switch {
	case strings.TrimSpace(requirements.Scheme) == "":
		return nil, errors.New("x402http: requirements.scheme is required")
	case strings.TrimSpace(requirements.Network) == "":
		return nil, errors.New("x402http: requirements.network is required")
	case strings.TrimSpace(requirements.Asset) == "":
		return nil, errors.New("x402http: requirements.asset is required")
	case strings.TrimSpace(requirements.Amount) == "":
		return nil, errors.New("x402http: requirements.amount is required")
	case strings.TrimSpace(requirements.PayTo) == "":
		return nil, errors.New("x402http: requirements.payTo is required")
	}
	if requirements.MaxTimeoutSeconds <= 0 {
		requirements.MaxTimeoutSeconds = defaultMaxTimeoutSeconds
	}
	// Copy Extra so normalization never touches the caller's map, then pin
	// the canonical flow: omitted paymentFlow would be read by clients as
	// the default "authorization" flow, which this gate does not perform.
	extra := make(map[string]interface{}, len(requirements.Extra)+1)
	for key, value := range requirements.Extra {
		extra[key] = value
	}
	if existing, ok := extra["paymentFlow"]; ok && existing != PaymentFlowUpfront {
		return nil, fmt.Errorf("x402http: requirements.extra.paymentFlow %v conflicts with the gate's %q flow", existing, PaymentFlowUpfront)
	}
	extra["paymentFlow"] = PaymentFlowUpfront
	requirements.Extra = extra
	if cfg.Resource == nil || strings.TrimSpace(cfg.Resource.URL) == "" {
		return nil, errors.New("x402http: resource with a URL is required for the 402 challenge")
	}
	if cfg.RequestTimeout < 0 {
		return nil, errors.New("x402http: request timeout must not be negative")
	}
	requestTimeout := cfg.RequestTimeout
	if requestTimeout == 0 {
		requestTimeout = defaultRequestTimeout
	}
	return &Gate{
		facilitator:    cfg.Facilitator,
		requirements:   requirements,
		resource:       *cfg.Resource,
		requestTimeout: requestTimeout,
	}, nil
}

// Wrap gates next behind the payment: requests without a verifiable payment
// receive the 402 challenge, paid requests are settled before being
// forwarded, payment headers are stripped from the forwarded request, and the
// settlement receipt is published as trusted response headers. Which routes
// and methods are paid is application policy: wrap exactly the handlers that
// should be paid.
func (g *Gate) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		payload, ok := g.paymentPayloadFromRequest(w, r)
		if !ok {
			return
		}
		if payload.X402Version != int(types.X402VersionV2) {
			g.write402(w, reasonInvalidPayment, "")
			return
		}
		if !paymentRequirementsMatchAccepted(g.requirements, payload.Accepted) {
			g.write402(w, reasonRequirementsMismatch, "")
			return
		}

		settleCtx, cancel := context.WithTimeout(r.Context(), g.requestTimeout)
		requirements := g.requirements
		settled, err := g.facilitator.Settle(settleCtx, payload, &requirements)
		cancel()
		if err != nil || settled == nil {
			g.write402(w, reasonSettleFailed, "")
			return
		}
		receipt, err := receiptOf(settled)
		if err != nil {
			g.write402(w, reasonSettleFailed, "")
			return
		}
		if !settled.Success {
			// Structured settlement failure: still a challenge, but the
			// client receives the receipt so it can tell a pending
			// broadcast from a payable failure instead of paying blindly.
			g.write402(w, reasonSettleFailed, receipt)
			return
		}
		setPaymentResponseHeaders(w.Header(), receipt)

		settlement := Settlement{
			Transaction: strings.TrimSpace(settled.Transaction),
			Network:     settled.Network,
			Payer:       strings.TrimSpace(settled.Payer),
		}
		// Forward on the inbound request context: the settle deadline must
		// not abort resource execution.
		forwarded := r.Clone(context.WithValue(r.Context(), settlementKey{}, settlement))
		StripPaymentHeaders(forwarded.Header)
		response := &gateResponseWriter{ResponseWriter: w, receipt: receipt}
		next.ServeHTTP(response, forwarded)
		if !response.wroteHeader {
			response.WriteHeader(http.StatusOK)
		}
	})
}

// paymentPayloadFromRequest parses the inbound payment, writing the 402
// challenge and returning false when no usable payload is present.
func (g *Gate) paymentPayloadFromRequest(w http.ResponseWriter, r *http.Request) (*types.PaymentPayload, bool) {
	raw := ""
	// PAYMENT-SIGNATURE is the canonical v2 request header; X-PAYMENT is the
	// legacy compatibility form and must not override it.
	for _, name := range []string{HeaderPaymentSignature, HeaderXPayment} {
		if value := strings.TrimSpace(r.Header.Get(name)); value != "" {
			raw = value
			break
		}
	}
	if raw == "" {
		g.write402(w, reasonPaymentRequired, "")
		return nil, false
	}
	payload, ok := decodePaymentPayload(raw)
	if !ok {
		g.write402(w, reasonInvalidPayment, "")
		return nil, false
	}
	return payload, true
}

// decodePaymentPayload decodes a raw payment header value: direct JSON first,
// then base64 (Std, RawStd, URL, RawURL) wrapping JSON.
func decodePaymentPayload(raw string) (*types.PaymentPayload, bool) {
	var direct types.PaymentPayload
	if err := json.Unmarshal([]byte(raw), &direct); err == nil {
		return &direct, true
	}
	for _, encoding := range []*base64.Encoding{
		base64.StdEncoding,
		base64.RawStdEncoding,
		base64.URLEncoding,
		base64.RawURLEncoding,
	} {
		decoded, err := encoding.DecodeString(raw)
		if err != nil {
			continue
		}
		var payload types.PaymentPayload
		if err := json.Unmarshal(decoded, &payload); err == nil {
			return &payload, true
		}
	}
	return nil, false
}

// paymentRequirementsMatchAccepted checks that the client is paying against
// the contract this gate advertised. Core payment terms must match exactly,
// while server-declared Extra fields must be preserved by the client; clients
// may add extra fields of their own.
func paymentRequirementsMatchAccepted(required, accepted types.PaymentRequirements) bool {
	if required.Scheme != accepted.Scheme ||
		required.Network != accepted.Network ||
		required.Asset != accepted.Asset ||
		required.Amount != accepted.Amount ||
		required.PayTo != accepted.PayTo ||
		required.MaxTimeoutSeconds != accepted.MaxTimeoutSeconds {
		return false
	}
	if required.Extra == nil {
		return true
	}
	return objectContainsSubset(normalizeJSONValue(required.Extra), normalizeJSONValue(accepted.Extra))
}

// normalizeJSONValue converts typed Go values to the generic shapes produced
// when PaymentPayload is decoded from JSON, avoiding false mismatches such as
// int(2) versus float64(2) inside Extra.
func normalizeJSONValue(value interface{}) interface{} {
	raw, err := json.Marshal(value)
	if err != nil {
		return value
	}
	var normalized interface{}
	if err := json.Unmarshal(raw, &normalized); err != nil {
		return value
	}
	return normalized
}

func objectContainsSubset(expected, actual interface{}) bool {
	expectedMap, expectedOK := expected.(map[string]interface{})
	if !expectedOK {
		return reflect.DeepEqual(expected, actual)
	}
	actualMap, actualOK := actual.(map[string]interface{})
	if !actualOK {
		return false
	}
	for key, value := range expectedMap {
		actualValue, ok := actualMap[key]
		if !ok || !objectContainsSubset(value, actualValue) {
			return false
		}
	}
	return true
}

// receiptOf encodes the settlement response as the base64 receipt carried in
// the payment response headers.
func receiptOf(settled *types.PaymentSettleResponse) (string, error) {
	raw, err := json.Marshal(settled)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(raw), nil
}

// setPaymentResponseHeaders publishes the trusted receipt ahead of any
// headers the downstream handler writes.
func setPaymentResponseHeaders(header http.Header, receipt string) {
	header.Set(HeaderPaymentResponse, receipt)
	header.Set(HeaderXPaymentResponse, receipt)
}

// write402 answers with the x402 challenge: the JSON body carries the
// protocol version, the failure reason, the resource metadata, and the
// accepted requirements; both challenge headers carry the same body base64
// encoded. receipt, when non-empty, publishes a structured settlement
// result (e.g. a pending or failed settlement) alongside the challenge.
// Payment-signaling responses are never cacheable.
func (g *Gate) write402(w http.ResponseWriter, reason string, receipt string) {
	body := struct {
		X402Version int                         `json:"x402Version"`
		Error       string                      `json:"error,omitempty"`
		Resource    types.ResourceInfo          `json:"resource"`
		Accepts     []types.PaymentRequirements `json:"accepts"`
	}{
		X402Version: int(types.X402VersionV2),
		Error:       reason,
		Resource:    g.resource,
		Accepts:     []types.PaymentRequirements{g.requirements},
	}
	raw, err := json.Marshal(body)
	if err != nil {
		http.Error(w, "encode x402 payment requirements", http.StatusInternalServerError)
		return
	}
	encoded := base64.StdEncoding.EncodeToString(raw)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set(HeaderPaymentRequired, encoded)
	w.Header().Set(HeaderXPaymentRequired, encoded)
	if receipt != "" {
		setPaymentResponseHeaders(w.Header(), receipt)
	}
	w.WriteHeader(http.StatusPaymentRequired)
	_, _ = w.Write(raw)
}

// gateResponseWriter keeps the trusted settlement receipt ahead of any
// headers written by the downstream handler.
type gateResponseWriter struct {
	http.ResponseWriter
	receipt     string
	wroteHeader bool
}

func (w *gateResponseWriter) WriteHeader(status int) {
	if w.wroteHeader {
		return
	}
	w.wroteHeader = true
	StripPaymentHeaders(w.Header())
	if w.receipt != "" {
		setPaymentResponseHeaders(w.Header(), w.receipt)
		mergeCachePrivate(w.Header())
	}
	w.ResponseWriter.WriteHeader(status)
}

// mergeCachePrivate adds the private cache directive to a response that
// carries a settlement receipt, so a shared proxy or CDN never serves a paid
// response without the payment gate running. Existing directives are kept.
func mergeCachePrivate(header http.Header) {
	const private = "private"
	existing := strings.TrimSpace(header.Get("Cache-Control"))
	if existing == "" {
		header.Set("Cache-Control", private)
		return
	}
	for _, directive := range strings.Split(existing, ",") {
		if strings.EqualFold(strings.TrimSpace(directive), private) {
			return
		}
	}
	header.Set("Cache-Control", existing+", "+private)
}

func (w *gateResponseWriter) Write(body []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(body)
}

func (w *gateResponseWriter) Flush() {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (w *gateResponseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
