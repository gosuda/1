package facilitator

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNewTronFacilitatorFailsFast(t *testing.T) {
	f, err := NewTronFacilitator("tron:mainnet", "https://grpc.trongrid.io", "00")
	require.Nil(t, f)
	require.True(t, errors.Is(err, ErrTronNotImplemented), "constructor must fail fast, got %v", err)

}

func TestTronFacilitatorVerifySettleNotImplemented(t *testing.T) {
	f := &TronFacilitator{}

	_, err := f.Verify(t.Context(), nil, nil)
	require.True(t, errors.Is(err, ErrTronNotImplemented), "verify must fail loudly, got %v", err)

	_, err = f.Settle(t.Context(), nil, nil)
	require.True(t, errors.Is(err, ErrTronNotImplemented), "settle must fail loudly, got %v", err)

	require.Nil(t, f.Supported())
}
