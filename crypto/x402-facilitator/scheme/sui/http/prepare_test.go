package suihttp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gosuda/x402-facilitator/scheme/sui"
	rpcv2 "github.com/gosuda/x402-facilitator/scheme/sui/grpc/pb/sui/rpc/v2"
	"github.com/gosuda/x402-facilitator/types"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	testNetwork = "sui:testnet"
	testSender  = "0x1234"
	testPayTo   = "0xabcd"
	testAmount  = "10000"
	testMaxBody = 64 << 10
)

func testPtr[T any](value T) *T { return &value }

func newTestPrepareHandler(t *testing.T, endpoints []string) http.Handler {
	t.Helper()
	handler, err := NewPrepareHandler(Config{
		Requirements: types.PaymentRequirements{
			Scheme:  string(types.Exact),
			Network: testNetwork,
			Asset:   "USDC",
			Amount:  testAmount,
			PayTo:   testPayTo,
		},
		ResourcePath: "/paid/resource",
		Endpoints:    endpoints,
	})
	require.NoError(t, err)
	return handler
}

func postPrepare(t *testing.T, handler http.Handler, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/x402/prepare", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)
	return recorder
}

func decodePrepareResponse(t *testing.T, recorder *httptest.ResponseRecorder) map[string]interface{} {
	t.Helper()
	var body map[string]interface{}
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &body))
	return body
}

func requireBase64Transaction(t *testing.T, body map[string]interface{}, key string) string {
	t.Helper()
	payload, ok := body[key].(map[string]interface{})
	require.True(t, ok, "response field %s should be a transaction payload", key)
	transaction, ok := payload["transaction"].(string)
	require.True(t, ok, "response field %s.transaction should be a string", key)
	decoded, err := base64.StdEncoding.DecodeString(transaction)
	require.NoError(t, err, "response field %s.transaction should be base64", key)
	require.NotEmpty(t, decoded)
	return transaction
}

// suiLedgerGRPCTestServer is a minimal Sui RPC fake covering exactly the
// calls sui.PreparePayment makes: balance, owned coin objects, and the
// epoch lookup for the expiration window. Unimplemented handlers make the
// corresponding RPC fail, which the error-path tests rely on.
type suiLedgerGRPCTestServer struct {
	rpcv2.UnimplementedLedgerServiceServer
	rpcv2.UnimplementedStateServiceServer
	rpcv2.UnimplementedTransactionExecutionServiceServer

	getServiceInfo   func(context.Context, *rpcv2.GetServiceInfoRequest) (*rpcv2.GetServiceInfoResponse, error)
	getBalance       func(context.Context, *rpcv2.GetBalanceRequest) (*rpcv2.GetBalanceResponse, error)
	listOwnedObjects func(context.Context, *rpcv2.ListOwnedObjectsRequest) (*rpcv2.ListOwnedObjectsResponse, error)
}

