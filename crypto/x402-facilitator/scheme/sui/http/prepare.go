// Package suihttp serves the payer-facing half of the Sui gasless-stablecoin
// x402 flow over plain net/http: a POST prepare endpoint backed by
// sui.PreparePayment and the shared browser wallet client. It is
// chain-scoped by design; the chain-blind 402 gate lives in resource/http.
package suihttp

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	x402http "github.com/gosuda/x402-facilitator/resource/http"
	"github.com/gosuda/x402-facilitator/scheme/sui"
	"github.com/gosuda/x402-facilitator/types"
)

const (
	// defaultRequestTimeout bounds a prepare call when Config.RequestTimeout
	// is unset. The bound holds only for RPC clients that honor context
	// cancellation; it cannot preempt one that ignores its context.
	defaultRequestTimeout = 30 * time.Second

	// defaultMaxTimeoutSeconds is published as maxTimeoutSeconds when the
	// configured requirements leave it unset (mirror resource/http).
	defaultMaxTimeoutSeconds = 60

	// maxPrepareBodyBytes caps the prepare request body: the only field is
	// the sender address, so anything near this size is abuse.
	maxPrepareBodyBytes = 64 << 10
)

// Config owns one Sui x402 prepare contract.
type Config struct {
	// Requirements is the exact Sui gasless-stablecoin payment the payer
	// prepares for. Scheme, Network, Asset, Amount, PayTo are required.
	Requirements types.PaymentRequirements

	// ResourcePath is the path of the paid resource advertised in the
	// prepare response. Required, must start with "/".
	ResourcePath string

	// ResourceDescription is advertised verbatim in the prepare response.
	ResourceDescription string

	// ResourceMimeType defaults to "text/html".
	ResourceMimeType string

	// Endpoints are optional Sui RPC endpoints; empty uses network defaults.
	Endpoints []string

	// RequestTimeout bounds each prepare call. Zero defaults to 30s;
	// negative is a configuration error.
	RequestTimeout time.Duration
}

// PreparePaymentRequest is the prepare request body. Unknown JSON fields are
// skipped, so richer browser payloads keep working.
type PreparePaymentRequest struct {
	Sender string `json:"sender"`
}

// TransactionPayload carries one base64-encoded unsigned Sui transaction.
type TransactionPayload struct {
	Transaction string `json:"transaction"`
}

// PreparePaymentResponse is the prepare response body. The wire shape is
// byte-compatible with the Portal x402 prepare response: both transactions
// are unsigned base64 TransactionData bytes the payer signs with their own
// Sui runtime.
type PreparePaymentResponse struct {
	X402Version         int                       `json:"x402Version"`
	PaymentRequirements types.PaymentRequirements `json:"paymentRequirements"`
	Resource            *types.ResourceInfo       `json:"resource,omitempty"`
	PrepareTransaction  *TransactionPayload       `json:"prepareTransaction,omitempty"`
	PaymentTransaction  TransactionPayload        `json:"paymentTransaction"`
}

// NewPreparer validates cfg and returns the reusable prepare operation.
// cfg is not mutated; the preparer keeps a normalized copy of the
// requirements with the maxTimeoutSeconds default and the gate's upfront
// paymentFlow applied, so the echo satisfies the gate's accepted-requirements
// match. Applications that serve one shared prepare endpoint over multiple
// paid contracts decode the request, select the contract themselves, and
// call Preparer.WritePrepare directly; NewPrepareHandler is the thin
// single-contract HTTP facade over it.
func NewPreparer(cfg Config) (*Preparer, error) {
	requirements := cfg.Requirements
	switch {
	case strings.TrimSpace(requirements.Scheme) == "":
		return nil, errors.New("suihttp: requirements.scheme is required")
	case strings.TrimSpace(requirements.Network) == "":
		return nil, errors.New("suihttp: requirements.network is required")
	case strings.TrimSpace(requirements.Asset) == "":
		return nil, errors.New("suihttp: requirements.asset is required")
	case strings.TrimSpace(requirements.Amount) == "":
		return nil, errors.New("suihttp: requirements.amount is required")
	case strings.TrimSpace(requirements.PayTo) == "":
		return nil, errors.New("suihttp: requirements.payTo is required")
	}
	if sui.NormalizeAddress(requirements.PayTo) == "" {
		return nil, errors.New("suihttp: requirements.payTo must be a valid Sui address")
	}
	// Configuration errors die here, not per request: PreparePayment would
	// reject the same amount after RPC round trips the caller pays for.
	if amount, err := strconv.ParseUint(strings.TrimSpace(requirements.Amount), 10, 64); err != nil || amount == 0 {
		return nil, fmt.Errorf("suihttp: requirements.amount %q must be a positive integer", requirements.Amount)
	}
	if requirements.MaxTimeoutSeconds <= 0 {
		requirements.MaxTimeoutSeconds = defaultMaxTimeoutSeconds
	}
	// Copy Extra so normalization never touches the caller's map, then pin
	// the flow the chain-blind gate publishes: clients echo the prepare
	// requirements as their accepted contract, and the gate rejects
	// payments whose accepted Extra drops that paymentFlow.
	extra := make(map[string]interface{}, len(requirements.Extra)+1)
	for key, value := range requirements.Extra {
		extra[key] = value
	}
	if existing, ok := extra["paymentFlow"]; ok && existing != x402http.PaymentFlowUpfront {
		return nil, fmt.Errorf("suihttp: requirements.extra.paymentFlow %v conflicts with the %q payment flow", existing, x402http.PaymentFlowUpfront)
	}
	extra["paymentFlow"] = x402http.PaymentFlowUpfront
	requirements.Extra = extra
	resourcePath := strings.TrimSpace(cfg.ResourcePath)
	if !strings.HasPrefix(resourcePath, "/") {
		return nil, fmt.Errorf("suihttp: resource path %q must start with /", cfg.ResourcePath)
	}
	if cfg.RequestTimeout < 0 {
		return nil, errors.New("suihttp: request timeout must not be negative")
	}
	requestTimeout := cfg.RequestTimeout
	if requestTimeout == 0 {
		requestTimeout = defaultRequestTimeout
	}
	resourceMimeType := strings.TrimSpace(cfg.ResourceMimeType)
	resourceMimeType = cmp.Or(resourceMimeType, "text/html")
	return &Preparer{
		requirements: requirements,
		resourceInfo: types.ResourceInfo{
			Description: strings.TrimSpace(cfg.ResourceDescription),
			MimeType:    resourceMimeType,
		},
		resourcePath:   resourcePath,
		endpoints:      cfg.Endpoints,
		requestTimeout: requestTimeout,
	}, nil
}

