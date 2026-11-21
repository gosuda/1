package facilitator

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"strconv"
	"testing"
	"time"

	solclient "github.com/blocto/solana-go-sdk/client"
	solcommon "github.com/blocto/solana-go-sdk/common"
	soltypes "github.com/blocto/solana-go-sdk/types"
	"github.com/mr-tron/base58"
	"github.com/stretchr/testify/require"

	"github.com/gosuda/x402-facilitator/types"
)

// stubRPC stands in for the Solana RPC boundary: it records what the
// facilitator submits and simulates confirmation outcomes.
type stubRPC struct {
	signature string
	sendErr   error
	getErr    error
	pending   int  // not-found responses before confirming
	failed    bool // confirm with an on-chain error
	calls     int
	got       *soltypes.Transaction
	gotHash   string
}

func (s *stubRPC) SendTransaction(_ context.Context, tx soltypes.Transaction) (string, error) {
	s.got = &tx
	return s.signature, s.sendErr
}

func (s *stubRPC) GetTransaction(_ context.Context, txhash string) (*solclient.Transaction, error) {
	s.calls++
	s.gotHash = txhash
	if s.getErr != nil {
		return nil, s.getErr
	}
	if s.pending > 0 {
		s.pending--
		return nil, nil
	}
	if s.failed {
		return &solclient.Transaction{
			Slot: 1,
			Meta: &solclient.TransactionMeta{Err: map[string]any{"InstructionError": []any{float64(0), "custom program error"}}},
		}, nil
	}
	return &solclient.Transaction{Slot: 1, Meta: &solclient.TransactionMeta{}}, nil
}

func sigIsNonZero(signature []byte) bool {
	for _, b := range signature {
		if b != 0 {
			return true
		}
	}
	return false
}

func solanaTestAccount(t *testing.T) soltypes.Account {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	account, err := soltypes.AccountFromBytes(priv) // 64 bytes: seed + public key
	require.NoError(t, err)
	return account
}

func solanaTestPubkey() solcommon.PublicKey {
	raw := make([]byte, 32)
	_, _ = rand.Read(raw)
	return solcommon.PublicKeyFromBytes(raw)
}

// solanaTestTx builds a legacy transaction whose single instruction is an SPL
// Token TransferChecked paying amount base units of mint from a source token
// account to dest, signed by the payer and leaving the fee payer slot empty.
func solanaTestTx(t *testing.T, feePayer soltypes.Account, payer soltypes.Account, mint solcommon.PublicKey, dest solcommon.PublicKey, amount uint64, decimals uint8) soltypes.Transaction {
	t.Helper()
	accounts := []solcommon.PublicKey{
		feePayer.PublicKey, // 0: fee payer
		payer.PublicKey,    // 1: transfer authority (required signer)
		solanaTestPubkey(), // 2: source token account
		mint,               // 3: mint
		dest,               // 4: destination token account
		solcommon.TokenProgramID,
	}
	data := make([]byte, 0, 10)
	data = append(data, solanaTransferCheckedInstruction)
	data = binary.LittleEndian.AppendUint64(data, amount)
	data = append(data, decimals)

	message := soltypes.Message{
		Header: soltypes.MessageHeader{
			NumRequireSignatures:        2,
			NumReadonlySignedAccounts:   0,
			NumReadonlyUnsignedAccounts: 1,
		},
		Accounts:        accounts,
		RecentBlockHash: base58.Encode(make([]byte, 32)),
		Instructions: []soltypes.CompiledInstruction{{
			ProgramIDIndex: 5,
			Accounts:       []int{2, 3, 4, 1},
			Data:           data,
		}},
		Version: soltypes.MessageVersionLegacy,
	}
	signatures := make([]soltypes.Signature, 2)
	signatures[0] = make(soltypes.Signature, 64) // empty fee payer signature slot
	signatures[1] = soltypes.Signature(payer.Sign(mustSerializeMessage(t, message)))
	return soltypes.Transaction{
		Signatures: signatures,
		Message:    message,
	}
}

func mustSerializeMessage(t *testing.T, message soltypes.Message) []byte {
	t.Helper()
	raw, err := message.Serialize()
	require.NoError(t, err)
	return raw
}

func solanaTestFacilitator(t *testing.T) *SolanaFacilitator {
	t.Helper()
	feePayer := solanaTestAccount(t)
	feePayerKey := hex.EncodeToString(feePayer.PrivateKey)
	f, err := NewSolanaFacilitator("solana:devnet", "https://api.devnet.solana.com", feePayerKey)
	require.NoError(t, err)
	return f
}