func newSuiLedgerGRPCTestServer(t *testing.T, handler *suiLedgerGRPCTestServer) string {
	t.Helper()
	if handler == nil {
		handler = &suiLedgerGRPCTestServer{}
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	server := grpc.NewServer()
	rpcv2.RegisterLedgerServiceServer(server, handler)
	rpcv2.RegisterStateServiceServer(server, handler)
	rpcv2.RegisterTransactionExecutionServiceServer(server, handler)
	t.Cleanup(func() {
		server.Stop()
		_ = listener.Close()
	})
	go func() {
		_ = server.Serve(listener)
	}()
	return "http://" + listener.Addr().String()
}

func (s *suiLedgerGRPCTestServer) GetServiceInfo(ctx context.Context, req *rpcv2.GetServiceInfoRequest) (*rpcv2.GetServiceInfoResponse, error) {
	if s.getServiceInfo == nil {
		return nil, status.Error(codes.Unimplemented, "get service info not implemented")
	}
	return s.getServiceInfo(ctx, req)
}

func (s *suiLedgerGRPCTestServer) GetBalance(ctx context.Context, req *rpcv2.GetBalanceRequest) (*rpcv2.GetBalanceResponse, error) {
	if s.getBalance == nil {
		return nil, status.Error(codes.Unimplemented, "get balance not implemented")
	}
	return s.getBalance(ctx, req)
}

func (s *suiLedgerGRPCTestServer) ListOwnedObjects(ctx context.Context, req *rpcv2.ListOwnedObjectsRequest) (*rpcv2.ListOwnedObjectsResponse, error) {
	if s.listOwnedObjects == nil {
		return nil, status.Error(codes.Unimplemented, "list owned objects not implemented")
	}
	return s.listOwnedObjects(ctx, req)
}

func suiBalanceGRPCResult(addressBalance uint64, coinBalance uint64) *rpcv2.GetBalanceResponse {
	return &rpcv2.GetBalanceResponse{
		Balance: &rpcv2.Balance{
			CoinType:       testPtr(sui.TestnetUSDCType),
			Balance:        testPtr(addressBalance + coinBalance),
			AddressBalance: testPtr(addressBalance),
			CoinBalance:    testPtr(coinBalance),
		},
	}
}

func suiCoinObjectGRPCResult(objectID string, version uint64, balance uint64) *rpcv2.Object {
	return &rpcv2.Object{
		ObjectId:            testPtr(objectID),
		Version:             testPtr(version),
		Digest:              testPtr("11111111111111111111111111111111"),
		ObjectType:          testPtr("0x2::coin::Coin<" + sui.TestnetUSDCType + ">"),
		Balance:             testPtr(balance),
		PreviousTransaction: testPtr(""),
	}
}

func suiScatteredCoinsServer() *suiLedgerGRPCTestServer {
	return &suiLedgerGRPCTestServer{
		getServiceInfo: func(ctx context.Context, req *rpcv2.GetServiceInfoRequest) (*rpcv2.GetServiceInfoResponse, error) {
			return &rpcv2.GetServiceInfoResponse{Epoch: testPtr(uint64(42))}, nil
		},
		getBalance: func(ctx context.Context, req *rpcv2.GetBalanceRequest) (*rpcv2.GetBalanceResponse, error) {
			// The balance RPC reports both legs: the address balance and the
			// sum held in coin objects, which motivates the consolidation.
			return suiBalanceGRPCResult(0, 400000), nil
		},
		listOwnedObjects: func(ctx context.Context, req *rpcv2.ListOwnedObjectsRequest) (*rpcv2.ListOwnedObjectsResponse, error) {
			return &rpcv2.ListOwnedObjectsResponse{
				Objects: []*rpcv2.Object{
					suiCoinObjectGRPCResult("0xaaa1", 7, 300000),
					suiCoinObjectGRPCResult("0xaaa2", 8, 100000),
					// zero-balance objects are skipped by PreparePayment
					suiCoinObjectGRPCResult("0xaaa3", 9, 0),
				},
			}, nil
		},
	}
}

func TestPrepareHandlerPreparesPaymentForScatteredCoins(t *testing.T) {
	endpoint := newSuiLedgerGRPCTestServer(t, suiScatteredCoinsServer())
	handler := newTestPrepareHandler(t, []string{endpoint})

	recorder := postPrepare(t, handler, `{"sender":"`+testSender+`"}`, map[string]string{
		"X-Forwarded-Proto": "https",
		"X-Forwarded-Host":  "paid.example.com",
	})
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	require.Equal(t, "application/json", recorder.Header().Get("Content-Type"))
	require.Equal(t, "no-store", recorder.Header().Get("Cache-Control"))

	body := decodePrepareResponse(t, recorder)
	require.InDelta(t, float64(2), body["x402Version"], 0)

	requirements, ok := body["paymentRequirements"].(map[string]interface{})
	require.True(t, ok, "paymentRequirements should echo the configured requirements")
	require.Equal(t, "exact", requirements["scheme"])
	require.Equal(t, testNetwork, requirements["network"])
	require.Equal(t, "USDC", requirements["asset"])
	require.Equal(t, testAmount, requirements["amount"])
	require.Equal(t, testPayTo, requirements["payTo"])
	// maxTimeoutSeconds <= 0 is published as the default of 60.
	require.InDelta(t, float64(60), requirements["maxTimeoutSeconds"], 0)

	// Clients echo the prepare requirements as their accepted contract, and
	// the gate's accepted-match requires its pinned paymentFlow to survive.
	extras, ok := requirements["extra"].(map[string]interface{})
	require.True(t, ok, "paymentRequirements should carry extra")
	require.Equal(t, "upfront", extras["paymentFlow"])

	resource, ok := body["resource"].(map[string]interface{})
	require.True(t, ok, "resource should describe the paid resource")
	// The advertised URL follows the proxy forwarding headers, not the
	// loopback request the test issued.
	require.Equal(t, "https://paid.example.com/paid/resource", resource["url"])
	require.Equal(t, "text/html", resource["mimeType"])

	requireBase64Transaction(t, body, "prepareTransaction")
	requireBase64Transaction(t, body, "paymentTransaction")
}

func TestPrepareHandlerSkipsPrepareTransactionWhenBalanceCovers(t *testing.T) {
	endpoint := newSuiLedgerGRPCTestServer(t, &suiLedgerGRPCTestServer{
		getServiceInfo: func(ctx context.Context, req *rpcv2.GetServiceInfoRequest) (*rpcv2.GetServiceInfoResponse, error) {
			return &rpcv2.GetServiceInfoResponse{Epoch: testPtr(uint64(42))}, nil
		},
		getBalance: func(ctx context.Context, req *rpcv2.GetBalanceRequest) (*rpcv2.GetBalanceResponse, error) {
			return suiBalanceGRPCResult(20000, 0), nil
		},
		// listOwnedObjects stays unimplemented: a covered sender must not
		// reach the coin-object listing at all.
	})
	handler := newTestPrepareHandler(t, []string{endpoint})

	recorder := postPrepare(t, handler, `{"sender":"`+testSender+`"}`, nil)
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())

	body := decodePrepareResponse(t, recorder)
	require.NotContains(t, body, "prepareTransaction")
	requireBase64Transaction(t, body, "paymentTransaction")
	// Without forwarding headers the URL falls back to the request host.
	resource, ok := body["resource"].(map[string]interface{})
	require.True(t, ok)
	require.Equal(t, "http://example.com/paid/resource", resource["url"])
}

