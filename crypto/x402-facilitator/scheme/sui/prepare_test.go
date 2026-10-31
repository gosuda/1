package sui

import (
	"context"
	"encoding/base64"
	"math"
	"testing"

	rpcv2 "github.com/gosuda/x402-facilitator/scheme/sui/grpc/pb/sui/rpc/v2"
	bcs "github.com/iotaledger/bcs-go"
	"github.com/stretchr/testify/require"
)

func decodePreparedLeg(t *testing.T, txB64 string) gaslessStablecoinTransactionData {
	t.Helper()
	txBytes, err := base64.StdEncoding.DecodeString(txB64)
	require.NoError(t, err)
	txData, err := bcs.Unmarshal[gaslessStablecoinTransactionData](txBytes)
	require.NoError(t, err)
	require.NotNil(t, txData.V1)
	return txData
}

func TestPreparePaymentBuildsConsolidationAndPaymentLegs(t *testing.T) {
	signer := newTestSigner(t)
	recipient := "0xabc"
	var methods []string
	endpoint, closeServer := newSuiTransactionGRPCTestServer(t, &suiTransactionGRPCTestServer{
		methods: &methods,
		getServiceInfo: func(ctx context.Context, req *rpcv2.GetServiceInfoRequest) (*rpcv2.GetServiceInfoResponse, error) {
			return &rpcv2.GetServiceInfoResponse{Epoch: ptr(uint64(42))}, nil
		},
		getBalance: func(ctx context.Context, req *rpcv2.GetBalanceRequest) (*rpcv2.GetBalanceResponse, error) {
			require.Equal(t, signer.Address(), req.GetOwner())
			require.Equal(t, TestnetUSDCType, req.GetCoinType())
			return suiBalanceGRPCResult(TestnetUSDCType, 0, 400000), nil
		},
		listOwnedObjects: func(ctx context.Context, req *rpcv2.ListOwnedObjectsRequest) (*rpcv2.ListOwnedObjectsResponse, error) {
			require.Equal(t, signer.Address(), req.GetOwner())
			return &rpcv2.ListOwnedObjectsResponse{
				Objects: []*rpcv2.Object{
					suiCoinObjectGRPCResult("0x1111", 7, 300000, TestnetUSDCType),
					suiCoinObjectGRPCResult("0x2222", 8, 100000, TestnetUSDCType),
					suiCoinObjectGRPCResult("0x3333", 9, 0, TestnetUSDCType),
				},
			}, nil
		},
		execute: func(ctx context.Context, req *rpcv2.ExecuteTransactionRequest) (*rpcv2.ExecuteTransactionResponse, error) {
			t.Fatal("PreparePayment must not execute transactions")
			return nil, nil
		},
	})
	defer closeServer()

	prepared, err := PreparePayment(context.Background(), GaslessStablecoinObjectBalancePayment{
		Sender:    signer.Address(),
		Recipient: recipient,
		Network:   "sui:testnet",
		Asset:     "USDC",
		Amount:    "10000",
		Endpoints: []string{endpoint},
	})
	require.NoError(t, err)
	require.Equal(t, "10000", prepared.PaymentAmount)
	require.Equal(t, "400000", prepared.ConsolidationAmount)
	require.Len(t, prepared.CoinObjects, 2)
	require.NotEmpty(t, prepared.ConsolidationTransaction)
	require.NotEmpty(t, prepared.PaymentTransaction)
	require.Equal(t, []string{"GetBalance", "ListOwnedObjects", "GetServiceInfo"}, methods)

	senderAddress := NormalizeAddress(signer.Address())
	consolidation := decodePreparedLeg(t, prepared.ConsolidationTransaction)
	require.Equal(t, senderAddress, consolidation.V1.Sender.String())
	programmable := consolidation.V1.Kind.ProgrammableTransaction
	require.NotNil(t, programmable)
	require.Len(t, programmable.Inputs, 3)
	require.Len(t, programmable.Commands, 2)
	for i, command := range programmable.Commands {
		require.NotNil(t, command.MoveCall)
		require.Equal(t, "coin", command.MoveCall.Module)
		require.Equal(t, "send_funds", command.MoveCall.Function)
		require.Len(t, command.MoveCall.Arguments, 2)
		require.Equal(t, uint16(i), *command.MoveCall.Arguments[0].Input)
		require.Equal(t, uint16(2), *command.MoveCall.Arguments[1].Input)
	}
	require.NotNil(t, programmable.Inputs[2].Pure)
	senderParsed, err := ParseAddress(senderAddress)
	require.NoError(t, err)
	senderBytes, err := bcs.Marshal(&senderParsed)
	require.NoError(t, err)
	require.Equal(t, senderBytes, programmable.Inputs[2].Pure.Bytes)

	payment := decodePreparedLeg(t, prepared.PaymentTransaction)
	require.Equal(t, senderAddress, payment.V1.Sender.String())
	paymentProgrammable := payment.V1.Kind.ProgrammableTransaction
	require.NotNil(t, paymentProgrammable)
	require.Len(t, paymentProgrammable.Inputs, 2)
	require.NotNil(t, paymentProgrammable.Inputs[0].FundsWithdrawal)
	require.NotNil(t, paymentProgrammable.Inputs[0].FundsWithdrawal.Reservation.MaxAmountU64)
	require.Equal(t, uint64(10000), *paymentProgrammable.Inputs[0].FundsWithdrawal.Reservation.MaxAmountU64)
	require.NotNil(t, paymentProgrammable.Inputs[1].Pure)
	recipientParsed, err := ParseAddress(recipient)
	require.NoError(t, err)
	recipientBytes, err := bcs.Marshal(&recipientParsed)
	require.NoError(t, err)
	require.Equal(t, recipientBytes, paymentProgrammable.Inputs[1].Pure.Bytes)
	require.Len(t, paymentProgrammable.Commands, 2)
	require.Equal(t, "balance", paymentProgrammable.Commands[0].MoveCall.Module)
	require.Equal(t, "redeem_funds", paymentProgrammable.Commands[0].MoveCall.Function)
	require.Equal(t, "balance", paymentProgrammable.Commands[1].MoveCall.Module)
	require.Equal(t, "send_funds", paymentProgrammable.Commands[1].MoveCall.Function)
	require.Equal(t, consolidation.V1.Expiration, payment.V1.Expiration)
}

