package facilitator

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"time"

	solclient "github.com/blocto/solana-go-sdk/client"
	solcommon "github.com/blocto/solana-go-sdk/common"
	soltypes "github.com/blocto/solana-go-sdk/types"
	"github.com/mr-tron/base58"

	"github.com/gosuda/x402-facilitator/scheme"
	"github.com/gosuda/x402-facilitator/types"
)

// solanaTransferCheckedInstruction is the SPL Token program instruction code
// for TransferChecked (https://spl.solana.com/token).
const solanaTransferCheckedInstruction = 12

var _ scheme.Facilitator = (*SolanaFacilitator)(nil)

// txSubmitter broadcasts a signed transaction and returns its signature. It
// exists so tests can stub the Solana RPC boundary.
type txSubmitter interface {
	SendTransaction(ctx context.Context, tx soltypes.Transaction) (string, error)
}

// txConfirmer fetches the settlement status of a submitted transaction. It
// exists so tests can stub the confirmation boundary.
type txConfirmer interface {
	GetTransaction(ctx context.Context, txhash string) (*solclient.Transaction, error)
}

const (
	// solanaSettlementTimeout bounds how long Settle waits for the submitted
	// transaction to reach a confirmed, successful state.
	solanaSettlementTimeout = 30 * time.Second
	// solanaSettlementPollInterval spaces out confirmation status polls.
	solanaSettlementPollInterval = 2 * time.Second
)

// SolanaFacilitator settles x402 "exact" payments on Solana. The payer builds
// and signs an SPL Token TransferChecked transaction that names the
// facilitator's fee payer as the transaction fee payer; the facilitator
// verifies the transfer against the payment requirements, co-signs with its
// fee payer key, submits the transaction to the RPC endpoint, and reports
// success only once the transaction is confirmed on chain.
type SolanaFacilitator struct {
	scheme         types.Scheme
	network        string
	feePayer       soltypes.Account
	client         txSubmitter
	confirmer      txConfirmer
	confirmTimeout time.Duration
	confirmTick    time.Duration
}

// NewSolanaFacilitator builds a Solana facilitator for a CAIP-2 Solana
// network (e.g. "solana:mainnet"). rpcUrl is the Solana JSON-RPC endpoint and
// privateKeyHex is the hex-encoded fee payer keypair whose public key
// resource servers advertise to payers through the supported endpoint.
func NewSolanaFacilitator(network string, rpcUrl string, privateKeyHex string) (*SolanaFacilitator, error) {
	if !strings.HasPrefix(network, "solana:") || strings.TrimPrefix(network, "solana:") == "" {
		return nil, fmt.Errorf("invalid Solana network %q: expected a CAIP-2 identifier like solana:mainnet", network)
	}
	if strings.TrimSpace(rpcUrl) == "" {
		return nil, fmt.Errorf("rpc URL is required")
	}
	privateKey, err := hex.DecodeString(strings.TrimPrefix(strings.TrimSpace(privateKeyHex), "0x"))
	if err != nil {
		return nil, fmt.Errorf("invalid hex private key: %w", err)
	}
	feePayer, err := soltypes.AccountFromBytes(privateKey)
	if err != nil {
		return nil, fmt.Errorf("invalid private key format: %w", err)
	}

	rpcClient := solclient.NewClient(rpcUrl)
	return &SolanaFacilitator{
		scheme:         types.Exact,
		network:        network,
		feePayer:       feePayer,
		client:         rpcClient,
		confirmer:      rpcClient,
		confirmTimeout: solanaSettlementTimeout,
		confirmTick:    solanaSettlementPollInterval,
	}, nil
}

func (s *SolanaFacilitator) Verify(ctx context.Context, payload *types.PaymentPayload, req *types.PaymentRequirements) (*types.PaymentVerifyResponse, error) {
	if payload == nil || req == nil {
		return &types.PaymentVerifyResponse{
			IsValid:       false,
			InvalidReason: types.ErrInvalidPayloadFormat.Error(),
		}, nil
	}

	_, payer, invalid := s.verifyPayment(payload, req)
	if invalid != nil {
		invalid.Payer = payer
		return invalid, nil
	}

	return &types.PaymentVerifyResponse{
		IsValid: true,
		Payer:   payer,
	}, nil
}

