// Package exchange 定义交易所端口：应用层只依赖这里的接口，
// 具体交易所实现放在各自的子包里，由 main.go 注册进来。
package exchange

import (
	"context"
	"errors"
	"time"

	"dex-grid/internal/domain/account"
	"dex-grid/internal/domain/market"
	"dex-grid/internal/domain/order"
	"dex-grid/internal/domain/position"

	"github.com/shopspring/decimal"
)

// ErrNotSupported 表示该交易所不具备此能力，调用方应先查 Capabilities。
var ErrNotSupported = errors.New("exchange: capability not supported")

// Exchange 是查询与交易端口。
//
// 交易方法收发切片而不是单笔，避免上层出现「单笔」和「批量」两套路径。
// 单笔调用传长度为 1 的切片，适配器按 Capabilities.BatchPlace 决定
// 用批量接口还是串行发送。
type Exchange interface {
	Name() string
	Capabilities() Capabilities

	// Markets 返回可交易的【永续合约】市场，供页面的交易对下拉使用。
	//
	// 本系统的策略全部是合约网格（普通网格与马丁网格都带杠杆、都需要
	// 做空与 reduce-only），现货市场既不支持这些语义也没有杠杆，
	// 因此适配器必须把现货过滤掉，不能让它出现在选择列表里。
	// 已下架（非活跃）的合约同样不返回。
	Markets(ctx context.Context) ([]MarketInfo, error)
	// Market 返回单个永续合约市场的元数据。传入现货或未知交易对时返回错误。
	Market(ctx context.Context, symbol string) (market.Market, error)
	// Ticker 返回最新盘口与标记价。
	Ticker(ctx context.Context, symbol string) (Ticker, error)
	// Klines 返回 K 线，供行情分析模块使用。
	Klines(ctx context.Context, symbol, interval string, limit int) ([]market.Kline, error)

	SetLeverage(ctx context.Context, symbol string, leverage int, mode market.MarginMode) error

	PlaceOrders(ctx context.Context, reqs []PlaceRequest) ([]PlaceResult, error)
	CancelOrders(ctx context.Context, reqs []CancelRequest) ([]CancelResult, error)
	// CancelAll 撤销指定交易对上的全部挂单，不得波及其他市场。
	CancelAll(ctx context.Context, symbol string) error

	// 以下三个是对账用的快照查询。
	OpenOrders(ctx context.Context, symbol string) ([]order.Order, error)
	Position(ctx context.Context, symbol string) (position.Position, error)
	Account(ctx context.Context) (account.Snapshot, error)

	Close() error
}

// Streamer 是实时事件流端口。
//
// 与 Exchange 分开是因为两者的实现进度和失败模式都不一样：查询与交易走 REST
// 就能完整工作，事件流需要 WebSocket 与断线重连。拆开后适配器可以先交付
// 可用的交易能力，再补流式能力，而不必先放一个会 panic 的空实现。
type Streamer interface {
	// Subscribe 把订单簿、订单回报、仓位更新合并成一个 channel。
	//
	// 合流而不是三个 channel，是为了让上层的 select 只有少数几个分支，
	// 也避免多 channel 之间乱序导致「先收到成交、后收到挂单确认」。
	Subscribe(ctx context.Context, symbol string) (<-chan StreamEvent, error)
}

// StreamingExchange 是同时具备交易与流式能力的交易所，Runner 要求这个。
type StreamingExchange interface {
	Exchange
	Streamer
}

// Capabilities 描述交易所支持哪些能力，上层据此降级。
type Capabilities struct {
	// BatchPlace 单批最大下单数，0 表示不支持批量。
	BatchPlace  int
	BatchCancel int
	// ModifyOrder 支持改价改量，可省掉一次撤单。
	ModifyOrder bool
	// PostOnly 为 false 时本系统无法运行——除建仓外全部挂单都是 post-only。
	PostOnly   bool
	ReduceOnly bool
	// NativeTPSL 支持交易所原生条件单。
	NativeTPSL bool
	// MaxOpenOrders 单市场最大挂单数，0 表示不限。
	MaxOpenOrders int
}

// MarketInfo 是交易对下拉需要的最小信息。
type MarketInfo struct {
	Symbol      string `json:"symbol"`
	MarketIndex int    `json:"market_index"`
	// Type 恒为永续合约类型，保留字段是为了让页面能直观确认这一点，
	// 也方便将来接入区分多种合约类型的交易所。
	Type      string          `json:"type"`
	Status    string          `json:"status"`
	MarkPrice decimal.Decimal `json:"mark_price"`
	// DailyQuoteVolume 用于给下拉列表排序：成交量大的排前面。
	DailyQuoteVolume decimal.Decimal `json:"daily_quote_volume"`
	MaxLeverage      int             `json:"max_leverage"`
}

// Ticker 是行情快照。
type Ticker struct {
	Symbol string
	Book   market.BookTicker
	Mark   decimal.Decimal
	Index  decimal.Decimal
	Last   decimal.Decimal
	Time   time.Time
}

// PlaceRequest 是一笔下单意图。
type PlaceRequest struct {
	Symbol        string
	ClientOrderID order.ClientOrderID
	Side          order.Side
	Type          order.Type
	// Price 对限价单是挂单价；对市价单是可接受的最差价格（滑点保护）。
	Price      decimal.Decimal
	Quantity   decimal.Decimal
	TIF        order.TIF
	ReduceOnly bool
	// ExpireAt 为零值时由适配器套用默认有效期。
	ExpireAt time.Time
}

// PlaceResult 是单笔下单的结果。批量调用时按入参顺序一一对应。
type PlaceResult struct {
	ClientOrderID order.ClientOrderID
	ExchangeID    string
	TxHash        string
	Err           error
}

type CancelRequest struct {
	Symbol        string
	ClientOrderID order.ClientOrderID
	ExchangeID    string
}

type CancelResult struct {
	ClientOrderID order.ClientOrderID
	TxHash        string
	Err           error
}

// StreamEvent 是事件流上的一条消息。同一时刻只有一个字段非空。
type StreamEvent struct {
	Ticker   *Ticker
	Order    *order.Order
	Position *position.Position
	Account  *account.Snapshot
	// Resync 为真表示流刚重连，本地状态可能已过期，上层应触发全量对账。
	Resync bool
	Err    error
	Time   time.Time
}
