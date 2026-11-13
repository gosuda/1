package sui

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestGetNetworkInfoIncludesDefaultPublicNodeEndpoints(t *testing.T) {
	tests := []struct {
		network     string
		networkName string
		networkID   string
		defaultURL  string
	}{
		{"sui:mainnet", "Sui Mainnet", "mainnet", "https://fullnode.mainnet.sui.io:443"},
		{"sui:testnet", "Sui Testnet", "testnet", "https://fullnode.testnet.sui.io:443"},
		{"sui:localnet", "Sui Localnet", "localnet", "http://127.0.0.1:9000"},
	}

	for _, tt := range tests {
		t.Run(tt.network, func(t *testing.T) {
			info := GetNetworkInfo(tt.network)
			require.NotNil(t, info)
			require.Equal(t, tt.network, info.Network)
			require.Equal(t, tt.networkName, info.NetworkName)
			require.Equal(t, tt.networkID, info.NetworkID)
			require.NotEmpty(t, info.DefaultURLs)
			require.Equal(t, tt.defaultURL, info.DefaultURLs[0])
			require.Equal(t, tt.networkName, GetNetworkName(tt.network))
			require.Equal(t, tt.networkID, GetNetworkID(tt.network))
			require.Equal(t, info.DefaultURLs, GetDefaultURLs(tt.network))
		})
	}
}

func TestGetGaslessStablecoinType(t *testing.T) {
	coinType, ok := GetGaslessStablecoinType("sui:mainnet", "USDC")
	require.True(t, ok)
	require.Equal(t, USDCType, coinType)

	coinType, ok = GetGaslessStablecoinType("sui:mainnet", USDCType)
	require.True(t, ok)
	require.Equal(t, USDCType, coinType)

	coinType, ok = GetGaslessStablecoinType("sui:testnet", "USDC")
	require.True(t, ok)
	require.Equal(t, TestnetUSDCType, coinType)

	coinType, ok = GetGaslessStablecoinType("sui:testnet", TestnetUSDCType)
	require.True(t, ok)
	require.Equal(t, TestnetUSDCType, coinType)

	_, ok = GetGaslessStablecoinType("sui:testnet", USDCType)
	require.False(t, ok)

	require.Len(t, GetGaslessStablecoinTypes("sui:mainnet"), len(DefaultGaslessStablecoinTypeList))
	require.Equal(t, TestnetUSDCType, GetGaslessStablecoinTypes("sui:testnet")[0])
	_, ok = GetGaslessStablecoinType("sui:mainnet", "NOT_A_TOKEN")
	require.False(t, ok)
	require.Nil(t, GetNetworkInfo("sui:unknown"))
}

func TestGetGaslessStablecoinDecimals(t *testing.T) {
	decimals, ok := GetGaslessStablecoinDecimals("sui:mainnet", "USDC")
	require.True(t, ok)
	require.Equal(t, uint8(6), decimals)

	decimals, ok = GetGaslessStablecoinDecimals("sui:mainnet", USDCType)
	require.True(t, ok)
	require.Equal(t, uint8(6), decimals)
	require.Equal(t, "10000", MinimumGaslessStablecoinAmount(decimals).String())

	decimals, ok = GetGaslessStablecoinDecimals("sui:testnet", TestnetUSDCType)
	require.True(t, ok)
	require.Equal(t, uint8(6), decimals)

	_, ok = GetGaslessStablecoinDecimals("sui:mainnet", "NOT_A_TOKEN")
	require.False(t, ok)
}

