package types

// Specification: https://github.com/x402-foundation/x402/blob/main/specs/x402-specification.md
//
// These structs are redeclared locally (not aliased from internal/sdk) so
// swaggo/swag can parse them during `make generate-api`; drift against the
// upstream SDK is guarded by TestV2WireCompatibility.

// Network is a CAIP-2 network identifier (e.g., "eip155:84532" for Base Sepolia).
type Network string

// PaymentRequirements defines the structure for accepted payments by the resource server.
// This corresponds to the server's response in the 402 Payment Required flow.
type PaymentRequirements struct {
	// Scheme of the payment protocol to use (e.g., "exact")
	Scheme string `json:"scheme"`
	// Network in CAIP-2 format (e.g., "eip155:84532")
	Network string `json:"network"`
	// Asset is the token contract address (EVM) or mint (SVM) being paid
	Asset string `json:"asset"`
	// Amount required to pay for the resource in atomic units, as a decimal string
	Amount string `json:"amount"`
	// Address to pay value to
	PayTo string `json:"payTo"`
	// Maximum time in seconds for the resource server to respond
	MaxTimeoutSeconds int `json:"maxTimeoutSeconds"`
	// Scheme-specific metadata (e.g., EIP-712 domain name and version)
	Extra map[string]interface{} `json:"extra,omitempty"`
}

// PaymentPayload represents the data the client sends in the X-PAYMENT header.
// In v2 the scheme and network fields live inside Accepted, not at the top level.
type PaymentPayload struct {
	// Version of the x402 payment protocol
	X402Version int `json:"x402Version"`
	// Scheme-specific payment material (e.g., an EIP-3009 authorization + signature)
	Payload map[string]interface{} `json:"payload"`
	// The PaymentRequirements the client is paying against
	Accepted PaymentRequirements `json:"accepted"`
	// Resource describes the resource being paid for (optional)
	Resource *ResourceInfo `json:"resource,omitempty"`
	// Protocol extension data (optional)
	Extensions map[string]interface{} `json:"extensions,omitempty"`
}

// ResourceInfo describes the resource being paid for. In v2 these fields moved
// out of PaymentRequirements and onto PaymentPayload.
type ResourceInfo struct {
	URL         string `json:"url"`
	Description string `json:"description,omitempty"`
	MimeType    string `json:"mimeType,omitempty"`
}

// PaymentVerifyRequest is the request body sent to facilitator's /verify endpoint.
type PaymentVerifyRequest struct {
	PaymentPayload      PaymentPayload      `json:"paymentPayload"`
	PaymentRequirements PaymentRequirements `json:"paymentRequirements"`
}

// PaymentVerifyResponse is the response returned from the /verify endpoint.
type PaymentVerifyResponse struct {
	// Whether the payment payload is valid
	IsValid bool `json:"isValid"`
	// Short machine-readable code (e.g., "insufficient_balance") when IsValid is false
	InvalidReason string `json:"invalidReason,omitempty"`
	// Human-readable explanation when IsValid is false
	InvalidMessage string `json:"invalidMessage,omitempty"`
	// Address that signed the payment, when recoverable
	Payer string `json:"payer,omitempty"`
}

// PaymentSettleRequest is the request body sent to facilitator's /settle endpoint.
type PaymentSettleRequest struct {
	PaymentPayload      PaymentPayload      `json:"paymentPayload"`
	PaymentRequirements PaymentRequirements `json:"paymentRequirements"`
}

// PaymentSettleResponse is the response from the /settle endpoint.
type PaymentSettleResponse struct {
	// Whether the payment broadcast was accepted
	Success bool `json:"success"`
	// Short machine-readable error code when Success is false
	ErrorReason string `json:"errorReason,omitempty"`
	// Human-readable error message when Success is false
	ErrorMessage string `json:"errorMessage,omitempty"`
	// Address that signed the payment, when recoverable
	Payer string `json:"payer,omitempty"`
	// Transaction hash of the settled payment (replaces the v1 field "txHash")
	Transaction string `json:"transaction"`
	// Network in CAIP-2 format (replaces the v1 field "networkId")
	Network Network `json:"network"`
}

// SupportedKind represents a supported scheme and network pair
// used in the /supported endpoint.
type SupportedKind struct {
	// Protocol version supported for this pair
	X402Version int `json:"x402Version"`
	// Scheme of the payment protocol (e.g., "exact")
	Scheme string `json:"scheme"`
	// Network in CAIP-2 format
	Network string `json:"network"`
	// Scheme-specific metadata (e.g., SVM fee payer address)
	Extra map[string]interface{} `json:"extra,omitempty"`
}

// SupportedResponse is the response structure returned from the /supported endpoint.
type SupportedResponse struct {
	// Accepted (scheme, network) pairs
	Kinds []SupportedKind `json:"kinds"`
	// Protocol extensions this facilitator understands
	Extensions []string `json:"extensions"`
	// CAIP-2 family pattern (e.g., "eip155:*") → fee-payer addresses
	Signers map[string][]string `json:"signers"`
}
