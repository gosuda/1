package sdk

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSDKTypeImports verifies that SDK types are properly re-exported
func TestSDKTypeImports(t *testing.T) {
	t.Run("PaymentPayload type is accessible", func(t *testing.T) {
		var _ PaymentPayload
	})

	t.Run("PaymentRequirements type is accessible", func(t *testing.T) {
		var _ PaymentRequirements
	})

	t.Run("FacilitatorEvmSigner interface is accessible", func(t *testing.T) {
		var _ FacilitatorEvmSigner
	})

	t.Run("VerifyResponse type is accessible", func(t *testing.T) {
		var _ VerifyResponse
	})

	t.Run("SettleResponse type is accessible", func(t *testing.T) {
		var _ SettleResponse
	})

	t.Run("Network type is accessible", func(t *testing.T) {
		var _ Network
	})
}

// TestSDKFunctionImports verifies that SDK functions are properly re-exported
func TestSDKFunctionImports(t *testing.T) {
	t.Run("NewFacilitator function is accessible", func(t *testing.T) {
		require.NotNil(t, NewFacilitator, "NewFacilitator should not be nil")
	})

	t.Run("NewExactEvmScheme function is accessible", func(t *testing.T) {
		require.NotNil(t, NewExactEvmScheme, "NewExactEvmScheme should not be nil")
	})

	t.Run("VerifyUniversalSignature function is accessible", func(t *testing.T) {
		require.NotNil(t, VerifyUniversalSignature, "VerifyUniversalSignature should not be nil")
	})

	t.Run("HashEIP3009Authorization function is accessible", func(t *testing.T) {
		require.NotNil(t, HashEIP3009Authorization, "HashEIP3009Authorization should not be nil")
	})

	t.Run("IsPermit2Payload function is accessible", func(t *testing.T) {
		require.NotNil(t, IsPermit2Payload, "IsPermit2Payload should not be nil")
	})

	t.Run("IsEIP3009Payload function is accessible", func(t *testing.T) {
		require.NotNil(t, IsEIP3009Payload, "IsEIP3009Payload should not be nil")
	})
}

// TestSDKConstantImports verifies that SDK constants are properly re-exported
func TestSDKConstantImports(t *testing.T) {
	t.Run("PERMIT2Address constant is accessible", func(t *testing.T) {
		assert.NotEmpty(t, PERMIT2Address, "PERMIT2Address should not be empty")
	})

	t.Run("SchemeExact constant is accessible", func(t *testing.T) {
		assert.NotEmpty(t, SchemeExact, "SchemeExact should not be empty")
		assert.Equal(t, "exact", SchemeExact)
	})
}

// TestPayloadDetection verifies payload type detection functions
func TestPayloadDetection(t *testing.T) {
	t.Run("nil payload detection", func(t *testing.T) {
		assert.False(t, IsEIP3009Payload(nil), "nil should not be detected as EIP3009")
		assert.False(t, IsPermit2Payload(nil), "nil should not be detected as Permit2")
	})

	t.Run("empty map payload detection", func(t *testing.T) {
		empty := map[string]interface{}{}
		assert.False(t, IsEIP3009Payload(empty), "empty map should not be detected as EIP3009")
		assert.False(t, IsPermit2Payload(empty), "empty map should not be detected as Permit2")
	})

	t.Run("EIP3009 payload detection", func(t *testing.T) {
		eip3009Payload := map[string]interface{}{
			"authorization": map[string]interface{}{
				"from":        "0x1234567890123456789012345678901234567890",
				"to":          "0x0987654321098765432109876543210987654321",
				"value":       "1000000",
				"validAfter":  "0",
				"validBefore": "9999999999",
				"nonce":       "0x0000000000000000000000000000000000000000000000000000000000000001",
			},
			"signature": "0x1234",
		}

		assert.True(t, IsEIP3009Payload(eip3009Payload), "Should detect EIP3009 payload")
		assert.False(t, IsPermit2Payload(eip3009Payload), "Should not detect as Permit2 payload")
	})

	t.Run("Permit2 payload detection", func(t *testing.T) {
		permit2Payload := map[string]interface{}{
			"permit2Authorization": map[string]interface{}{
				"from": "0x1234567890123456789012345678901234567890",
				"permitted": map[string]interface{}{
					"token":  "0x036CbD53842c5426634e7929541eC2318f3dCF7e",
					"amount": "1000000",
				},
				"spender":  "0x0987654321098765432109876543210987654321",
				"nonce":    "1",
				"deadline": "9999999999",
				"witness": map[string]interface{}{
					"to":         "0x0987654321098765432109876543210987654321",
					"validAfter": "0",
					"extra":      "0x",
				},
			},
			"signature": "0x1234",
		}

		assert.True(t, IsPermit2Payload(permit2Payload), "Should detect Permit2 payload")
		assert.False(t, IsEIP3009Payload(permit2Payload), "Should not detect as EIP3009 payload")
	})
}

// TestFacilitatorCreation verifies that facilitator can be created
func TestFacilitatorCreation(t *testing.T) {
	t.Run("Create facilitator instance", func(t *testing.T) {
		facilitator := NewFacilitator()
		require.NotNil(t, facilitator, "Facilitator should not be nil")
	})
}
