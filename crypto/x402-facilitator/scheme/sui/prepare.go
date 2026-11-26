package sui

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// PreparedPayment is the unsigned artifact set a payer needs to fund an x402
// Sui gasless-stablecoin payment. Both transactions are base64 encoded
// unsigned TransactionData bytes sharing one expiration window: sign them
// with NewSignedPayload and submit them in order with
// ExecuteSignedTransactionBlock.
type PreparedPayment struct {
	// PaymentAmount is the exact amount the payment transaction moves to the
	// recipient, normalized from the request.
	PaymentAmount string

	// ConsolidationTransaction moves the sender's owned coin objects into
	// their own address balance. It is empty when the address balance
	// already covers the payment amount.
	ConsolidationTransaction string
	// ConsolidationAmount is the total moved by
	// ConsolidationTransaction, "0" when there is none.
	ConsolidationAmount string
	// CoinObjects are the non-zero coin objects consumed by
	// ConsolidationTransaction; nil when there is none.
	CoinObjects []OwnedCoinObject

	// PaymentTransaction moves PaymentAmount to the recipient.
	PaymentTransaction string

	// Expiration is the validity window embedded in both transactions.
	Expiration TransactionExpiration
}

// PreparePayment resolves what a payer must sign to fund a gasless-stablecoin
// payment without signing or submitting anything. When the sender's address
// balance does not cover the amount it selects their non-zero coin objects
// for a consolidation transaction into their own address balance, and it
// always builds the amount-based payment transaction to the recipient.
func PreparePayment(ctx context.Context, payment GaslessStablecoinObjectBalancePayment) (*PreparedPayment, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	sender := NormalizeAddress(payment.Sender)
	if sender == "" {
		return nil, errors.New("empty sender")
	}
	recipient := NormalizeAddress(payment.Recipient)
	if recipient == "" {
		return nil, errors.New("empty recipient")
	}
	paymentAmount, err := strconv.ParseUint(strings.TrimSpace(payment.Amount), 10, 64)
	if err != nil || paymentAmount == 0 {
		return nil, fmt.Errorf("invalid amount: %s", payment.Amount)
	}
	coinType, err := resolveGaslessStablecoinAsset(payment.Network, payment.Asset)
	if err != nil {
		return nil, err
	}

	client, err := NewClientForNetwork(payment.Network, payment.Endpoints)
	if err != nil {
		return nil, err
	}
	defer client.Close()

	balance, err := client.Balance(ctx, sender, coinType)
	if err != nil {
		return nil, err
	}
	var nonZeroCoinObjects []OwnedCoinObject
	var consolidationAmount uint64
	if balance.AddressBalance < paymentAmount {
		if balance.CoinBalance < paymentAmount-balance.AddressBalance {
			return nil, fmt.Errorf("insufficient %s balance: need %d, address balance %d, coin object balance %d", coinType, paymentAmount, balance.AddressBalance, balance.CoinBalance)
		}

		coinObjects, err := client.ListOwnedCoinObjects(ctx, sender, coinType)
		if err != nil {
			return nil, err
		}
		nonZeroCoinObjects = make([]OwnedCoinObject, 0, len(coinObjects))
		for _, coinObject := range coinObjects {
			if coinObject.Balance == 0 {
				continue
			}
			nonZeroCoinObjects = append(nonZeroCoinObjects, coinObject)
		}

		for _, coinObject := range nonZeroCoinObjects {
			if consolidationAmount > ^uint64(0)-coinObject.Balance {
				return nil, errors.New("coin object balance sum overflows uint64")
			}
			consolidationAmount += coinObject.Balance
		}

		if consolidationAmount < paymentAmount-balance.AddressBalance {
			return nil, fmt.Errorf("insufficient %s balance: need %d, address balance %d, coin object balance %d", coinType, paymentAmount, balance.AddressBalance, consolidationAmount)
		}
	}

	info := GetNetworkInfo(payment.Network)
	if info == nil {
		return nil, fmt.Errorf("unsupported Sui network %q", payment.Network)
	}
	expiration, err := client.ResolveGaslessStablecoinExpiration(ctx, info.ChainDigest)
	if err != nil {
		return nil, err
	}

	prepared := &PreparedPayment{
		PaymentAmount:       strconv.FormatUint(paymentAmount, 10),
		ConsolidationAmount: "0",
		Expiration:          *expiration,
	}
	if consolidationAmount > 0 {
		consolidationTxBytes, err := BuildCoinObjectsToAddressBalanceTransferTransaction(ctx, CoinObjectsToAddressBalanceTransfer{
			Sender:      sender,
			Recipient:   sender,
			Network:     payment.Network,
			Asset:       payment.Asset,
			CoinObjects: nonZeroCoinObjects,
			Endpoints:   payment.Endpoints,
			Expiration:  expiration,
		})
		if err != nil {
			return nil, err
		}
		prepared.ConsolidationTransaction = base64.StdEncoding.EncodeToString(consolidationTxBytes)
		prepared.ConsolidationAmount = strconv.FormatUint(consolidationAmount, 10)
		prepared.CoinObjects = nonZeroCoinObjects
	}

	paymentTxBytes, err := BuildGaslessStablecoinTransferTransaction(ctx, GaslessStablecoinTransfer{
		Sender:     sender,
		Recipient:  recipient,
		Network:    payment.Network,
		Asset:      payment.Asset,
		Amount:     prepared.PaymentAmount,
		Endpoints:  payment.Endpoints,
		Expiration: expiration,
	})
	if err != nil {
		return nil, err
	}
	prepared.PaymentTransaction = base64.StdEncoding.EncodeToString(paymentTxBytes)
	return prepared, nil
}
