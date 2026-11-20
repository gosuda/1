package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/require"

	"github.com/gosuda/x402-facilitator/scheme"
	"github.com/gosuda/x402-facilitator/types"
)

// stubFacilitator records calls so tests can assert the version gate runs
// before any facilitator logic.
type stubFacilitator struct {
	verifyCalls int
	settleCalls int
}

func (f *stubFacilitator) Verify(_ context.Context, _ *types.PaymentPayload, _ *types.PaymentRequirements) (*types.PaymentVerifyResponse, error) {
	f.verifyCalls++
	return &types.PaymentVerifyResponse{IsValid: true, Payer: "0xpayer"}, nil
}

func (f *stubFacilitator) Settle(_ context.Context, _ *types.PaymentPayload, _ *types.PaymentRequirements) (*types.PaymentSettleResponse, error) {
	f.settleCalls++
	return &types.PaymentSettleResponse{Success: true, Transaction: "0xtx", Network: "eip155:84532"}, nil
}

func (f *stubFacilitator) Supported() *types.SupportedResponse {
	return &types.SupportedResponse{Kinds: []types.SupportedKind{{
		X402Version: int(types.X402VersionV2),
		Scheme:      string(types.Exact),
		Network:     "eip155:84532",
	}}, Extensions: []string{}, Signers: map[string][]string{}}
}

var _ scheme.Facilitator = (*stubFacilitator)(nil)

func postJSON(t *testing.T, h http.Handler, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(raw))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestVerifyRejectsUnsupportedX402Version(t *testing.T) {
	stub := &stubFacilitator{}
	srv := NewServer(stub)

	for _, version := range []int{0, 1} {
		rec := postJSON(t, srv, "/verify", types.PaymentVerifyRequest{
			PaymentPayload: types.PaymentPayload{X402Version: version},
		})
		require.Equal(t, http.StatusBadRequest, rec.Code, "x402Version %d must be rejected", version)
		require.Contains(t, rec.Body.String(), "unsupported x402Version")
	}
	require.Zero(t, stub.verifyCalls, "facilitator must not be invoked for rejected versions")

	rec := postJSON(t, srv, "/verify", types.PaymentVerifyRequest{
		PaymentPayload: types.PaymentPayload{X402Version: int(types.X402VersionV2)},
	})
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, 1, stub.verifyCalls)
}

func TestSettleRejectsUnsupportedX402Version(t *testing.T) {
	stub := &stubFacilitator{}
	srv := NewServer(stub)

	for _, version := range []int{0, 1} {
		rec := postJSON(t, srv, "/settle", types.PaymentSettleRequest{
			PaymentPayload: types.PaymentPayload{X402Version: version},
		})
		require.Equal(t, http.StatusBadRequest, rec.Code, "x402Version %d must be rejected", version)
		require.Contains(t, rec.Body.String(), "unsupported x402Version")
	}
	require.Zero(t, stub.settleCalls, "facilitator must not be invoked for rejected versions")

	rec := postJSON(t, srv, "/settle", types.PaymentSettleRequest{
		PaymentPayload: types.PaymentPayload{X402Version: int(types.X402VersionV2)},
	})
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, 1, stub.settleCalls)
}