func TestPreparePaymentSkipsConsolidationWhenAddressBalanceCovers(t *testing.T) {
	signer := newTestSigner(t)
	var methods []string
	endpoint, closeServer := newSuiTransactionGRPCTestServer(t, &suiTransactionGRPCTestServer{
		methods: &methods,
		getServiceInfo: func(ctx context.Context, req *rpcv2.GetServiceInfoRequest) (*rpcv2.GetServiceInfoResponse, error) {
			return &rpcv2.GetServiceInfoResponse{Epoch: ptr(uint64(42))}, nil
		},
		getBalance: func(ctx context.Context, req *rpcv2.GetBalanceRequest) (*rpcv2.GetBalanceResponse, error) {
			return suiBalanceGRPCResult(TestnetUSDCType, 10000, 400000), nil
		},
		listOwnedObjects: func(ctx context.Context, req *rpcv2.ListOwnedObjectsRequest) (*rpcv2.ListOwnedObjectsResponse, error) {
			t.Fatal("coin object lookup should be skipped when address balance is sufficient")
			return nil, nil
		},
		execute: func(ctx context.Context, req *rpcv2.ExecuteTransactionRequest) (*rpcv2.ExecuteTransactionResponse, error) {
			t.Fatal("PreparePayment must not execute transactions")
			return nil, nil
		},
	})
	defer closeServer()

	prepared, err := PreparePayment(context.Background(), GaslessStablecoinObjectBalancePayment{
		Sender:    signer.Address(),
		Recipient: "0xabc",
		Network:   "sui:testnet",
		Asset:     "USDC",
		Amount:    "10000",
		Endpoints: []string{endpoint},
	})
	require.NoError(t, err)
	require.Empty(t, prepared.ConsolidationTransaction)
	require.Equal(t, "0", prepared.ConsolidationAmount)
	require.Nil(t, prepared.CoinObjects)
	require.NotEmpty(t, prepared.PaymentTransaction)
	require.Equal(t, "10000", prepared.PaymentAmount)
	require.Equal(t, []string{"GetBalance", "GetServiceInfo"}, methods)
}