func TestStablecoinAmountToAtomic(t *testing.T) {
	tests := []struct {
		name    string
		network string
		asset   string
		amount  string
		want    string
		wantErr error
	}{
		{name: "symbol mainnet", network: "sui:mainnet", asset: "USDC", amount: "0.01", want: "10000"},
		{name: "whole amount", network: "sui:mainnet", asset: "USDC", amount: "1", want: "1000000"},
		{name: "smallest unit", network: "sui:mainnet", asset: "USDC", amount: "0.000001", want: "1"},
		{name: "zero", network: "sui:mainnet", asset: "USDC", amount: "0", want: "0"},
		{name: "coin type asset", network: "sui:mainnet", asset: USDCType, amount: "0.01", want: "10000"},
		{name: "testnet coin type", network: "sui:testnet", asset: TestnetUSDCType, amount: "0.01", want: "10000"},
		{name: "bare fraction", network: "sui:mainnet", asset: "USDC", amount: ".5", want: "500000"},
		{name: "whitespace trimmed", network: "sui:mainnet", asset: "USDC", amount: " 0.01 ", want: "10000"},
		{name: "all-zero overflow tail truncated", network: "sui:mainnet", asset: "USDC", amount: "1.0000000", want: "1000000"},
		{name: "unknown network falls back to defaults", network: "sui:devnet", asset: "USDC", amount: "0.01", want: "10000"},
		{name: "precision rejected", network: "sui:mainnet", asset: "USDC", amount: "0.0000001", wantErr: ErrStablecoinPrecision},
		{name: "empty", network: "sui:mainnet", asset: "USDC", amount: "", wantErr: ErrInvalidStablecoinAmount},
		{name: "spaces only", network: "sui:mainnet", asset: "USDC", amount: "   ", wantErr: ErrInvalidStablecoinAmount},
		{name: "negative", network: "sui:mainnet", asset: "USDC", amount: "-1", wantErr: ErrInvalidStablecoinAmount},
		{name: "negative fraction", network: "sui:mainnet", asset: "USDC", amount: "-0.01", wantErr: ErrInvalidStablecoinAmount},
		{name: "exponent", network: "sui:mainnet", asset: "USDC", amount: "1e3", wantErr: ErrInvalidStablecoinAmount},
		{name: "double dot", network: "sui:mainnet", asset: "USDC", amount: "1.2.3", wantErr: ErrInvalidStablecoinAmount},
		{name: "trailing dot", network: "sui:mainnet", asset: "USDC", amount: "1.", wantErr: ErrInvalidStablecoinAmount},
		{name: "letters", network: "sui:mainnet", asset: "USDC", amount: "abc", wantErr: ErrInvalidStablecoinAmount},
		{name: "unknown asset", network: "sui:mainnet", asset: "NOT_A_TOKEN", amount: "1", wantErr: ErrUnknownStablecoinAsset},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := StablecoinAmountToAtomic(tt.network, tt.asset, tt.amount)
			if tt.wantErr != nil {
				require.ErrorIs(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.want, got)
		})
	}

	_, err := StablecoinAmountToAtomic("sui:mainnet", "NOT_A_TOKEN", "1")
	require.ErrorIs(t, err, ErrUnknownStablecoinAsset)
	require.ErrorContains(t, err, "NOT_A_TOKEN")
	require.ErrorContains(t, err, "sui:mainnet")
}

func TestFormatStablecoinAtomicAmount(t *testing.T) {
	tests := []struct {
		name    string
		network string
		asset   string
		atomic  string
		want    string
		wantErr error
	}{
		{name: "fractional", network: "sui:mainnet", asset: "USDC", atomic: "10000", want: "0.01"},
		{name: "whole", network: "sui:mainnet", asset: "USDC", atomic: "1000000", want: "1"},
		{name: "zero", network: "sui:mainnet", asset: "USDC", atomic: "0", want: "0"},
		{name: "all zeros", network: "sui:mainnet", asset: "USDC", atomic: "000", want: "0"},
		{name: "leading zeros", network: "sui:mainnet", asset: "USDC", atomic: "0010000", want: "0.01"},
		{name: "trailing fractional zeros trimmed", network: "sui:mainnet", asset: "USDC", atomic: "1500000", want: "1.5"},
		{name: "sub-unit keeps fraction", network: "sui:mainnet", asset: "USDC", atomic: "1", want: "0.000001"},
		{name: "coin type asset", network: "sui:mainnet", asset: USDCType, atomic: "10000", want: "0.01"},
		{name: "unknown network falls back to defaults", network: "sui:devnet", asset: "USDC", atomic: "10000", want: "0.01"},
		{name: "beyond uint64 stays exact", network: "sui:mainnet", asset: "USDC", atomic: "99999999999999999999000000", want: "99999999999999999999"},
		{name: "empty", network: "sui:mainnet", asset: "USDC", atomic: "", wantErr: ErrInvalidStablecoinAmount},
		{name: "negative", network: "sui:mainnet", asset: "USDC", atomic: "-5", wantErr: ErrInvalidStablecoinAmount},
		{name: "fractional atomic", network: "sui:mainnet", asset: "USDC", atomic: "1.5", wantErr: ErrInvalidStablecoinAmount},
		{name: "exponent", network: "sui:mainnet", asset: "USDC", atomic: "1e3", wantErr: ErrInvalidStablecoinAmount},
		{name: "plus sign", network: "sui:mainnet", asset: "USDC", atomic: "+3", wantErr: ErrInvalidStablecoinAmount},
		{name: "unknown asset", network: "sui:mainnet", asset: "NOT_A_TOKEN", atomic: "1", wantErr: ErrUnknownStablecoinAsset},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := FormatStablecoinAtomicAmount(tt.network, tt.asset, tt.atomic)
			if tt.wantErr != nil {
				require.ErrorIs(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.want, got)
		})
	}
}

func TestStablecoinAmountRoundTrip(t *testing.T) {
	tests := []struct {
		name   string
		amount string
	}{
		{name: "fractional", amount: "0.01"},
		{name: "whole", amount: "1"},
		{name: "smallest unit", amount: "0.000001"},
		{name: "zero", amount: "0"},
		{name: "mixed", amount: "123.456"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			atomic, err := StablecoinAmountToAtomic("sui:mainnet", "USDC", tt.amount)
			require.NoError(t, err)
			human, err := FormatStablecoinAtomicAmount("sui:mainnet", "USDC", atomic)
			require.NoError(t, err)
			require.Equal(t, tt.amount, human)
		})
	}
}