// Preparer executes the reusable Sui prepare operation for one payment
// contract: it validates the sender, delegates transaction construction to
// sui.PreparePayment, and writes the payer-facing prepare response. It
// never signs or submits anything.
type Preparer struct {
	requirements   types.PaymentRequirements
	resourceInfo   types.ResourceInfo
	resourcePath   string
	endpoints      []string
	requestTimeout time.Duration
}

// NewPrepareHandler validates cfg and returns a POST-only handler that
// decodes the sender-only prepare request body and serves one fixed
// payment contract. Applications routing a shared prepare endpoint over
// multiple contracts should build a Preparer per contract and call
// WritePrepare after their own route selection.
func NewPrepareHandler(cfg Config) (http.Handler, error) {
	preparer, err := NewPreparer(cfg)
	if err != nil {
		return nil, err
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req PreparePaymentRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxPrepareBodyBytes)).Decode(&req); err != nil {
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
				return
			}
			http.Error(w, "invalid JSON body", http.StatusBadRequest)
			return
		}
		preparer.WritePrepare(w, r, req.Sender)
	}), nil
}

// WritePrepare runs the prepare operation for sender and writes the
// prepare response. The advertised resource URL follows the request's
// proxy forwarding headers. sender is the raw request field; it is
// normalized here.
func (p *Preparer) WritePrepare(w http.ResponseWriter, r *http.Request, sender string) {
	if p == nil {
		http.Error(w, "prepare is not configured", http.StatusInternalServerError)
		return
	}
	sender = sui.NormalizeAddress(sender)
	if sender == "" {
		http.Error(w, "sender is required", http.StatusBadRequest)
		return
	}

	ctx := r.Context()
	cancel := func() {}
	if p.requestTimeout > 0 {
		ctx, cancel = context.WithTimeout(ctx, p.requestTimeout)
	}
	defer cancel()
	prepared, err := sui.PreparePayment(ctx, sui.GaslessStablecoinObjectBalancePayment{
		Sender:    sender,
		Recipient: p.requirements.PayTo,
		Network:   p.requirements.Network,
		Asset:     p.requirements.Asset,
		Amount:    p.requirements.Amount,
		Endpoints: p.endpoints,
	})
	if err != nil {
		http.Error(w, "prepare payment: "+err.Error(), http.StatusBadGateway)
		return
	}

	response := PreparePaymentResponse{
		X402Version:         int(types.X402VersionV2),
		PaymentRequirements: p.requirements,
		Resource: &types.ResourceInfo{
			URL:         publicURLForPath(r, p.resourcePath),
			Description: p.resourceInfo.Description,
			MimeType:    p.resourceInfo.MimeType,
		},
		PaymentTransaction: TransactionPayload{Transaction: prepared.PaymentTransaction},
	}
	if prepared.ConsolidationTransaction != "" {
		response.PrepareTransaction = &TransactionPayload{Transaction: prepared.ConsolidationTransaction}
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(response)
}

// publicURLForPath resolves a public absolute URL from request forwarding
// headers so the advertised resource URL matches how the payer reached the
// server; it returns the bare path when no host is known.
func publicURLForPath(r *http.Request, path string) string {
	if r == nil {
		return ""
	}
	scheme, _, _ := strings.Cut(r.Header.Get("X-Forwarded-Proto"), ",")
	scheme = strings.ToLower(strings.TrimSpace(scheme))
	if scheme == "" {
		if r.TLS != nil {
			scheme = "https"
		} else {
			scheme = "http"
		}
	}
	host, _, _ := strings.Cut(r.Header.Get("X-Forwarded-Host"), ",")
	host = strings.TrimSpace(host)
	host = cmp.Or(host, strings.TrimSpace(r.Host))
	if host == "" {
		return path
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	return scheme + "://" + host + path
}
