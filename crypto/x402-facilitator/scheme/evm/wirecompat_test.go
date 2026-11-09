package evm

import (
	"encoding/json"
	"testing"

	"github.com/gosuda/x402-facilitator/scheme/evm/internal/sdk"
	"github.com/gosuda/x402-facilitator/types"
	"github.com/stretchr/testify/require"
)

// TestV2WireCompatibility round-trips every local wire struct through the
// corresponding SDK re-export and asserts JSON equality, so any upstream
// field rename surfaces as a local test failure rather than silently
// breaking /verify, /settle, and /supported.
//
// This guard lives in the scheme/evm module because it is the only module
// in the repository whose graph still contains the x402 SDK: the root
// module stays chain-blind and SDK-free.
func TestV2WireCompatibility(t *testing.T) {
	t.Run("PaymentRequirements", func(t *testing.T) {
		local := types.PaymentRequirements{
			Scheme:            "exact",
			Network:           "eip155:84532",
			Asset:             "0x036CbD53842c5426634e7929541eC2318f3dCF7e",
			Amount:            "1000",
			PayTo:             "0x1234567890123456789012345678901234567890",
			MaxTimeoutSeconds: 300,
			Extra: map[string]interface{}{
				"name":    "USDC",
				"version": "2",
			},
		}
		assertWireCompat[sdk.PaymentRequirements](t, local)
	})

	t.Run("PaymentPayload", func(t *testing.T) {
		local := types.PaymentPayload{
			X402Version: int(types.X402VersionV2),
			Payload: map[string]interface{}{
				"signature": "0xabc",
				"authorization": map[string]interface{}{
					"from":        "0x1234567890123456789012345678901234567890",
					"to":          "0x2345678901234567890123456789012345678901",
					"value":       "1000",
					"validAfter":  "0",
					"validBefore": "9999999999",
					"nonce":       "0x0000000000000000000000000000000000000000000000000000000000000001",
				},
			},
			Accepted: types.PaymentRequirements{
				Scheme:            "exact",
				Network:           "eip155:84532",
				Asset:             "0x036CbD53842c5426634e7929541eC2318f3dCF7e",
				Amount:            "1000",
				PayTo:             "0x2345678901234567890123456789012345678901",
				MaxTimeoutSeconds: 300,
			},
			Resource: &types.ResourceInfo{
				URL:         "https://example.com/api/data",
				Description: "example resource",
				MimeType:    "application/json",
			},
			Extensions: map[string]interface{}{
				"eip2612GasSponsoring": map[string]interface{}{
					"sponsor": "0x3456789012345678901234567890123456789012",
				},
			},
		}
		assertWireCompat[sdk.PaymentPayload](t, local)
	})

	t.Run("SupportedKind", func(t *testing.T) {
		local := types.SupportedKind{
			X402Version: int(types.X402VersionV2),
			Scheme:      "exact",
			Network:     "eip155:84532",
			Extra: map[string]interface{}{
				"feePayer": "0x1234567890123456789012345678901234567890",
			},
		}
		assertWireCompat[sdk.SupportedKind](t, local)
	})

	t.Run("SupportedResponse", func(t *testing.T) {
		local := types.SupportedResponse{
			Kinds: []types.SupportedKind{{
				X402Version: int(types.X402VersionV2),
				Scheme:      "exact",
				Network:     "eip155:84532",
			}},
			Extensions: []string{"eip2612GasSponsoring"},
			Signers: map[string][]string{
				"eip155:*": {"0x1234567890123456789012345678901234567890"},
			},
		}
		assertWireCompat[sdk.SupportedResponse](t, local)
	})

	t.Run("PaymentVerifyResponse", func(t *testing.T) {
		local := types.PaymentVerifyResponse{
			IsValid:        false,
			InvalidReason:  "insufficient_balance",
			InvalidMessage: "payer balance is below the requested amount",
			Payer:          "0x1234567890123456789012345678901234567890",
		}
		assertWireCompat[sdk.VerifyResponse](t, local)
	})

	t.Run("PaymentSettleResponse", func(t *testing.T) {
		local := types.PaymentSettleResponse{
			Success:     true,
			Payer:       "0x1234567890123456789012345678901234567890",
			Transaction: "0xabcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789",
			Network:     types.Network("eip155:84532"),
		}
		assertWireCompat[sdk.SettleResponse](t, local)
	})
}

// assertWireCompat round-trips local through the SDK type and asserts JSON
// equality, catching field renames and tag changes across the boundary.
func assertWireCompat[SDK any](t *testing.T, local any) {
	t.Helper()

	localJSON, err := json.Marshal(local)
	require.NoError(t, err, "marshal local value")

	var remote SDK
	require.NoError(t, json.Unmarshal(localJSON, &remote), "unmarshal local JSON into SDK type")

	remoteJSON, err := json.Marshal(remote)
	require.NoError(t, err, "marshal SDK value")

	require.JSONEq(t, string(localJSON), string(remoteJSON),
		"wire drift between local %T and its x402 SDK counterpart", local)
}
