package x402http

import "net/http"

// x402 wire headers. PAYMENT-SIGNATURE, PAYMENT-REQUIRED, and
// PAYMENT-RESPONSE are the canonical v2 names; the X-PAYMENT-* trio is the
// legacy compatibility form, published alongside the canonical response
// headers and accepted as an inbound fallback only.
const (
	HeaderPaymentSignature = "PAYMENT-SIGNATURE"
	HeaderXPayment         = "X-PAYMENT"
	HeaderPaymentRequired  = "PAYMENT-REQUIRED"
	HeaderXPaymentRequired = "X-PAYMENT-REQUIRED"
	HeaderPaymentResponse  = "PAYMENT-RESPONSE"
	HeaderXPaymentResponse = "X-PAYMENT-RESPONSE"
)

var paymentHeaders = [...]string{
	HeaderXPayment,
	HeaderPaymentSignature,
	HeaderPaymentRequired,
	HeaderXPaymentRequired,
	HeaderPaymentResponse,
	HeaderXPaymentResponse,
}

// StripPaymentHeaders deletes every x402 payment header from h.
func StripPaymentHeaders(h http.Header) {
	for _, name := range paymentHeaders {
		h.Del(name)
	}
}
