package evm

import (
	"encoding/hex"
	"encoding/json"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/gosuda/x402-facilitator/scheme/evm/internal/sdk"
	"github.com/stretchr/testify/require"
)

func TestAuthorizationWireCompat(t *testing.T) {
	const nonceHex = "0x0102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f20"
	var nonce [32]byte
	nonceBytes, err := hex.DecodeString(nonceHex[2:])
	require.NoError(t, err)
	copy(nonce[:], nonceBytes)

	local := Authorization{
		From:        common.HexToAddress("0x47322Ca28a85B12a7EA64a251Cd8b9Ea1fac037b"),
		To:          common.HexToAddress("0x1234567890123456789012345678901234567890"),
		Value:       big.NewInt(10000),
		ValidAfter:  big.NewInt(0),
		ValidBefore: big.NewInt(9999999999),
		Nonce:       nonce,
	}

	t.Run("marshal matches upstream wire shape", func(t *testing.T) {
		data, err := json.Marshal(local)
		require.NoError(t, err)

		var upstream sdk.ExactEIP3009Authorization
		require.NoError(t, json.Unmarshal(data, &upstream))

		require.Equal(t, local.From, common.HexToAddress(upstream.From))
		require.Equal(t, local.To, common.HexToAddress(upstream.To))
		require.Equal(t, "10000", upstream.Value)
		require.Equal(t, "0", upstream.ValidAfter)
		require.Equal(t, "9999999999", upstream.ValidBefore)
		require.Equal(t, nonceHex, upstream.Nonce)
	})

	t.Run("unmarshal from upstream wire shape", func(t *testing.T) {
		upstream := sdk.ExactEIP3009Authorization{
			From:        local.From.Hex(),
			To:          local.To.Hex(),
			Value:       "10000",
			ValidAfter:  "0",
			ValidBefore: "9999999999",
			Nonce:       nonceHex,
		}
		data, err := json.Marshal(upstream)
		require.NoError(t, err)

		var decoded Authorization
		require.NoError(t, json.Unmarshal(data, &decoded))
		require.Equal(t, local.From, decoded.From)
		require.Equal(t, local.To, decoded.To)
		require.Equal(t, 0, local.Value.Cmp(decoded.Value))
		require.Equal(t, 0, local.ValidAfter.Cmp(decoded.ValidAfter))
		require.Equal(t, 0, local.ValidBefore.Cmp(decoded.ValidBefore))
		require.Equal(t, local.Nonce, decoded.Nonce)
	})

	t.Run("round-trip preserves fields", func(t *testing.T) {
		data, err := json.Marshal(local)
		require.NoError(t, err)
		var decoded Authorization
		require.NoError(t, json.Unmarshal(data, &decoded))
		require.Equal(t, local.From, decoded.From)
		require.Equal(t, local.To, decoded.To)
		require.Equal(t, 0, local.Value.Cmp(decoded.Value))
		require.Equal(t, 0, local.ValidAfter.Cmp(decoded.ValidAfter))
		require.Equal(t, 0, local.ValidBefore.Cmp(decoded.ValidBefore))
		require.Equal(t, local.Nonce, decoded.Nonce)
	})

	t.Run("rejects invalid decimal", func(t *testing.T) {
		var decoded Authorization
		err := json.Unmarshal([]byte(`{"from":"0x0","to":"0x0","value":"not-a-number","validAfter":"0","validBefore":"0","nonce":"`+nonceHex+`"}`), &decoded)
		require.Error(t, err)
	})

	t.Run("rejects wrong nonce length", func(t *testing.T) {
		var decoded Authorization
		err := json.Unmarshal([]byte(`{"from":"0x0","to":"0x0","value":"0","validAfter":"0","validBefore":"0","nonce":"0xabcd"}`), &decoded)
		require.Error(t, err)
	})
}

