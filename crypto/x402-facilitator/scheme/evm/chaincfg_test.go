package evm

import (
	"math/big"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/stretchr/testify/require"
)

func TestGetDomainConfig(t *testing.T) {
	const (
		baseSepolia     = "eip155:84532"
		usdcBaseSepolia = "0x036CbD53842c5426634e7929541eC2318f3dCF7e"
	)

	t.Run("resolves by token symbol", func(t *testing.T) {
		cfg := GetDomainConfig(baseSepolia, "USDC")
		require.NotNil(t, cfg, "symbol lookup should succeed for known token")
		require.Equal(t, common.HexToAddress(usdcBaseSepolia), cfg.VerifyingContract)
		require.Equal(t, "USDC", cfg.Name)
	})

	t.Run("resolves by contract address", func(t *testing.T) {
		cfg := GetDomainConfig(baseSepolia, usdcBaseSepolia)
		require.NotNil(t, cfg, "address lookup should succeed for known token")
		require.Equal(t, common.HexToAddress(usdcBaseSepolia), cfg.VerifyingContract)
		require.Equal(t, "USDC", cfg.Name)
	})

	t.Run("address lookup is case-insensitive", func(t *testing.T) {
		cfg := GetDomainConfig(baseSepolia, strings.ToLower(usdcBaseSepolia))
		require.NotNil(t, cfg, "lowercase address should still resolve")
		require.Equal(t, common.HexToAddress(usdcBaseSepolia), cfg.VerifyingContract)
	})

	t.Run("symbol lookup and address lookup return the same config", func(t *testing.T) {
		bySymbol := GetDomainConfig(baseSepolia, "USDC")
		byAddress := GetDomainConfig(baseSepolia, usdcBaseSepolia)
		require.NotNil(t, bySymbol)
		require.NotNil(t, byAddress)
		require.Equal(t, bySymbol.VerifyingContract, byAddress.VerifyingContract)
		require.Equal(t, bySymbol.Name, byAddress.Name)
		require.Equal(t, bySymbol.Version, byAddress.Version)
		require.Equal(t, bySymbol.ChainID.String(), byAddress.ChainID.String())
	})

	t.Run("returns nil for unknown chain", func(t *testing.T) {
		require.Nil(t, GetDomainConfig("eip155:999999", "USDC"))
		require.Nil(t, GetDomainConfig("eip155:999999", usdcBaseSepolia))
	})

	t.Run("returns nil for unknown symbol on known chain", func(t *testing.T) {
		require.Nil(t, GetDomainConfig(baseSepolia, "WETH"))
	})

	t.Run("returns nil for unknown contract address on known chain", func(t *testing.T) {
		require.Nil(t, GetDomainConfig(baseSepolia, "0x0000000000000000000000000000000000000000"))
	})
}

func TestGetChainInfoIncludesDefaultPublicNodeEndpoints(t *testing.T) {
	tests := []struct {
		network     string
		networkName string
		chainID     int64
		defaultURL  string
	}{
		{"eip155:1", "Ethereum Mainnet", 1, "https://ethereum-rpc.publicnode.com"},
		{"eip155:11155111", "Ethereum Sepolia", 11155111, "https://ethereum-sepolia-rpc.publicnode.com"},
		{"eip155:8453", "Base", 8453, "https://base-rpc.publicnode.com"},
		{"eip155:84532", "Base Sepolia", 84532, "https://base-sepolia-rpc.publicnode.com"},
		{"eip155:42161", "Arbitrum One", 42161, "https://arbitrum-one-rpc.publicnode.com"},
		{"eip155:421614", "Arbitrum Sepolia", 421614, "https://arbitrum-sepolia-rpc.publicnode.com"},
	}

	for _, tt := range tests {
		t.Run(tt.network, func(t *testing.T) {
			info := GetChainInfo(tt.network)
			require.NotNil(t, info)
			require.Equal(t, tt.networkName, info.NetworkName)
			require.Equal(t, big.NewInt(tt.chainID), info.ChainID)
			require.NotEmpty(t, info.DefaultURLs)
			require.Equal(t, tt.defaultURL, info.DefaultURLs[0])
			require.Equal(t, info.DefaultURLs, GetDefaultURLs(tt.network))
			require.Equal(t, tt.networkName, GetNetworkName(tt.network))
			require.Equal(t, tt.network, GetChainName(big.NewInt(tt.chainID)))
			require.Equal(t, Permit2Address, GetContractAddress(tt.network, "permit2"))
			require.Equal(t, X402ExactPermit2ProxyAddress, GetContractAddress(tt.network, "X402ExactPermit2Proxy"))
		})
	}
}