func TestPreparePaymentRejectsInsufficientTotalBalance(t *testing.T) {
	signer := newTestSigner(t)
	var methods []string
	endpoint, closeServer := newSuiTransactionGRPCTestServer(t, &suiTransactionGRPCTestServer{
		methods: &methods,
		getBalance: func(ctx context.Context, req *rpcv2.GetBalanceRequest) (*rpcv2.GetBalanceResponse, error) {
			return suiBalanceGRPCResult(TestnetUSDCType, 1000, 2000), nil
		},
		getServiceInfo: func(ctx context.Context, req *rpcv2.GetServiceInfoRequest) (*rpcv2.GetServiceInfoResponse, error) {
			t.Fatal("expiration should not be resolved when total balance is insufficient")
			return nil, nil
		},
		listOwnedObjects: func(ctx context.Context, req *rpcv2.ListOwnedObjectsRequest) (*rpcv2.ListOwnedObjectsResponse, error) {
			t.Fatal("coin objects should not be listed when total balance is insufficient")
			return nil, nil
		},
	})
	defer closeServer()

	prepared, err := PreparePayment(context.Background(), GaslessStablecoinObjectBalancePayment{
		Sender:    signer.Address(),
		Recipient: "0xabc",
		Network:   "sui:testnet",
		Asset:     "USDC",
		Amount:    "10000",
		Endpoints: []string{endpoint},
	})
	require.Nil(t, prepared)
	require.ErrorContains(t, err, "insufficient")
	require.Equal(t, []string{"GetBalance"}, methods)
}

func TestPreparePaymentRejectsInsufficientCoinObjectSum(t *testing.T) {
	signer := newTestSigner(t)
	var methods []string
	endpoint, closeServer := newSuiTransactionGRPCTestServer(t, &suiTransactionGRPCTestServer{
		methods: &methods,
		getBalance: func(ctx context.Context, req *rpcv2.GetBalanceRequest) (*rpcv2.GetBalanceResponse, error) {
			return suiBalanceGRPCResult(TestnetUSDCType, 1000, 50000), nil
		},
		listOwnedObjects: func(ctx context.Context, req *rpcv2.ListOwnedObjectsRequest) (*rpcv2.ListOwnedObjectsResponse, error) {
			return &rpcv2.ListOwnedObjectsResponse{
				Objects: []*rpcv2.Object{
					suiCoinObjectGRPCResult("0x1111", 7, 3000, TestnetUSDCType),
					suiCoinObjectGRPCResult("0x2222", 8, 2000, TestnetUSDCType),
					suiCoinObjectGRPCResult("0x3333", 9, 1000, TestnetUSDCType),
				},
			}, nil
		},
	})
	defer closeServer()

	prepared, err := PreparePayment(context.Background(), GaslessStablecoinObjectBalancePayment{
		Sender:    signer.Address(),
		Recipient: "0xabc",
		Network:   "sui:testnet",
		Asset:     "USDC",
		Amount:    "10000",
		Endpoints: []string{endpoint},
	})
	require.Nil(t, prepared)
	require.ErrorContains(t, err, "insufficient")
	require.ErrorContains(t, err, "coin object balance 6000")
	require.Equal(t, []string{"GetBalance", "ListOwnedObjects"}, methods)
}

func TestPreparePaymentRejectsOverflowingCoinObjectSum(t *testing.T) {
	signer := newTestSigner(t)
	var methods []string
	endpoint, closeServer := newSuiTransactionGRPCTestServer(t, &suiTransactionGRPCTestServer{
		methods: &methods,
		getBalance: func(ctx context.Context, req *rpcv2.GetBalanceRequest) (*rpcv2.GetBalanceResponse, error) {
			return suiBalanceGRPCResult(TestnetUSDCType, 1000, math.MaxUint64), nil
		},
		listOwnedObjects: func(ctx context.Context, req *rpcv2.ListOwnedObjectsRequest) (*rpcv2.ListOwnedObjectsResponse, error) {
			return &rpcv2.ListOwnedObjectsResponse{
				Objects: []*rpcv2.Object{
					suiCoinObjectGRPCResult("0x1111", 7, math.MaxUint64, TestnetUSDCType),
					suiCoinObjectGRPCResult("0x2222", 8, math.MaxUint64, TestnetUSDCType),
				},
			}, nil
		},
	})
	defer closeServer()

	prepared, err := PreparePayment(context.Background(), GaslessStablecoinObjectBalancePayment{
		Sender:    signer.Address(),
		Recipient: "0xabc",
		Network:   "sui:testnet",
		Asset:     "USDC",
		Amount:    "10000",
		Endpoints: []string{endpoint},
	})
	require.Nil(t, prepared)
	require.ErrorContains(t, err, "coin object balance sum overflows uint64")
	require.Equal(t, []string{"GetBalance", "ListOwnedObjects"}, methods)
}