func (s *SolanaFacilitator) Settle(ctx context.Context, payload *types.PaymentPayload, req *types.PaymentRequirements) (*types.PaymentSettleResponse, error) {
	network := types.Network("")
	if req != nil {
		network = types.Network(req.Network)
	}
	fail := func(reason, message, payer string) (*types.PaymentSettleResponse, error) {
		return &types.PaymentSettleResponse{
			Success:      false,
			ErrorReason:  reason,
			ErrorMessage: message,
			Payer:        payer,
			Network:      network,
		}, nil
	}

	if payload == nil || req == nil {
		return fail(types.ErrInvalidPayloadFormat.Error(), "payload or requirements is nil", "")
	}

	transaction, payer, invalid := s.verifyPayment(payload, req)
	if invalid != nil {
		return fail(invalid.InvalidReason, invalid.InvalidMessage, payer)
	}

	// Co-sign as the fee payer without disturbing the payer's signature
	// slot. AddSignature matches the signature to a required signer account
	// and fails if the fee payer is not one for this message.
	message, err := transaction.Message.Serialize()
	if err != nil {
		return nil, fmt.Errorf("failed to serialize message: %w", err)
	}
	if err := transaction.AddSignature(s.feePayer.Sign(message)); err != nil {
		return nil, fmt.Errorf("failed to co-sign as fee payer: %w", err)
	}

	signature, err := s.client.SendTransaction(ctx, *transaction)
	if err != nil {
		return fail(types.ErrTransactionFailed.Error(), err.Error(), payer)
	}

	// SendTransaction success only means the node accepted the transaction
	// for processing; report success once it is actually confirmed.
	if err := s.awaitSettlement(ctx, signature); err != nil {
		resp, _ := fail(types.ErrTransactionFailed.Error(), err.Error(), payer)
		resp.Transaction = signature
		return resp, nil
	}

	return &types.PaymentSettleResponse{
		Success:     true,
		Payer:       payer,
		Transaction: signature,
		Network:     network,
	}, nil
}

// awaitSettlement polls the confirmation endpoint until the submitted
// transaction is visible and successful. On-chain failure and confirmation
// timeouts both surface as errors, which Settle maps to a structured
// settlement failure.
func (s *SolanaFacilitator) awaitSettlement(ctx context.Context, signature string) error {
	deadline := time.Now().Add(s.confirmTimeout)
	for {
		result, err := s.confirmer.GetTransaction(ctx, signature)
		if err != nil {
			return fmt.Errorf("failed to fetch transaction status: %w", err)
		}
		if result != nil {
			if result.Meta != nil && result.Meta.Err != nil {
				return fmt.Errorf("transaction failed on chain: %v", result.Meta.Err)
			}
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("transaction %s was not confirmed within %s", signature, s.confirmTimeout)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("settlement aborted: %w", ctx.Err())
		case <-time.After(s.confirmTick):
		}
	}
}

// Supported returns the (scheme, network) kinds this facilitator serves. The
// fee payer address is advertised through the kind extra so payers can build
// the transaction, and through the signers map keyed by the CAIP-2 family.
func (s *SolanaFacilitator) Supported() *types.SupportedResponse {
	return &types.SupportedResponse{
		Kinds: []types.SupportedKind{{
			X402Version: int(types.X402VersionV2),
			Scheme:      string(s.scheme),
			Network:     s.network,
			Extra: map[string]interface{}{
				"feePayer":            s.feePayer.PublicKey.String(),
				"transactionEncoding": "base64",
			},
		}},
		Extensions: []string{},
		Signers: map[string][]string{
			"solana:*": {s.feePayer.PublicKey.String()},
		},
	}
}