func solanaTestRequirements(mint solcommon.PublicKey, payTo solcommon.PublicKey, amount uint64) *types.PaymentRequirements {
	return &types.PaymentRequirements{
		Scheme:            string(types.Exact),
		Network:           "solana:devnet",
		Asset:             mint.String(),
		Amount:            strconv.FormatUint(amount, 10),
		PayTo:             payTo.String(),
		MaxTimeoutSeconds: 60,
	}
}

func solanaTestPayload(t *testing.T, tx soltypes.Transaction, req *types.PaymentRequirements) *types.PaymentPayload {
	t.Helper()
	return &types.PaymentPayload{
		X402Version: int(types.X402VersionV2),
		Payload: map[string]interface{}{
			"transaction": base64.StdEncoding.EncodeToString(mustSerializeTransaction(t, tx)),
		},
		Accepted: *req,
	}
}

func mustSerializeTransaction(t *testing.T, tx soltypes.Transaction) []byte {
	t.Helper()
	raw, err := tx.Serialize()
	require.NoError(t, err)
	return raw
}

func TestNewSolanaFacilitatorValidatesInputs(t *testing.T) {
	feePayer := solanaTestAccount(t)
	key := hex.EncodeToString(feePayer.PrivateKey)

	_, err := NewSolanaFacilitator("eip155:84532", "https://api.devnet.solana.com", key)
	require.ErrorContains(t, err, "invalid Solana network")

	_, err = NewSolanaFacilitator("solana:", "https://api.devnet.solana.com", key)
	require.ErrorContains(t, err, "invalid Solana network")

	_, err = NewSolanaFacilitator("solana:devnet", "   ", key)
	require.ErrorContains(t, err, "rpc URL is required")

	_, err = NewSolanaFacilitator("solana:devnet", "https://api.devnet.solana.com", "not-hex")
	require.ErrorContains(t, err, "invalid hex private key")

	_, err = NewSolanaFacilitator("solana:devnet", "https://api.devnet.solana.com", "aabb")
	require.ErrorContains(t, err, "invalid private key format")
}

func TestNewSolanaFacilitatorAcceptsValidConfig(t *testing.T) {
	f := solanaTestFacilitator(t)
	require.NotEqual(t, solcommon.PublicKey{}, f.feePayer.PublicKey)
	require.Equal(t, "solana:devnet", f.network)
}

func TestSupportedAdvertisesFeePayer(t *testing.T) {
	f := solanaTestFacilitator(t)
	supported := f.Supported()
	require.NotNil(t, supported)
	require.Len(t, supported.Kinds, 1)

	kind := supported.Kinds[0]
	require.Equal(t, int(types.X402VersionV2), kind.X402Version)
	require.Equal(t, string(types.Exact), kind.Scheme)
	require.Equal(t, "solana:devnet", kind.Network)
	require.Equal(t, f.feePayer.PublicKey.String(), kind.Extra["feePayer"])
	require.Equal(t, "base64", kind.Extra["transactionEncoding"])
	require.Equal(t, []string{f.feePayer.PublicKey.String()}, supported.Signers["solana:*"])
}

func TestSolanaVerifyRejectsInvalidEnvelopes(t *testing.T) {
	f := solanaTestFacilitator(t)
	payer := solanaTestAccount(t)
	mint := solanaTestPubkey()
	payTo := solanaTestPubkey()
	dest, _, err := solcommon.FindAssociatedTokenAddress(payTo, mint)
	require.NoError(t, err)

	req := solanaTestRequirements(mint, payTo, 1_000)
	tx := solanaTestTx(t, f.feePayer, payer, mint, dest, 1_000, 6)
	payload := solanaTestPayload(t, tx, req)

	tests := []struct {
		name   string
		mutate func(p *types.PaymentPayload, r *types.PaymentRequirements)
		reason string
	}{
		{"wrong version", func(p *types.PaymentPayload, r *types.PaymentRequirements) { p.X402Version = 1 }, types.ErrInvalidPayloadFormat.Error()},
		{"wrong scheme", func(p *types.PaymentPayload, r *types.PaymentRequirements) { r.Scheme = "other" }, types.ErrIncompatibleScheme.Error()},
		{"wrong network", func(p *types.PaymentPayload, r *types.PaymentRequirements) { r.Network = "solana:mainnet" }, types.ErrNetworkMismatch.Error()},
		{"accepted asset drift", func(p *types.PaymentPayload, r *types.PaymentRequirements) {
			p.Accepted.Asset = solanaTestPubkey().String()
		}, types.ErrTokenMismatch.Error()},
		{"accepted amount drift", func(p *types.PaymentPayload, r *types.PaymentRequirements) { p.Accepted.Amount = "1" }, types.ErrAmountMismatch.Error()},
		{"accepted payTo drift", func(p *types.PaymentPayload, r *types.PaymentRequirements) {
			p.Accepted.PayTo = solanaTestPubkey().String()
		}, types.ErrRecipientMismatch.Error()},
		{"empty payTo", func(p *types.PaymentPayload, r *types.PaymentRequirements) { r.PayTo = " " }, types.ErrRecipientMismatch.Error()},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := *payload
			r := *req
			tt.mutate(&p, &r)

			res, err := f.Verify(t.Context(), &p, &r)
			require.NoError(t, err)
			require.False(t, res.IsValid)
			require.Equal(t, tt.reason, res.InvalidReason)
		})
	}
}