func TestPreparePaymentRejectsInvalidInputWithoutRPC(t *testing.T) {
	signer := newTestSigner(t)
	var methods []string
	_, closeServer := newSuiTransactionGRPCTestServer(t, &suiTransactionGRPCTestServer{
		methods: &methods,
		getBalance: func(ctx context.Context, req *rpcv2.GetBalanceRequest) (*rpcv2.GetBalanceResponse, error) {
			t.Fatal("invalid input must fail before any RPC")
			return nil, nil
		},
	})
	defer closeServer()

	cases := []struct {
		name    string
		payment GaslessStablecoinObjectBalancePayment
		wantErr string
	}{
		{
			name:    "empty sender",
			payment: GaslessStablecoinObjectBalancePayment{Recipient: "0xabc", Network: "sui:testnet", Asset: "USDC", Amount: "10000"},
			wantErr: "empty sender",
		},
		{
			name:    "empty recipient",
			payment: GaslessStablecoinObjectBalancePayment{Sender: signer.Address(), Network: "sui:testnet", Asset: "USDC", Amount: "10000"},
			wantErr: "empty recipient",
		},
		{
			name:    "zero amount",
			payment: GaslessStablecoinObjectBalancePayment{Sender: signer.Address(), Recipient: "0xabc", Network: "sui:testnet", Asset: "USDC", Amount: "0"},
			wantErr: "invalid amount",
		},
		{
			name:    "non numeric amount",
			payment: GaslessStablecoinObjectBalancePayment{Sender: signer.Address(), Recipient: "0xabc", Network: "sui:testnet", Asset: "USDC", Amount: "abc"},
			wantErr: "invalid amount",
		},
		{
			name:    "asset not allowlisted",
			payment: GaslessStablecoinObjectBalancePayment{Sender: signer.Address(), Recipient: "0xabc", Network: "sui:testnet", Asset: "0x2::sui::SUI", Amount: "10000"},
			wantErr: "not gasless stablecoin allowlisted",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			prepared, err := PreparePayment(context.Background(), tc.payment)
			require.Nil(t, prepared)
			require.ErrorContains(t, err, tc.wantErr)
		})
	}
	require.Empty(t, methods)
}

func TestExecuteGaslessStablecoinObjectBalancePaymentDefaultsSenderToSigner(t *testing.T) {
	signer := newTestSigner(t)
	var methods []string
	endpoint, closeServer := newSuiTransactionGRPCTestServer(t, &suiTransactionGRPCTestServer{
		methods: &methods,
		getServiceInfo: func(ctx context.Context, req *rpcv2.GetServiceInfoRequest) (*rpcv2.GetServiceInfoResponse, error) {
			return &rpcv2.GetServiceInfoResponse{Epoch: ptr(uint64(42))}, nil
		},
		getBalance: func(ctx context.Context, req *rpcv2.GetBalanceRequest) (*rpcv2.GetBalanceResponse, error) {
			require.Equal(t, signer.Address(), req.GetOwner())
			return suiBalanceGRPCResult(TestnetUSDCType, 10000, 400000), nil
		},
		execute: func(ctx context.Context, req *rpcv2.ExecuteTransactionRequest) (*rpcv2.ExecuteTransactionResponse, error) {
			return &rpcv2.ExecuteTransactionResponse{
				Transaction: suiExecutedTransactionGRPCResult("11111111111111111111111111111111", "0xabc", TestnetUSDCType, "10000"),
			}, nil
		},
	})
	defer closeServer()

	result, err := ExecuteGaslessStablecoinObjectBalancePayment(context.Background(), GaslessStablecoinObjectBalancePayment{
		Recipient: "0xabc",
		Network:   "sui:testnet",
		Asset:     "USDC",
		Amount:    "10000",
		Endpoints: []string{endpoint},
	}, signer)
	require.NoError(t, err)
	require.NotNil(t, result.PaymentTransaction)
	require.Nil(t, result.PrepareTransaction)
	require.Equal(t, []string{"GetBalance", "GetServiceInfo", "ExecuteTransaction"}, methods)
}

func TestExecuteGaslessStablecoinObjectBalancePaymentRejectsSignerMismatch(t *testing.T) {
	signer := newTestSigner(t)
	var methods []string
	endpoint, closeServer := newSuiTransactionGRPCTestServer(t, &suiTransactionGRPCTestServer{
		methods: &methods,
		getBalance: func(ctx context.Context, req *rpcv2.GetBalanceRequest) (*rpcv2.GetBalanceResponse, error) {
			t.Fatal("signer mismatch must fail before any RPC")
			return nil, nil
		},
	})
	defer closeServer()

	result, err := ExecuteGaslessStablecoinObjectBalancePayment(context.Background(), GaslessStablecoinObjectBalancePayment{
		Sender:    "0x123",
		Recipient: "0xabc",
		Network:   "sui:testnet",
		Asset:     "USDC",
		Amount:    "10000",
		Endpoints: []string{endpoint},
	}, signer)
	require.Nil(t, result)
	require.ErrorContains(t, err, "does not match sender")
	require.Empty(t, methods)
}