// verifyPayment validates the payment envelope and the signed SPL Token
// TransferChecked transaction against the requirements. It returns the
// decoded transaction and the payer address on success; on failure it
// returns a structured invalid response and the payer when recoverable.
func (s *SolanaFacilitator) verifyPayment(payload *types.PaymentPayload, req *types.PaymentRequirements) (*soltypes.Transaction, string, *types.PaymentVerifyResponse) {
	if invalid := s.validateEnvelope(payload, req); invalid != nil {
		return nil, "", invalid
	}

	raw, ok := payload.Payload["transaction"].(string)
	if !ok || strings.TrimSpace(raw) == "" {
		return nil, "", invalidSolanaPayload("payload.transaction (base64 encoded Solana transaction) is required")
	}
	transactionBytes, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		return nil, "", invalidSolanaPayload("payload.transaction is not valid base64: " + err.Error())
	}
	transaction, err := soltypes.TransactionDeserialize(transactionBytes)
	if err != nil {
		return nil, "", &types.PaymentVerifyResponse{
			InvalidReason:  types.ErrInvalidTransaction.Error(),
			InvalidMessage: "payload.transaction is not a valid Solana transaction: " + err.Error(),
		}
	}
	if transaction.Message.Version != soltypes.MessageVersionLegacy {
		return nil, "", &types.PaymentVerifyResponse{
			InvalidReason:  types.ErrInvalidTransaction.Error(),
			InvalidMessage: "only legacy Solana transactions are supported",
		}
	}
	// Minimal supported shape: exactly one instruction, the payment itself.
	// The facilitator co-signs the whole message, so extra instructions
	// (ComputeBudget, Memo, arbitrary spending) are rejected outright.
	if len(transaction.Message.Instructions) != 1 {
		return nil, "", &types.PaymentVerifyResponse{
			InvalidReason:  types.ErrInvalidTransaction.Error(),
			InvalidMessage: fmt.Sprintf("transaction must contain exactly one instruction, got %d", len(transaction.Message.Instructions)),
		}
	}
	// The first required signer is the transaction fee payer. Pinning it to
	// the configured key keeps the facilitator's signature scoped to paying
	// fees; the transfer itself stays authorized by the payer's signature.
	if len(transaction.Message.Accounts) == 0 || transaction.Message.Accounts[0] != s.feePayer.PublicKey {
		return nil, "", &types.PaymentVerifyResponse{
			InvalidReason:  types.ErrInvalidTransaction.Error(),
			InvalidMessage: "transaction fee payer is not the facilitator fee payer",
		}
	}

	transfer, invalid := solanaTransferChecked(transaction.Message, req)
	if invalid != nil {
		return nil, "", invalid
	}

	// The facilitator only signs as the fee payer; its key must not appear
	// as an instruction account, or co-signing would approve spending from it.
	if solanaReferencesAccount(transaction.Message, transaction.Message.Instructions[0], s.feePayer.PublicKey) {
		return nil, "", &types.PaymentVerifyResponse{
			InvalidReason:  types.ErrInvalidTransaction.Error(),
			InvalidMessage: "transaction uses the facilitator fee payer as an instruction account",
		}
	}

	messageBytes, err := transaction.Message.Serialize()
	if err != nil {
		return nil, "", &types.PaymentVerifyResponse{
			InvalidReason:  types.ErrInvalidTransaction.Error(),
			InvalidMessage: "failed to serialize transaction message: " + err.Error(),
		}
	}

	payer, invalid := solanaPayer(messageBytes, transaction, transfer)
	if invalid != nil {
		return nil, "", invalid
	}
	return &transaction, payer, nil
}

func invalidSolanaPayload(message string) *types.PaymentVerifyResponse {
	return &types.PaymentVerifyResponse{
		InvalidReason:  types.ErrInvalidPayloadFormat.Error(),
		InvalidMessage: message,
	}
}

// validateEnvelope mirrors the v2 envelope checks: version, scheme, network,
// asset, amount, and pay-to must agree between the accepted requirements the
// payer signed and the requirements the resource server forwards.
func (s *SolanaFacilitator) validateEnvelope(payload *types.PaymentPayload, req *types.PaymentRequirements) *types.PaymentVerifyResponse {
	if payload.X402Version != int(types.X402VersionV2) {
		return &types.PaymentVerifyResponse{InvalidReason: types.ErrInvalidPayloadFormat.Error()}
	}
	if payload.Accepted.Scheme != string(s.scheme) || req.Scheme != string(s.scheme) {
		return &types.PaymentVerifyResponse{InvalidReason: types.ErrIncompatibleScheme.Error()}
	}
	if payload.Accepted.Network != s.network || req.Network != s.network {
		return &types.PaymentVerifyResponse{InvalidReason: types.ErrNetworkMismatch.Error()}
	}
	if strings.TrimSpace(payload.Accepted.Asset) != strings.TrimSpace(req.Asset) {
		return &types.PaymentVerifyResponse{InvalidReason: types.ErrTokenMismatch.Error()}
	}
	if payload.Accepted.Amount != req.Amount {
		return &types.PaymentVerifyResponse{InvalidReason: types.ErrAmountMismatch.Error()}
	}
	if strings.TrimSpace(payload.Accepted.PayTo) != strings.TrimSpace(req.PayTo) || strings.TrimSpace(req.PayTo) == "" {
		return &types.PaymentVerifyResponse{InvalidReason: types.ErrRecipientMismatch.Error()}
	}
	return nil
}

