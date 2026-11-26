package evm

import (
	"encoding/hex"
	"math/big"
	"testing"
	"time"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/ethereum/go-ethereum/common"
	"github.com/stretchr/testify/require"
)

func TestMessageSignVerify(t *testing.T) {
	// Generate a random private key
	privKey, err := secp256k1.GeneratePrivateKey()
	require.NoError(t, err)

	// Create a signer using the private key
	signer := NewRawPrivateSigner(privKey.Serialize())

	// Create a message to sign
	message := Keccak256([]byte("Hello, World!"))

	// Sign the message
	signature, err := signer(message)
	require.NoError(t, err)

	pubkey, err := Ecrecover(message, signature)
	require.NoError(t, err)

	// Verify the signature
	valid := VerifySignature(pubkey, message, signature[:64])
	require.True(t, valid, "signature verification failed")
}

func TestPayloadSignVerify(t *testing.T) {
	// Generate a random private key
	privKey, err := secp256k1.GeneratePrivateKey()
	require.NoError(t, err)

	// Create a signer using the private key
	signer := NewRawPrivateSigner(privKey.Serialize())

	// Create a message to sign
	chain := "eip155:84532"
	token := "USDC"

	payload, err := NewEVMPayload(chain, token,
		"0x1234567890abcdef1234567890abcdef12345678", "0xabcdefabcdefabcdefabcdefabcdefabcdefabcdef", "100", signer)
	require.NoError(t, err)
	message := HashEip3009(payload.Authorization, GetDomainConfig(chain, token))
	signature, err := hex.DecodeString(payload.Signature)
	require.NoError(t, err)
	pubkey, err := Ecrecover(message, signature)
	require.NoError(t, err)

	// Verify the signature
	valid := VerifySignature(pubkey, message, signature[:64])
	require.True(t, valid, "signature verification failed")
}

func TestPermit2SignVerify(t *testing.T) {
	privKey, err := secp256k1.GeneratePrivateKey()
	require.NoError(t, err)

	signer := NewRawPrivateSigner(privKey.Serialize())

	auth := &Permit2Authorization{
		From:     common.HexToAddress("0x1234567890abcdef1234567890abcdef12345678"),
		Spender:  X402ExactPermit2ProxyAddress,
		Nonce:    big.NewInt(1),
		Deadline: big.NewInt(time.Now().Unix() + 3600),
		Permitted: Permit2TokenPermissions{
			Token:  common.HexToAddress("0x036CbD53842c5426634e7929541eC2318f3dCF7e"),
			Amount: big.NewInt(1000000),
		},
		Witness: Permit2Witness{
			To:         common.HexToAddress("0xabcdefabcdefabcdefabcdefabcdefabcdefabcd"),
			ValidAfter: big.NewInt(0),
		},
	}

	chainID := big.NewInt(84532) // base-sepolia

	sigHex, err := SignPermit2(auth, chainID, signer)
	require.NoError(t, err)

	digest := HashPermit2(auth, chainID)
	sig, err := hex.DecodeString(sigHex)
	require.NoError(t, err)

	pubkey, err := Ecrecover(digest, sig)
	require.NoError(t, err)

	valid := VerifySignature(pubkey, digest, sig[:64])
	require.True(t, valid, "permit2 signature verification failed")
}

func TestPermit2PayloadSignVerify(t *testing.T) {
	privKey, err := secp256k1.GeneratePrivateKey()
	require.NoError(t, err)

	signer := NewRawPrivateSigner(privKey.Serialize())

	chain := "eip155:84532"
	token := "USDC"

	payload, err := NewPermit2Payload(chain, token,
		"0x1234567890abcdef1234567890abcdef12345678", "0xabcdefabcdefabcdefabcdefabcdefabcdefabcd", "1000000", signer)
	require.NoError(t, err)
	require.NotNil(t, payload.Permit2Authorization)

	auth := payload.Permit2Authorization
	chainID := GetChainID(chain)
	require.NotNil(t, chainID)

	digest := HashPermit2(auth, chainID)
	sig, err := hex.DecodeString(payload.Signature)
	require.NoError(t, err)

	pubkey, err := Ecrecover(digest, sig)
	require.NoError(t, err)

	valid := VerifySignature(pubkey, digest, sig[:64])
	require.True(t, valid, "permit2 payload signature verification failed")
}
