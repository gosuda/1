package evm

import (
	"maps"
	"math/big"
	"slices"
	"strings"

	"github.com/ethereum/go-ethereum/common"
)

var (
	// Permit2 canonical contract address (same on all EVM chains via CREATE2)
	Permit2Address = common.HexToAddress("0x000000000022D473030F116dDEE9F6B43aC78BA3")

	// x402 exact payment proxy for Permit2
	X402ExactPermit2ProxyAddress = common.HexToAddress("0x402085c248EeA27D92E8b30b2C58ed07f9E20001")
)

const Permit2DeadlineBuffer int64 = 6

func GetTokenAddress(chain, asset string) common.Address {
	if strings.HasPrefix(asset, "0x") && len(asset) == 42 {
		return common.HexToAddress(asset)
	}
	if domainConfig := GetDomainConfig(chain, asset); domainConfig != nil {
		return domainConfig.VerifyingContract
	}
	return common.Address{}
}

func GetChainName(chainID *big.Int) string {
	if chainID == nil {
		return ""
	}
	return chainName[int(chainID.Int64())]
}

// chainName maps an EVM chain ID to its CAIP-2 network identifier.
// Entries here must have a corresponding chainInfo row below — otherwise
// NewEVMFacilitator would accept the network at constructor time and then
// fail later inside GetDomainConfig at verify/settle time.
var chainName = map[int]string{
	1:        "eip155:1",
	11155111: "eip155:11155111",
	8453:     "eip155:8453",
	84532:    "eip155:84532",
	42161:    "eip155:42161",
	421614:   "eip155:421614",
}

type ChainInfo struct {
	ChainID        *big.Int
	NetworkName    string
	DefaultURLs    []string
	Contracts      map[string]common.Address
	TokenContracts map[string]DomainConfig
}

func GetChainInfo(chain string) *ChainInfo {
	chainInfo, ok := chainInfo[chain]
	if !ok {
		return nil
	}
	chainInfo.DefaultURLs = slices.Clone(chainInfo.DefaultURLs)
	chainInfo.Contracts = maps.Clone(chainInfo.Contracts)
	chainInfo.TokenContracts = maps.Clone(chainInfo.TokenContracts)
	return &chainInfo
}

func GetChainID(chain string) *big.Int {
	chainInfo, ok := chainInfo[chain]
	if !ok {
		return nil
	}
	return chainInfo.ChainID
}

func GetNetworkName(chain string) string {
	chainInfo, ok := chainInfo[chain]
	if !ok {
		return ""
	}
	return chainInfo.NetworkName
}

func GetDefaultURLs(chain string) []string {
	chainInfo, ok := chainInfo[chain]
	if !ok {
		return nil
	}
	return slices.Clone(chainInfo.DefaultURLs)
}

func GetContractAddress(chain, name string) common.Address {
	chainInfo, ok := chainInfo[chain]
	if !ok {
		return common.Address{}
	}
	for contractName, contractAddress := range chainInfo.Contracts {
		if strings.EqualFold(contractName, name) {
			return contractAddress
		}
	}
	return common.Address{}
}

func GetDomainConfig(chain, asset string) *DomainConfig {
	info, ok := chainInfo[chain]
	if !ok {
		return nil
	}
	// v2 callers send the token contract address; reverse-resolve by VerifyingContract.
	if strings.HasPrefix(asset, "0x") && len(asset) == 42 {
		addr := common.HexToAddress(asset)
		for _, cfg := range info.TokenContracts {
			if cfg.VerifyingContract == addr {
				return &cfg
			}
		}
		return nil
	}
	cfg, ok := info.TokenContracts[asset]
	if !ok {
		return nil
	}
	return &cfg
}

var chainInfo = map[string]ChainInfo{
	"eip155:1": {
		ChainID:     big.NewInt(1),
		NetworkName: "Ethereum Mainnet",
		DefaultURLs: []string{
			"https://ethereum-rpc.publicnode.com",
		},
		Contracts: defaultProtocolContracts(),
		TokenContracts: map[string]DomainConfig{
			"USDC": {
				Name:              "USD Coin",
				Version:           "2",
				ChainID:           big.NewInt(1),
				VerifyingContract: common.HexToAddress("0xA0b86991c6218b36c1d19D4a2e9Eb0cE3606eB48"),
			},
		},
	},
	"eip155:11155111": {
		ChainID:     big.NewInt(11155111),
		NetworkName: "Ethereum Sepolia",
		DefaultURLs: []string{
			"https://ethereum-sepolia-rpc.publicnode.com",
		},
		Contracts: defaultProtocolContracts(),
		TokenContracts: map[string]DomainConfig{
			"USDC": {
				Name:              "USDC",
				Version:           "2",
				ChainID:           big.NewInt(11155111),
				VerifyingContract: common.HexToAddress("0x1c7D4B196Cb0C7B01d743Fbc6116a902379C7238"),
			},
		},
	},
	"eip155:8453": {
		ChainID:     big.NewInt(8453),
		NetworkName: "Base",
		DefaultURLs: []string{
			"https://base-rpc.publicnode.com",
			"https://mainnet.base.org",
		},
		Contracts: defaultProtocolContracts(),
		TokenContracts: map[string]DomainConfig{
			"USDC": {
				Name:              "USD Coin",
				Version:           "2",
				ChainID:           big.NewInt(8453),
				VerifyingContract: common.HexToAddress("0x833589fCD6eDb6E08f4c7C32D4f71b54bdA02913"),
			},
		},
	},
	"eip155:84532": {
		ChainID:     big.NewInt(84532),
		NetworkName: "Base Sepolia",
		DefaultURLs: []string{
			"https://base-sepolia-rpc.publicnode.com",
			"https://sepolia.base.org",
		},
		Contracts: defaultProtocolContracts(),
		TokenContracts: map[string]DomainConfig{
			"USDC": {
				Name:              "USDC",
				Version:           "2",
				ChainID:           big.NewInt(84532),
				VerifyingContract: common.HexToAddress("0x036CbD53842c5426634e7929541eC2318f3dCF7e"),
			},
		},
	},
	"eip155:42161": {
		ChainID:     big.NewInt(42161),
		NetworkName: "Arbitrum One",
		DefaultURLs: []string{
			"https://arbitrum-one-rpc.publicnode.com",
			"https://arb1.arbitrum.io/rpc",
		},
		Contracts: defaultProtocolContracts(),
		TokenContracts: map[string]DomainConfig{
			"USDC": {
				Name:              "USD Coin",
				Version:           "2",
				ChainID:           big.NewInt(42161),
				VerifyingContract: common.HexToAddress("0xaf88d065e77c8cC2239327C5EDb3A432268e5831"),
			},
		},
	},
	"eip155:421614": {
		ChainID:     big.NewInt(421614),
		NetworkName: "Arbitrum Sepolia",
		DefaultURLs: []string{
			"https://arbitrum-sepolia-rpc.publicnode.com",
			"https://sepolia-rollup.arbitrum.io/rpc",
		},
		Contracts: defaultProtocolContracts(),
		TokenContracts: map[string]DomainConfig{
			"USDC": {
				Name:              "USDC",
				Version:           "2",
				ChainID:           big.NewInt(421614),
				VerifyingContract: common.HexToAddress("0x75faf114eafb1BDbe2F0316DF893fd58CE46AA4d"),
			},
		},
	},
}

func defaultProtocolContracts() map[string]common.Address {
	return map[string]common.Address{
		"Permit2":               Permit2Address,
		"X402ExactPermit2Proxy": X402ExactPermit2ProxyAddress,
	}
}