func TestSolanaVerifyRejectsInvalidTransactions(t *testing.T) {
	f := solanaTestFacilitator(t)
	payer := solanaTestAccount(t)
	mint := solanaTestPubkey()
	payTo := solanaTestPubkey()
	dest, _, err := solcommon.FindAssociatedTokenAddress(payTo, mint)
	require.NoError(t, err)
	req := solanaTestRequirements(mint, payTo, 1_000)

	buildPayload := func(rawTx string) *types.PaymentPayload {
		return &types.PaymentPayload{
			X402Version: int(types.X402VersionV2),
			Payload:     map[string]interface{}{"transaction": rawTx},
			Accepted:    *req,
		}
	}

	tests := []struct {
		name    string
		rawTx   func(t *testing.T) string
		message string
		reason  string
	}{
		{"missing transaction", func(t *testing.T) string { return "" }, "required", types.ErrInvalidPayloadFormat.Error()},
		{"not base64", func(t *testing.T) string { return "!!!" }, "base64", types.ErrInvalidPayloadFormat.Error()},
		{"not a transaction", func(t *testing.T) string { return base64.StdEncoding.EncodeToString([]byte("garbage")) }, "not a valid Solana transaction", types.ErrInvalidTransaction.Error()},
		{"wrong fee payer", func(t *testing.T) string {
			tx := solanaTestTx(t, solanaTestAccount(t), payer, mint, dest, 1_000, 6)
			return base64.StdEncoding.EncodeToString(mustSerializeTransaction(t, tx))
		}, "fee payer is not the facilitator fee payer", types.ErrInvalidTransaction.Error()},
		{"no transfer instruction", func(t *testing.T) string {
			tx := solanaTestTx(t, f.feePayer, payer, mint, dest, 1_000, 6)
			tx.Message.Instructions = nil
			return base64.StdEncoding.EncodeToString(mustSerializeTransaction(t, tx))
		}, "exactly one instruction", types.ErrInvalidTransaction.Error()},
		{"extra instruction", func(t *testing.T) string {
			tx := solanaTestTx(t, f.feePayer, payer, mint, dest, 1_000, 6)
			tx.Message.Instructions = append(tx.Message.Instructions, soltypes.CompiledInstruction{
				ProgramIDIndex: 5,
				Accounts:       []int{2, 3},
				Data:           []byte{0x3},
			})
			return base64.StdEncoding.EncodeToString(mustSerializeTransaction(t, tx))
		}, "exactly one instruction, got 2", types.ErrInvalidTransaction.Error()},
		{"fee payer as instruction source", func(t *testing.T) string {
			tx := solanaTestTx(t, f.feePayer, payer, mint, dest, 1_000, 6)
			tx.Message.Accounts[2] = f.feePayer.PublicKey
			return base64.StdEncoding.EncodeToString(mustSerializeTransaction(t, tx))
		}, "uses the facilitator fee payer as an instruction account", types.ErrInvalidTransaction.Error()},
		{"too many required signers", func(t *testing.T) string {
			tx := solanaTestTx(t, f.feePayer, payer, mint, dest, 1_000, 6)
			tx.Message.Header.NumRequireSignatures = 3
			tx.Signatures = append(tx.Signatures, make(soltypes.Signature, 64))
			return base64.StdEncoding.EncodeToString(mustSerializeTransaction(t, tx))
		}, "exactly the fee payer and the payer", types.ErrInvalidTransaction.Error()},
		{"spl multisig", func(t *testing.T) string {
			tx := solanaTestTx(t, f.feePayer, payer, mint, dest, 1_000, 6)
			tx.Message.Accounts = append(tx.Message.Accounts, solanaTestPubkey())
			tx.Message.Instructions[0].Accounts = append(tx.Message.Instructions[0].Accounts, len(tx.Message.Accounts)-1)
			return base64.StdEncoding.EncodeToString(mustSerializeTransaction(t, tx))
		}, "SPL multisig", types.ErrInvalidTransaction.Error()},
		{"random non-zero payer signature", func(t *testing.T) string {
			tx := solanaTestTx(t, f.feePayer, payer, mint, dest, 1_000, 6)
			rand.Read(tx.Signatures[1])
			return base64.StdEncoding.EncodeToString(mustSerializeTransaction(t, tx))
		}, "did not sign", types.ErrInvalidSignature.Error()},
		{"wrong mint", func(t *testing.T) string {
			tx := solanaTestTx(t, f.feePayer, payer, solanaTestPubkey(), dest, 1_000, 6)
			return base64.StdEncoding.EncodeToString(mustSerializeTransaction(t, tx))
		}, "mint does not match", types.ErrTokenMismatch.Error()},
		{"wrong amount", func(t *testing.T) string {
			tx := solanaTestTx(t, f.feePayer, payer, mint, dest, 999, 6)
			return base64.StdEncoding.EncodeToString(mustSerializeTransaction(t, tx))
		}, "does not match required amount", types.ErrAmountMismatch.Error()},
		{"destination not payTo ATA", func(t *testing.T) string {
			tx := solanaTestTx(t, f.feePayer, payer, mint, solanaTestPubkey(), 1_000, 6)
			return base64.StdEncoding.EncodeToString(mustSerializeTransaction(t, tx))
		}, "not the associated token account", types.ErrRecipientMismatch.Error()},
		{"authority did not sign", func(t *testing.T) string {
			tx := solanaTestTx(t, f.feePayer, payer, mint, dest, 1_000, 6)
			tx.Signatures[1] = make(soltypes.Signature, 64) // present but empty
			return base64.StdEncoding.EncodeToString(mustSerializeTransaction(t, tx))
		}, "did not sign", types.ErrInvalidSignature.Error()},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res, err := f.Verify(t.Context(), buildPayload(tt.rawTx(t)), req)
			require.NoError(t, err)
			require.False(t, res.IsValid)
			require.Equal(t, tt.reason, res.InvalidReason)
			require.Contains(t, res.InvalidMessage, tt.message)
		})
	}
}

