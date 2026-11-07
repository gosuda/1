package x402http

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/gosuda/x402-facilitator/types"
)

func TestWrapParsesInboundPaymentEncodings(t *testing.T) {
	raw, err := json.Marshal(v2Payload())
	require.NoError(t, err)

	cases := []struct {
		name   string
		header string
		value  string
	}{
		{"direct json", HeaderPaymentSignature, string(raw)},
		{"base64 std wrapped", HeaderPaymentSignature, base64.StdEncoding.EncodeToString(raw)},
		{"base64 url-safe wrapped", HeaderPaymentSignature, base64.URLEncoding.EncodeToString(raw)},
		{"legacy x-payment fallback", HeaderXPayment, base64.StdEncoding.EncodeToString(raw)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gate, stub := newTestGate(t, nil)
			handler := gate.Wrap(okHandler)

			req := httptest.NewRequest(http.MethodGet, "/resource", nil)
			req.Header.Set(tc.header, tc.value)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			require.Equal(t, http.StatusOK, rec.Code)
			require.Equal(t, 1, stub.settleCalls)
			require.Equal(t, int(types.X402VersionV2), stub.payload.X402Version)
		})
	}
}

func TestWrapRejectsUndecodableAndLegacyVersions(t *testing.T) {
	cases := []struct {
		name  string
		value string
	}{
		{"garbage", "!!!not-base64-or-json!!!"},
		{"v1 payload", `{"x402Version":1,"payload":{}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gate, stub := newTestGate(t, nil)
			downstream := false
			handler := gate.Wrap(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { downstream = true }))

			req := httptest.NewRequest(http.MethodGet, "/resource", nil)
			req.Header.Set(HeaderPaymentSignature, tc.value)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			require.Equal(t, http.StatusPaymentRequired, rec.Code)
			require.False(t, downstream)
			require.Zero(t, stub.settleCalls)
			body := decodeChallenge(t, rec)
			require.Equal(t, "invalid payment payload", body.Error)
		})
	}
}

func TestWrapPrefersCanonicalSignatureOverLegacyHeader(t *testing.T) {
	gate, stub := newTestGate(t, nil)
	handler := gate.Wrap(okHandler)

	raw, err := json.Marshal(v2Payload())
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodGet, "/resource", nil)
	req.Header.Set(HeaderPaymentSignature, string(raw))
	req.Header.Set(HeaderXPayment, "!!!not-a-payment!!!")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, 1, stub.settleCalls, "canonical PAYMENT-SIGNATURE must win over the legacy fallback")
}