// solanaCompiledTransfer is the decoded SPL Token TransferChecked instruction
// with its accounts resolved against the message account list.
type solanaCompiledTransfer struct {
	from      solcommon.PublicKey
	mint      solcommon.PublicKey
	to        solcommon.PublicKey
	authority solcommon.PublicKey
	signers   []solcommon.PublicKey
	amount    uint64
	decimals  uint8
}

// solanaTransferChecked locates the single SPL Token TransferChecked
// instruction in the message and validates it against the requirements: the
// mint, amount, and destination must match, and the destination token account
// must be the associated token account of payTo.
func solanaTransferChecked(message soltypes.Message, req *types.PaymentRequirements) (*solanaCompiledTransfer, *types.PaymentVerifyResponse) {
	invalid := func(reason, message string) *types.PaymentVerifyResponse {
		return &types.PaymentVerifyResponse{InvalidReason: reason, InvalidMessage: message}
	}

	amount, err := strconv.ParseUint(strings.TrimSpace(req.Amount), 10, 64)
	if err != nil {
		return nil, invalid(types.ErrAmountMismatch.Error(), "requirements amount is not an unsigned integer: "+err.Error())
	}

	compiled := message.Instructions[0]
	programID, err := solanaAccount(message, compiled.ProgramIDIndex)
	if err != nil || programID != solcommon.TokenProgramID {
		return nil, invalid(types.ErrInvalidTransaction.Error(), "transaction instruction is not an SPL Token program instruction")
	}
	if len(compiled.Data) == 0 || compiled.Data[0] != solanaTransferCheckedInstruction {
		return nil, invalid(types.ErrInvalidTransaction.Error(), "transaction instruction is not SPL Token TransferChecked")
	}
	transfer, err := solanaDecodeTransferChecked(message, compiled)
	if err != nil {
		return nil, invalid(types.ErrInvalidTransaction.Error(), "malformed SPL TransferChecked instruction: "+err.Error())
	}

	mint, err := solanaPublicKey(req.Asset)
	if err != nil {
		return nil, invalid(types.ErrInvalidToken.Error(), "requirements asset is not a valid Solana public key: "+err.Error())
	}
	if transfer.mint != mint {
		return nil, invalid(types.ErrTokenMismatch.Error(), "transaction mint does not match the required asset")
	}
	if transfer.amount != amount {
		return nil, invalid(types.ErrAmountMismatch.Error(), fmt.Sprintf("transaction amount %d does not match required amount %d", transfer.amount, amount))
	}
	payTo, err := solanaPublicKey(req.PayTo)
	if err != nil {
		return nil, invalid(types.ErrRecipientMismatch.Error(), "requirements payTo is not a valid Solana public key: "+err.Error())
	}
	expectedDestination, _, err := solcommon.FindAssociatedTokenAddress(payTo, mint)
	if err != nil {
		return nil, invalid(types.ErrRecipientMismatch.Error(), "failed to derive associated token account: "+err.Error())
	}
	if transfer.to != expectedDestination {
		return nil, invalid(types.ErrRecipientMismatch.Error(), "transaction destination is not the associated token account of payTo")
	}
	return transfer, nil
}