func TestPrepareHandlerRequestErrors(t *testing.T) {
	endpoint := newSuiLedgerGRPCTestServer(t, suiScatteredCoinsServer())
	handler := newTestPrepareHandler(t, []string{endpoint})

	tests := []struct {
		name         string
		body         string
		wantStatus   int
		wantBodyPart string
	}{
		{
			name:         "oversized body",
			body:         `{"sender":"` + testSender + `","pad":"` + strings.Repeat("a", testMaxBody) + `"}`,
			wantStatus:   http.StatusRequestEntityTooLarge,
			wantBodyPart: "too large",
		},
		{
			name:         "malformed JSON",
			body:         `{"sender":`,
			wantStatus:   http.StatusBadRequest,
			wantBodyPart: "invalid JSON",
		},
		{
			name:         "empty sender",
			body:         `{"sender":"  "}`,
			wantStatus:   http.StatusBadRequest,
			wantBodyPart: "sender is required",
		},
		{
			name:         "non-address sender",
			body:         `{"sender":"0xzz"}`,
			wantStatus:   http.StatusBadRequest,
			wantBodyPart: "sender is required",
		},
		{
			name:         "missing sender",
			body:         `{}`,
			wantStatus:   http.StatusBadRequest,
			wantBodyPart: "sender is required",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			recorder := postPrepare(t, handler, tt.body, nil)
			require.Equal(t, tt.wantStatus, recorder.Code)
			responseBody, err := io.ReadAll(recorder.Body)
			require.NoError(t, err)
			require.Contains(t, string(responseBody), tt.wantBodyPart)
		})
	}
}

func TestPrepareHandlerMethodIsPostOnly(t *testing.T) {
	endpoint := newSuiLedgerGRPCTestServer(t, suiScatteredCoinsServer())
	handler := newTestPrepareHandler(t, []string{endpoint})

	req := httptest.NewRequest(http.MethodGet, "/x402/prepare", nil)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)
	require.Equal(t, http.StatusMethodNotAllowed, recorder.Code)
	require.Equal(t, http.MethodPost, recorder.Header().Get("Allow"))
}

