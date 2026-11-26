package main

import (
	"encoding/hex"
	"encoding/json"

	"github.com/gosuda/x402-facilitator/api/client"
	"github.com/gosuda/x402-facilitator/scheme/evm"
	"github.com/gosuda/x402-facilitator/types"
	"github.com/rs/zerolog/log"
	"github.com/spf13/cobra"
)

var cmd = &cobra.Command{
	Use:   "x402-client",
	Short: "Start the facilitator client",
	Run:   run,
}

var (
	url     string
	scheme  string
	network string
	token   string
	from    string
	to      string
	amount  string
	privkey string
	method  string
)

func init() {
	fs := cmd.PersistentFlags()

	fs.StringVarP(&url, "url", "u", "http://localhost:9090", "Base URL of the facilitator server")
	fs.StringVarP(&scheme, "scheme", "s", "exact", "Payment scheme to use")
	fs.StringVarP(&network, "network", "n", "eip155:84532", "CAIP-2 network to pay on (e.g. eip155:84532)")
	fs.StringVarP(&token, "token", "t", "USDC", "token contract for sending")
	fs.StringVarP(&from, "from", "F", "", "Sender address")
	fs.StringVarP(&to, "to", "T", "", "Recipient address")
	fs.StringVarP(&amount, "amount", "A", "", "Amount to send")
	fs.StringVarP(&privkey, "privkey", "P", "", "Sender private key")
	fs.StringVarP(&method, "method", "m", "eip3009", "Payment method (eip3009 or permit2)")
}

func main() {
	if err := cmd.Execute(); err != nil {
		log.Fatal().Err(err).Msg("Failed to execute command")
	}
}

func run(cmd *cobra.Command, args []string) {
	client, err := client.NewClient(url)
	if err != nil {
		log.Fatal().Err(err).Msg("Failed to create client")
	}

	// Build a signed payment for the selected scheme
	log.Info().Msg("Sending payment request")
	var paymentPayload *types.PaymentPayload
	var paymentRequirements *types.PaymentRequirements
	switch scheme {
	case "exact":
		priv, err := hex.DecodeString(privkey)
		if err != nil {
			log.Fatal().Err(err).Msg("Failed to decode private key")
		}
		signer := evm.NewRawPrivateSigner(priv)

		var jsonPayload []byte
		switch method {
		case "permit2":
			permit2Payload, err := evm.NewPermit2Payload(network, token, from, to, amount, signer)
			if err != nil {
				log.Fatal().Err(err).Msg("Failed to create Permit2 payload")
			}
			jsonPayload, err = json.Marshal(permit2Payload)
			if err != nil {
				log.Fatal().Err(err).Msg("Failed to marshal Permit2 payload to JSON")
			}
		default: // "eip3009"
			evmPayload, err := evm.NewEVMPayload(network, token, from, to, amount, signer)
			if err != nil {
				log.Fatal().Err(err).Msg("Failed to create EVM payload")
			}
			jsonPayload, err = json.Marshal(evmPayload)
			if err != nil {
				log.Fatal().Err(err).Msg("Failed to marshal EVM payload to JSON")
			}
		}

		// v2 PaymentPayload.Payload is a decoded map, not raw JSON
		var payloadMap map[string]interface{}
		if err := json.Unmarshal(jsonPayload, &payloadMap); err != nil {
			log.Fatal().Err(err).Msg("Failed to decode payload into map")
		}

		paymentRequirements = &types.PaymentRequirements{
			Scheme:  scheme,
			Network: network,
			Asset:   token,
			Amount:  amount,
			PayTo:   to,
		}
		paymentPayload = &types.PaymentPayload{
			X402Version: int(types.X402VersionV2),
			Payload:     payloadMap,
			Accepted:    *paymentRequirements,
		}
	}

	verifyResp, err := client.Verify(cmd.Context(), paymentPayload, paymentRequirements)
	if err != nil {
		log.Fatal().Err(err).Msg("Failed to verify payment")
	}
	if !verifyResp.IsValid {
		log.Error().Str("invalidReason", verifyResp.InvalidReason).Msg("Payment verification failed")
		return
	}

	settleResp, err := client.Settle(cmd.Context(), paymentPayload, paymentRequirements)
	if err != nil {
		log.Fatal().Err(err).Msg("Failed to settle payment")
	}
	if !settleResp.Success {
		log.Error().Msg("Payment settlement failed")
		return
	}
	log.Info().Msg("Payment settled successfully")

}
