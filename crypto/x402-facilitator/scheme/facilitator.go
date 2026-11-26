// Package scheme declares the common x402 facilitator contract that every
// scheme implements. Concrete chain facilitators live beside their scheme
// under scheme/<chain>/facilitator.
package scheme

import (
	"context"

	"github.com/gosuda/x402-facilitator/types"
)

// Facilitator is the common x402 facilitator contract every scheme
// implements.
type Facilitator interface {
	Verify(ctx context.Context, payment *types.PaymentPayload, req *types.PaymentRequirements) (*types.PaymentVerifyResponse, error)
	Settle(ctx context.Context, payment *types.PaymentPayload, req *types.PaymentRequirements) (*types.PaymentSettleResponse, error)
	Supported() *types.SupportedResponse
}