func TestPermit2AuthorizationWireCompat(t *testing.T) {
	local := Permit2Authorization{
		From: common.HexToAddress("0x47322Ca28a85B12a7EA64a251Cd8b9Ea1fac037b"),
		Permitted: Permit2TokenPermissions{
			Token:  common.HexToAddress("0x036CbD53842c5426634e7929541eC2318f3dCF7e"),
			Amount: big.NewInt(1000000),
		},
		Spender:  X402ExactPermit2ProxyAddress,
		Nonce:    big.NewInt(42),
		Deadline: big.NewInt(9999999999),
		Witness: Permit2Witness{
			To:         common.HexToAddress("0x1234567890123456789012345678901234567890"),
			ValidAfter: big.NewInt(0),
		},
	}

	t.Run("marshal matches upstream wire shape", func(t *testing.T) {
		data, err := json.Marshal(local)
		require.NoError(t, err)

		var upstream sdk.Permit2Authorization
		require.NoError(t, json.Unmarshal(data, &upstream))

		require.Equal(t, local.From, common.HexToAddress(upstream.From))
		require.Equal(t, local.Permitted.Token, common.HexToAddress(upstream.Permitted.Token))
		require.Equal(t, "1000000", upstream.Permitted.Amount)
		require.Equal(t, local.Spender, common.HexToAddress(upstream.Spender))
		require.Equal(t, "42", upstream.Nonce)
		require.Equal(t, "9999999999", upstream.Deadline)
		require.Equal(t, local.Witness.To, common.HexToAddress(upstream.Witness.To))
		require.Equal(t, "0", upstream.Witness.ValidAfter)
	})

	t.Run("unmarshal from upstream wire shape", func(t *testing.T) {
		upstream := sdk.Permit2Authorization{
			From: local.From.Hex(),
			Permitted: sdk.Permit2TokenPermissions{
				Token:  local.Permitted.Token.Hex(),
				Amount: "1000000",
			},
			Spender:  local.Spender.Hex(),
			Nonce:    "42",
			Deadline: "9999999999",
			Witness: sdk.Permit2Witness{
				To:         local.Witness.To.Hex(),
				ValidAfter: "0",
			},
		}
		data, err := json.Marshal(upstream)
		require.NoError(t, err)

		var decoded Permit2Authorization
		require.NoError(t, json.Unmarshal(data, &decoded))
		require.Equal(t, local.From, decoded.From)
		require.Equal(t, local.Permitted.Token, decoded.Permitted.Token)
		require.Equal(t, 0, local.Permitted.Amount.Cmp(decoded.Permitted.Amount))
		require.Equal(t, local.Spender, decoded.Spender)
		require.Equal(t, 0, local.Nonce.Cmp(decoded.Nonce))
		require.Equal(t, 0, local.Deadline.Cmp(decoded.Deadline))
		require.Equal(t, local.Witness.To, decoded.Witness.To)
		require.Equal(t, 0, local.Witness.ValidAfter.Cmp(decoded.Witness.ValidAfter))
	})

	t.Run("round-trip preserves fields", func(t *testing.T) {
		data, err := json.Marshal(local)
		require.NoError(t, err)
		var decoded Permit2Authorization
		require.NoError(t, json.Unmarshal(data, &decoded))
		require.Equal(t, local.From, decoded.From)
		require.Equal(t, local.Permitted.Token, decoded.Permitted.Token)
		require.Equal(t, 0, local.Permitted.Amount.Cmp(decoded.Permitted.Amount))
		require.Equal(t, local.Spender, decoded.Spender)
		require.Equal(t, 0, local.Nonce.Cmp(decoded.Nonce))
		require.Equal(t, 0, local.Deadline.Cmp(decoded.Deadline))
		require.Equal(t, local.Witness.To, decoded.Witness.To)
		require.Equal(t, 0, local.Witness.ValidAfter.Cmp(decoded.Witness.ValidAfter))
	})
}

func TestEVMPayloadUnmarshalUpstreamBytes(t *testing.T) {
	upstream := sdk.ExactEIP3009Payload{
		Signature: "0x" + string(make([]byte, 130)), // 65-byte placeholder signature in hex
		Authorization: sdk.ExactEIP3009Authorization{
			From:        "0x47322Ca28a85B12a7EA64a251Cd8b9Ea1fac037b",
			To:          "0x1234567890123456789012345678901234567890",
			Value:       "10000",
			ValidAfter:  "0",
			ValidBefore: "9999999999",
			Nonce:       "0x0102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f20",
		},
	}
	data, err := json.Marshal(upstream)
	require.NoError(t, err)

	var local EVMPayload
	require.NoError(t, json.Unmarshal(data, &local))
	require.NotNil(t, local.Authorization)
	require.Equal(t, "0x47322Ca28a85B12a7EA64a251Cd8b9Ea1fac037b", local.Authorization.From.Hex())
	require.Equal(t, int64(10000), local.Authorization.Value.Int64())
	require.Equal(t, int64(0), local.Authorization.ValidAfter.Int64())
	require.Equal(t, int64(9999999999), local.Authorization.ValidBefore.Int64())
	require.Equal(t, byte(0x01), local.Authorization.Nonce[0])
	require.Equal(t, byte(0x20), local.Authorization.Nonce[31])
}

func TestPermit2PayloadUnmarshalUpstreamBytes(t *testing.T) {
	upstream := sdk.ExactPermit2Payload{
		Signature: "0x" + string(make([]byte, 130)),
		Permit2Authorization: sdk.Permit2Authorization{
			From: "0x47322Ca28a85B12a7EA64a251Cd8b9Ea1fac037b",
			Permitted: sdk.Permit2TokenPermissions{
				Token:  "0x036CbD53842c5426634e7929541eC2318f3dCF7e",
				Amount: "1000000",
			},
			Spender:  X402ExactPermit2ProxyAddress.Hex(),
			Nonce:    "42",
			Deadline: "9999999999",
			Witness: sdk.Permit2Witness{
				To:         "0x1234567890123456789012345678901234567890",
				ValidAfter: "0",
			},
		},
	}
	data, err := json.Marshal(upstream)
	require.NoError(t, err)

	var local Permit2Payload
	require.NoError(t, json.Unmarshal(data, &local))
	require.NotNil(t, local.Permit2Authorization)
	auth := local.Permit2Authorization
	require.Equal(t, "0x47322Ca28a85B12a7EA64a251Cd8b9Ea1fac037b", auth.From.Hex())
	require.Equal(t, "0x036CbD53842c5426634e7929541eC2318f3dCF7e", auth.Permitted.Token.Hex())
	require.Equal(t, int64(1000000), auth.Permitted.Amount.Int64())
	require.Equal(t, X402ExactPermit2ProxyAddress, auth.Spender)
	require.Equal(t, int64(42), auth.Nonce.Int64())
	require.Equal(t, int64(9999999999), auth.Deadline.Int64())
	require.Equal(t, int64(0), auth.Witness.ValidAfter.Int64())
}