func TestPrepareHandlerRPCFailureIsBadGateway(t *testing.T) {
	// getBalance stays unimplemented, so the RPC call itself fails.
	endpoint := newSuiLedgerGRPCTestServer(t, nil)
	handler := newTestPrepareHandler(t, []string{endpoint})

	recorder := postPrepare(t, handler, `{"sender":"`+testSender+`"}`, nil)
	require.Equal(t, http.StatusBadGateway, recorder.Code)
	responseBody, err := io.ReadAll(recorder.Body)
	require.NoError(t, err)
	require.Contains(t, string(responseBody), "prepare payment: ")
}

func TestNewPrepareHandlerRejectsInvalidConfig(t *testing.T) {
	validRequirements := types.PaymentRequirements{
		Scheme:  string(types.Exact),
		Network: testNetwork,
		Asset:   "USDC",
		Amount:  testAmount,
		PayTo:   testPayTo,
	}
	tests := []struct {
		name       string
		config     Config
		wantAction string
	}{
		{
			name: "missing scheme",
			config: Config{
				Requirements: func() types.PaymentRequirements {
					req := validRequirements
					req.Scheme = ""
					return req
				}(),
				ResourcePath: "/paid/resource",
			},
			wantAction: "requirements.scheme",
		},
		{
			name: "missing network",
			config: Config{
				Requirements: func() types.PaymentRequirements {
					req := validRequirements
					req.Network = " "
					return req
				}(),
				ResourcePath: "/paid/resource",
			},
			wantAction: "requirements.network",
		},
		{
			name: "missing asset",
			config: Config{
				Requirements: func() types.PaymentRequirements {
					req := validRequirements
					req.Asset = ""
					return req
				}(),
				ResourcePath: "/paid/resource",
			},
			wantAction: "requirements.asset",
		},
		{
			name: "missing amount",
			config: Config{
				Requirements: func() types.PaymentRequirements {
					req := validRequirements
					req.Amount = ""
					return req
				}(),
				ResourcePath: "/paid/resource",
			},
			wantAction: "requirements.amount",
		},
		{
			name: "zero amount",
			config: Config{
				Requirements: func() types.PaymentRequirements {
					req := validRequirements
					req.Amount = "0"
					return req
				}(),
				ResourcePath: "/paid/resource",
			},
			wantAction: "requirements.amount",
		},
		{
			name: "non-numeric amount",
			config: Config{
				Requirements: func() types.PaymentRequirements {
					req := validRequirements
					req.Amount = "1.5"
					return req
				}(),
				ResourcePath: "/paid/resource",
			},
			wantAction: "requirements.amount",
		},
		{
			name: "conflicting paymentFlow",
			config: Config{
				Requirements: func() types.PaymentRequirements {
					req := validRequirements
					req.Extra = map[string]interface{}{"paymentFlow": "escrow"}
					return req
				}(),
				ResourcePath: "/paid/resource",
			},
			wantAction: "requirements.extra.paymentFlow",
		},
		{
			name: "missing payTo",
			config: Config{
				Requirements: func() types.PaymentRequirements {
					req := validRequirements
					req.PayTo = ""
					return req
				}(),
				ResourcePath: "/paid/resource",
			},
			wantAction: "requirements.payTo",
		},
		{
			name: "invalid payTo address",
			config: Config{
				Requirements: func() types.PaymentRequirements {
					req := validRequirements
					req.PayTo = "0xzz"
					return req
				}(),
				ResourcePath: "/paid/resource",
			},
			wantAction: "requirements.payTo",
		},
		{
			name: "empty resource path",
			config: Config{
				Requirements: validRequirements,
				ResourcePath: "",
			},
			wantAction: "resource path",
		},
		{
			name: "relative resource path",
			config: Config{
				Requirements: validRequirements,
				ResourcePath: "paid/resource",
			},
			wantAction: "resource path",
		},
		{
			name: "negative request timeout",
			config: Config{
				Requirements:   validRequirements,
				ResourcePath:   "/paid/resource",
				RequestTimeout: -time.Second,
			},
			wantAction: "request timeout",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewPrepareHandler(tt.config)
			require.Error(t, err)
			require.Contains(t, err.Error(), "suihttp:")
			require.Contains(t, err.Error(), tt.wantAction)
		})
	}
}