// solanaDecodeTransferChecked decodes the SPL Token TransferChecked wire
// format: instruction u8, amount u64 LE, decimals u8, followed by the
// [source, mint, destination, authority, ...multisig signers] account list.
func solanaDecodeTransferChecked(message soltypes.Message, compiled soltypes.CompiledInstruction) (*solanaCompiledTransfer, error) {
	if len(compiled.Data) != 10 {
		return nil, fmt.Errorf("data length %d, want 10", len(compiled.Data))
	}
	if len(compiled.Accounts) < 4 {
		return nil, fmt.Errorf("account count %d, want at least 4", len(compiled.Accounts))
	}

	resolve := func(index int) (solcommon.PublicKey, error) {
		return solanaAccount(message, index)
	}
	from, err := resolve(compiled.Accounts[0])
	if err != nil {
		return nil, err
	}
	mint, err := resolve(compiled.Accounts[1])
	if err != nil {
		return nil, err
	}
	to, err := resolve(compiled.Accounts[2])
	if err != nil {
		return nil, err
	}
	authority, err := resolve(compiled.Accounts[3])
	if err != nil {
		return nil, err
	}
	signers := make([]solcommon.PublicKey, 0, len(compiled.Accounts)-4)
	for _, index := range compiled.Accounts[4:] {
		signer, err := resolve(index)
		if err != nil {
			return nil, err
		}
		signers = append(signers, signer)
	}

	return &solanaCompiledTransfer{
		from:      from,
		mint:      mint,
		to:        to,
		authority: authority,
		signers:   signers,
		amount:    binary.LittleEndian.Uint64(compiled.Data[1:9]),
		decimals:  compiled.Data[9],
	}, nil
}

// solanaPayer proves the payment was authorized by the transfer authority.
// The first supported version is single-payer only: the authority must be
// the second required signer (the first is the facilitator fee payer) and
// carry a valid Ed25519 signature over the serialized message. SPL multisig
// signers are rejected.
func solanaPayer(messageBytes []byte, transaction soltypes.Transaction, transfer *solanaCompiledTransfer) (string, *types.PaymentVerifyResponse) {
	invalid := func(reason, message string) *types.PaymentVerifyResponse {
		return &types.PaymentVerifyResponse{InvalidReason: reason, InvalidMessage: message}
	}

	if len(transfer.signers) > 0 {
		return "", invalid(types.ErrInvalidTransaction.Error(), "SPL multisig payments are not supported")
	}
	if transaction.Message.Header.NumRequireSignatures != 2 {
		return "", invalid(types.ErrInvalidTransaction.Error(), "transaction must have exactly the fee payer and the payer as required signers")
	}

	payerIndex := -1
	for i, account := range transaction.Message.Accounts {
		if account == transfer.authority {
			payerIndex = i
			break
		}
	}
	if payerIndex != 1 {
		return "", invalid(types.ErrInvalidSignature.Error(), "payment authority is not the payer required signer")
	}
	var signature []byte
	if payerIndex < len(transaction.Signatures) {
		signature = transaction.Signatures[payerIndex]
	}
	if !ed25519.Verify(ed25519.PublicKey(transfer.authority[:]), messageBytes, signature) {
		return "", invalid(types.ErrInvalidSignature.Error(), "payment authority did not sign the transaction")
	}
	return transfer.authority.String(), nil
}

// solanaAccount resolves a compiled instruction account index against the
// message account list.
func solanaAccount(message soltypes.Message, index int) (solcommon.PublicKey, error) {
	if index < 0 || index >= len(message.Accounts) {
		return solcommon.PublicKey{}, fmt.Errorf("account index %d out of range", index)
	}
	return message.Accounts[index], nil
}

// solanaReferencesAccount reports whether a compiled instruction references
// the given account, either as its program or in its account list.
func solanaReferencesAccount(message soltypes.Message, compiled soltypes.CompiledInstruction, key solcommon.PublicKey) bool {
	if program, err := solanaAccount(message, compiled.ProgramIDIndex); err == nil && program == key {
		return true
	}
	for _, index := range compiled.Accounts {
		if account, err := solanaAccount(message, index); err == nil && account == key {
			return true
		}
	}
	return false
}

func solanaPublicKey(value string) (solcommon.PublicKey, error) {
	decoded, err := base58.Decode(strings.TrimSpace(value))
	if err != nil {
		return solcommon.PublicKey{}, err
	}
	if len(decoded) != 32 {
		return solcommon.PublicKey{}, fmt.Errorf("decoded length %d, want 32", len(decoded))
	}
	return solcommon.PublicKeyFromBytes(decoded), nil
}
