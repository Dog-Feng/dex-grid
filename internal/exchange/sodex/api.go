package sodex

import (
	"strings"

	"github.com/sodex-tech/sodex-go-sdk-public/client"
)

const userAgent = "dex-grid/0.1"

// 端点与链 ID。REST 根路径不含 /api/v1/perps，由官方 client 自己拼接。
const (
	mainnetREST = client.DefaultBaseURL
	testnetREST = client.TestnetBaseURL
	mainnetWS   = "wss://mainnet-gw.sodex.dev/ws/perps"
	testnetWS   = "wss://testnet-gw.sodex.dev/ws/perps"

	mainnetChainID = client.DefaultChainID
	testnetChainID = client.TestnetChainID
)

const (
	perpMarketType = "perp"
	activeStatus   = "TRADING"
)

func defaultREST(network string) string {
	if strings.EqualFold(network, "testnet") {
		return testnetREST
	}
	return mainnetREST
}

func defaultWS(network string) string {
	if strings.EqualFold(network, "testnet") {
		return testnetWS
	}
	return mainnetWS
}

func defaultChainID(network string) uint64 {
	if strings.EqualFold(network, "testnet") {
		return testnetChainID
	}
	return mainnetChainID
}