func TestSolanaVerifyAcceptsValidPayment(t *testing.T) {
	f := solanaTestFacilitator(t)
	payer := solanaTestAccount(t)
	mint := solanaTestPubkey()
	payTo := solanaTestPubkey()
	dest, _, err := solcommon.FindAssociatedTokenAddress(payTo, mint)
	require.NoError(t, err)

	req := solanaTestRequirements(mint, payTo, 1_000)
	payload := solanaTestPayload(t, solanaTestTx(t, f.feePayer, payer, mint, dest, 1_000, 6), req)

	res, err := f.Verify(t.Context(), payload, req)
	require.NoError(t, err)
	require.True(t, res.IsValid)
	require.Equal(t, payer.PublicKey.String(), res.Payer)
}

func TestSolanaSettleCoSignsAndSubmits(t *testing.T) {
	f := solanaTestFacilitator(t)
	payer := solanaTestAccount(t)
	mint := solanaTestPubkey()
	payTo := solanaTestPubkey()
	dest, _, err := solcommon.FindAssociatedTokenAddress(payTo, mint)
	require.NoError(t, err)

	rpc := &stubRPC{signature: "5txSignature"}
	f.client = rpc
	f.confirmer = rpc

	req := solanaTestRequirements(mint, payTo, 1_000)
	payload := solanaTestPayload(t, solanaTestTx(t, f.feePayer, payer, mint, dest, 1_000, 6), req)

	res, err := f.Settle(t.Context(), payload, req)
	require.NoError(t, err)
	require.True(t, res.Success)
	require.Equal(t, "5txSignature", res.Transaction)
	require.Equal(t, payer.PublicKey.String(), res.Payer)
	require.Equal(t, types.Network("solana:devnet"), res.Network)

	// The facilitator must have co-signed in the fee payer slot and polled
	// the confirmation endpoint for the submitted signature.
	require.NotNil(t, rpc.got)
	require.Len(t, rpc.got.Signatures, 2)
	require.True(t, sigIsNonZero(rpc.got.Signatures[0]), "fee payer slot must be signed")
	require.True(t, sigIsNonZero(rpc.got.Signatures[1]), "payer signature must be preserved")
	require.Equal(t, "5txSignature", rpc.gotHash)
	require.Equal(t, 1, rpc.calls)
}