func TestNewPrepareHandlerAcceptsPositiveRequestTimeout(t *testing.T) {
	handler, err := NewPrepareHandler(Config{
		Requirements: types.PaymentRequirements{
			Scheme:  string(types.Exact),
			Network: testNetwork,
			Asset:   "USDC",
			Amount:  testAmount,
			PayTo:   testPayTo,
		},
		ResourcePath:   "/paid/resource",
		RequestTimeout: 5 * time.Second,
	})
	require.NoError(t, err)
	require.NotNil(t, handler)
}

func TestNewPrepareHandlerDoesNotMutateConfigExtra(t *testing.T) {
	extra := map[string]interface{}{"asset": "USDC"}
	handler, err := NewPrepareHandler(Config{
		Requirements: types.PaymentRequirements{
			Scheme:  string(types.Exact),
			Network: testNetwork,
			Asset:   "USDC",
			Amount:  testAmount,
			PayTo:   testPayTo,
			Extra:   extra,
		},
		ResourcePath: "/paid/resource",
	})
	require.NoError(t, err)
	require.NotNil(t, handler)

	// The pinned paymentFlow belongs to the handler's normalized copy;
	// the caller's config map must come out exactly as it went in.
	require.Equal(t, map[string]interface{}{"asset": "USDC"}, extra)
}

// Applications serving one shared prepare endpoint over multiple paid
// contracts skip the HTTP facade: they decode the request, select the
// contract, and call Preparer.WritePrepare directly.
func TestPreparerWritePrepareServesSelectedContract(t *testing.T) {
	endpoint := newSuiLedgerGRPCTestServer(t, suiScatteredCoinsServer())
	preparer, err := NewPreparer(Config{
		Requirements: types.PaymentRequirements{
			Scheme:  string(types.Exact),
			Network: testNetwork,
			Asset:   "USDC",
			Amount:  testAmount,
			PayTo:   testPayTo,
		},
		ResourcePath: "/paid/resource",
		Endpoints:    []string{endpoint},
	})
	require.NoError(t, err)

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/x402/prepare", nil)
	request.Header.Set("X-Forwarded-Proto", "https")
	request.Header.Set("X-Forwarded-Host", "paid.example.com")
	preparer.WritePrepare(recorder, request, testSender)

	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	require.Equal(t, "application/json", recorder.Header().Get("Content-Type"))
	require.Equal(t, "no-store", recorder.Header().Get("Cache-Control"))

	body := decodePrepareResponse(t, recorder)
	require.InDelta(t, float64(2), body["x402Version"], 0)
	requirements, ok := body["paymentRequirements"].(map[string]interface{})
	require.True(t, ok, "paymentRequirements should echo the configured requirements")
	extras, ok := requirements["extra"].(map[string]interface{})
	require.True(t, ok, "paymentRequirements should carry extra")
	require.Equal(t, "upfront", extras["paymentFlow"])
	resource, ok := body["resource"].(map[string]interface{})
	require.True(t, ok, "resource should describe the paid resource")
	require.Equal(t, "https://paid.example.com/paid/resource", resource["url"])
	requireBase64Transaction(t, body, "prepareTransaction")
	requireBase64Transaction(t, body, "paymentTransaction")
}

func TestPreparerWritePrepareRejectsMissingSender(t *testing.T) {
	preparer, err := NewPreparer(Config{
		Requirements: types.PaymentRequirements{
			Scheme:  string(types.Exact),
			Network: testNetwork,
			Asset:   "USDC",
			Amount:  testAmount,
			PayTo:   testPayTo,
		},
		ResourcePath: "/paid/resource",
	})
	require.NoError(t, err)

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/x402/prepare", nil)
	preparer.WritePrepare(recorder, request, "   ")

	require.Equal(t, http.StatusBadRequest, recorder.Code)
	require.Contains(t, recorder.Body.String(), "sender is required")
}
