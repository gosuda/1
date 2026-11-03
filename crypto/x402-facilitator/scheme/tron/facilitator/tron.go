package facilitator

import (
	"context"
	"errors"
	"fmt"

	"github.com/gosuda/x402-facilitator/types"
)

// ErrTronNotImplemented reports that the Tron facilitator is a placeholder:
// the scheme/tron types exist, but verify and settle are not implemented yet.
var ErrTronNotImplemented = errors.New("tron facilitator not implemented")

type TronFacilitator struct {
}

// NewTronFacilitator fails fast: constructing a facilitator that accepts
// payments it can neither verify nor settle would turn every payment into
// a silent 200, so callers get an error at boot instead.
func NewTronFacilitator(network string, url string, privateKeyHex string) (*TronFacilitator, error) {
	return nil, fmt.Errorf("%w (network=%q)", ErrTronNotImplemented, network)
}

func (t *TronFacilitator) Verify(ctx context.Context, payload *types.PaymentPayload, req *types.PaymentRequirements) (*types.PaymentVerifyResponse, error) {
	return nil, ErrTronNotImplemented
}

func (t *TronFacilitator) Settle(ctx context.Context, payload *types.PaymentPayload, req *types.PaymentRequirements) (*types.PaymentSettleResponse, error) {
	return nil, ErrTronNotImplemented
}

// Supported returns nil: the Tron facilitator is not implemented, so it
// is gated from discovery until a follow-up lands.
func (t *TronFacilitator) Supported() *types.SupportedResponse {
	return nil
}