func TestSolanaSettleWaitsForConfirmation(t *testing.T) {
	f := solanaTestFacilitator(t)
	f.confirmTick = time.Millisecond
	payer := solanaTestAccount(t)
	mint := solanaTestPubkey()
	payTo := solanaTestPubkey()
	dest, _, err := solcommon.FindAssociatedTokenAddress(payTo, mint)
	require.NoError(t, err)

	rpc := &stubRPC{signature: "5txPending", pending: 2}
	f.client = rpc
	f.confirmer = rpc

	req := solanaTestRequirements(mint, payTo, 1_000)
	payload := solanaTestPayload(t, solanaTestTx(t, f.feePayer, payer, mint, dest, 1_000, 6), req)

	res, err := f.Settle(t.Context(), payload, req)
	require.NoError(t, err)
	require.True(t, res.Success, "two not-found polls must be tolerated before confirmation")
	require.Equal(t, 3, rpc.calls)
}

func TestSolanaSettleReportsOnChainFailure(t *testing.T) {
	f := solanaTestFacilitator(t)
	payer := solanaTestAccount(t)
	mint := solanaTestPubkey()
	payTo := solanaTestPubkey()
	dest, _, err := solcommon.FindAssociatedTokenAddress(payTo, mint)
	require.NoError(t, err)

	rpc := &stubRPC{signature: "5txFailed", failed: true}
	f.client = rpc
	f.confirmer = rpc

	req := solanaTestRequirements(mint, payTo, 1_000)
	payload := solanaTestPayload(t, solanaTestTx(t, f.feePayer, payer, mint, dest, 1_000, 6), req)

	res, err := f.Settle(t.Context(), payload, req)
	require.NoError(t, err)
	require.False(t, res.Success, "an accepted-but-failed transaction must not report success")
	require.Equal(t, types.ErrTransactionFailed.Error(), res.ErrorReason)
	require.Contains(t, res.ErrorMessage, "failed on chain")
	require.Equal(t, "5txFailed", res.Transaction)
}

func TestSolanaSettleTimesOutWhenNeverConfirmed(t *testing.T) {
	f := solanaTestFacilitator(t)
	f.confirmTimeout = time.Millisecond
	f.confirmTick = time.Millisecond
	payer := solanaTestAccount(t)
	mint := solanaTestPubkey()
	payTo := solanaTestPubkey()
	dest, _, err := solcommon.FindAssociatedTokenAddress(payTo, mint)
	require.NoError(t, err)

	rpc := &stubRPC{signature: "5txStuck", pending: 999}
	f.client = rpc
	f.confirmer = rpc

	req := solanaTestRequirements(mint, payTo, 1_000)
	payload := solanaTestPayload(t, solanaTestTx(t, f.feePayer, payer, mint, dest, 1_000, 6), req)

	res, err := f.Settle(t.Context(), payload, req)
	require.NoError(t, err)
	require.False(t, res.Success)
	require.Contains(t, res.ErrorMessage, "not confirmed within")
}

func TestSolanaSettleReportsBroadcastFailure(t *testing.T) {
	f := solanaTestFacilitator(t)
	payer := solanaTestAccount(t)
	mint := solanaTestPubkey()
	payTo := solanaTestPubkey()
	dest, _, err := solcommon.FindAssociatedTokenAddress(payTo, mint)
	require.NoError(t, err)

	f.client = &stubRPC{sendErr: errors.New("rpc unavailable")}

	req := solanaTestRequirements(mint, payTo, 1_000)
	payload := solanaTestPayload(t, solanaTestTx(t, f.feePayer, payer, mint, dest, 1_000, 6), req)

	res, err := f.Settle(t.Context(), payload, req)
	require.NoError(t, err)
	require.False(t, res.Success)
	require.Equal(t, types.ErrTransactionFailed.Error(), res.ErrorReason)
	require.Contains(t, res.ErrorMessage, "rpc unavailable")
}

func TestSolanaSettleRejectsNilInputs(t *testing.T) {
	f := solanaTestFacilitator(t)

	res, err := f.Settle(t.Context(), nil, nil)
	require.NoError(t, err)
	require.False(t, res.Success)
	require.Equal(t, types.ErrInvalidPayloadFormat.Error(), res.ErrorReason)
}
