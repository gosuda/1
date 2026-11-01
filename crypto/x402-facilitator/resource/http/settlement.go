package x402http

import (
	"context"

	"github.com/gosuda/x402-facilitator/types"
)

// Settlement describes the payment settled for a request. The response
// headers stay the primary wire contract; this is the in-process convenience
// view recorded on the forwarded request's context.
type Settlement struct {
	Transaction string
	Network     types.Network
	Payer       string
}

type settlementKey struct{}

// SettlementFrom returns the settlement the gate recorded on ctx, and false
// when the request was not settled through the gate.
func SettlementFrom(ctx context.Context) (Settlement, bool) {
	settlement, ok := ctx.Value(settlementKey{}).(Settlement)
	return settlement, ok
}
