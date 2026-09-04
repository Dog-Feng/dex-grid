package sodex

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"dex-grid/internal/domain/market"
	"dex-grid/internal/domain/order"
	"dex-grid/internal/domain/position"
	"dex-grid/internal/exchange"

	"github.com/shopspring/decimal"
	"github.com/sodex-tech/sodex-go-sdk-public/client"
	"github.com/sodex-tech/sodex-go-sdk-public/common/enums"
)

// parseDec 把交易所返回的十进制字符串转成 Decimal；空串与非法值当 0。
func parseDec(s string) decimal.Decimal {
	s = strings.TrimSpace(s)
	if s == "" || s == "null" {
		return decimal.Zero
	}
	d, err := decimal.NewFromString(s)
	if err != nil {
		return decimal.Zero
	}
	return d
}

// flexBool 同时接受 true/false、"true"/"false"、0/1。SODEx 订单推送的 m 字段是字符串。
type flexBool bool

func (b *flexBool) UnmarshalJSON(raw []byte) error {
	switch s := strings.ToLower(strings.TrimSpace(strings.Trim(string(raw), `"`))); s {
	case "true", "1", "yes":
		*b = true
	case "false", "0", "no", "", "null":
		*b = false
	default:
		// SODEx 实盘里 m 有时是手续费/保证金小数，不能把整帧订单回报丢掉。
		*b = false
	}
	return nil
}

// flexString 同时接受 JSON 字符串和数字（SODEx 的 c / clOrdID 两种都出现过）。
type flexString string

func (s *flexString) UnmarshalJSON(raw []byte) error {
	raw = []byte(strings.TrimSpace(string(raw)))
	if len(raw) == 0 || string(raw) == "null" {
		*s = ""
		return nil
	}
	if raw[0] == '"' {
		var v string
		if err := json.Unmarshal(raw, &v); err != nil {
			return err
		}
		*s = flexString(v)
		return nil
	}
	*s = flexString(string(raw))
	return nil
}

func (s flexString) String() string { return string(s) }

// flexInt64 同时接受 JSON 数字和数字字符串。
type flexInt64 int64

func (n *flexInt64) UnmarshalJSON(raw []byte) error {
	raw = []byte(strings.TrimSpace(string(raw)))
	if len(raw) == 0 || string(raw) == "null" {
		*n = 0
		return nil
	}
	var v int64
	if err := json.Unmarshal(raw, &v); err == nil {
		*n = flexInt64(v)
		return nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return fmt.Errorf("sodex: 无法解析整数 %s", raw)
	}
	s = strings.TrimSpace(s)
	if s == "" {
		*n = 0
		return nil
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return fmt.Errorf("sodex: 无法解析整数 %s", raw)
	}
	*n = flexInt64(v)
	return nil
}

func (n flexInt64) Int64() int64 { return int64(n) }

func derefInt(p *int, fallback int) int {
	if p == nil || *p < 1 {
		return fallback
	}
	return *p
}

func toMarket(s client.Symbol) market.Market {
	tick := parseDec(s.TickSize)
	if !tick.IsPositive() && s.PricePrecision > 0 {
		tick = decimal.New(1, -int32(s.PricePrecision))
	}
	lot := parseDec(s.StepSize)
	if !lot.IsPositive() && s.QuantityPrecision > 0 {
		lot = decimal.New(1, -int32(s.QuantityPrecision))
	}
	return market.Market{
		Symbol:          s.Symbol,
		TickSize:        tick,
		LotSize:         lot,
		MinQty:          parseDec(s.MinQuantity),
		MinNotional:     parseDec(s.MinNotional),
		MaxLeverage:     derefInt(s.MaxLeverage, 1),
		PriceDecimals:   int32(s.PricePrecision),
		SizeDecimals:    int32(s.QuantityPrecision),
		MakerFeeRate:    parseDec(s.MakerFee),
		TakerFeeRate:    parseDec(s.TakerFee),
		MaintMarginRate: decimal.Zero,
	}
}

func toMarketInfo(s client.Symbol, tick client.Ticker) exchange.MarketInfo {
	mark := parseDec(ptrStr(tick.MarkPrice))
	if !mark.IsPositive() {
		mark = parseDec(tick.LastPrice)
	}
	return exchange.MarketInfo{
		Symbol:           s.Symbol,
		MarketIndex:      int(s.SymbolID),
		Type:             perpMarketType,
		Status:           s.Status,
		MarkPrice:        mark,
		DailyQuoteVolume: parseDec(tick.QuoteVolume),
		MaxLeverage:      derefInt(s.MaxLeverage, 1),
	}
}

func ptrStr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func isActiveStatus(status string) bool {
	return strings.EqualFold(strings.TrimSpace(status), activeStatus)
}

func clOrdIDString(id order.ClientOrderID) string {
	return strconv.FormatUint(uint64(id), 10)
}

func parseClOrdID(s string) order.ClientOrderID {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	v, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return 0
	}
	return order.ClientOrderID(v)
}

func toSide(s order.Side) enums.OrderSide {
	if s == order.Sell {
		return enums.OrderSideSell
	}
	return enums.OrderSideBuy
}

func sideFromString(s string) order.Side {
	switch strings.ToUpper(strings.TrimSpace(s)) {
	case "SELL", "2", "ASK":
		return order.Sell
	default:
		return order.Buy
	}
}

func toTIF(t order.TIF) (enums.TimeInForce, error) {
	switch t {
	case order.PostOnly:
		return enums.TimeInForceGTX, nil
	case order.IOC:
		return enums.TimeInForceIOC, nil
	case order.GTC:
		return enums.TimeInForceGTC, nil
	default:
		return 0, fmt.Errorf("不支持的有效期策略 %s", t)
	}
}

func tifFromString(s string) order.TIF {
	switch strings.ToUpper(strings.TrimSpace(s)) {
	case "GTX", "POST_ONLY", "POST-ONLY", "POSTONLY", "4":
		return order.PostOnly
	case "IOC", "3":
		return order.IOC
	default:
		return order.GTC
	}
}

func typeFromString(s string) order.Type {
	switch strings.ToUpper(strings.TrimSpace(s)) {
	case "MARKET", "2":
		return order.Market
	default:
		return order.Limit
	}
}

func toMarginMode(m market.MarginMode) enums.MarginMode {
	if m == market.MarginCross {
		return enums.MarginModeCross
	}
	return enums.MarginModeIsolated
}

func marginModeFromString(s string) market.MarginMode {
	switch strings.ToUpper(strings.TrimSpace(s)) {
	case "CROSS", "2":
		return market.MarginCross
	default:
		return market.MarginIsolated
	}
}

// stateFromStatus 把 SODEx 订单状态映射成领域状态。
//
// GTX（post-only）可能先部分成交再把剩余量拒绝。带成交量的 REJECTED
// 按「剩余已结束」处理成 Canceled，避免格子把已成交部分当成整单拒绝而重挂双份。
func stateFromStatus(status string, filled, total decimal.Decimal, reason string) (order.State, string) {
	switch strings.ToUpper(strings.TrimSpace(status)) {
	case "NEW", "PENDING_NEW", "PENDING_MODIFY", "PENDING_REPLACE", "TRIGGERED", "REPLACED":
		if filled.IsPositive() && filled.LessThan(total) {
			return order.StatePartiallyFilled, ""
		}
		return order.StateOpen, ""
	case "PARTIALLY_FILLED":
		return order.StatePartiallyFilled, ""
	case "FILLED":
		return order.StateFilled, ""
	case "CANCELED", "CANCELLED", "PENDING_CANCEL":
		return order.StateCanceled, ""
	case "EXPIRED":
		return order.StateExpired, status
	case "REJECTED":
		if filled.IsPositive() && filled.GreaterThanOrEqual(total) {
			return order.StateFilled, ""
		}
		if filled.IsPositive() {
			return order.StateCanceled, reason
		}
		if isPostOnlyReason(reason) || isPostOnlyReason(status) {
			return order.StateRejected, reason
		}
		return order.StateRejected, firstNonEmpty(reason, status)
	}
	if filled.IsPositive() && filled.LessThan(total) {
		return order.StatePartiallyFilled, status
	}
	return order.StateCanceled, "未知订单状态: " + status
}

func isPostOnlyReason(s string) bool {
	lower := strings.ToLower(s)
	for _, kw := range []string{
		"post only", "post-only", "postonly", "gtx",
		"would match", "would cross", "crosses the book",
		"immediately match", "taker order not allowed",
	} {
		if strings.Contains(lower, kw) {
			return true
		}
	}
	return false
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}

func toOrderFromREST(o client.Order, symbol string) order.Order {
	if symbol == "" {
		symbol = o.Symbol
	}
	filled := parseDec(o.ExecutedQty)
	total := parseDec(o.OrigQty)
	state, reason := stateFromStatus(o.Status, filled, total, "")
	avg := decimal.Zero
	if filled.IsPositive() && parseDec(o.ExecutedValue).IsPositive() {
		avg = parseDec(o.ExecutedValue).Div(filled)
	}
	return order.Order{
		ClientOrderID: parseClOrdID(o.ClOrdID),
		ExchangeID:    strconv.FormatUint(o.OrderID, 10),
		Symbol:        symbol,
		Side:          sideFromString(o.Side),
		Type:          typeFromString(o.Type),
		TIF:           tifFromString(o.TimeInForce),
		Price:         parseDec(o.Price),
		Quantity:      total,
		FilledQty:     filled,
		AvgFillPrice:  avg,
		State:         state,
		RejectReason:  reason,
		UpdatedAt:     unixMilli(o.UpdatedAt),
	}
}

func toOrderFromWS(o wsAccountOrderUpdate, symbol string) order.Order {
	if symbol == "" {
		symbol = o.Symbol
	}
	filled := parseDec(o.FilledQty.String())
	total := parseDec(o.OrigQty.String())
	state, reason := stateFromStatus(o.Status.String(), filled, total, o.Reason.String())
	avg := decimal.Zero
	if filled.IsPositive() && parseDec(o.FilledValue.String()).IsPositive() {
		avg = parseDec(o.FilledValue.String()).Div(filled)
	} else if parseDec(o.LastPrice.String()).IsPositive() && filled.IsPositive() {
		avg = parseDec(o.LastPrice.String())
	}
	oid := o.OrderID.Int64()
	if oid == 0 {
		oid = o.OrderIDAlt.Int64()
	}
	return order.Order{
		ClientOrderID: parseClOrdID(firstNonEmpty(o.ClOrdID.String(), o.ClOrdIDAlt.String())),
		ExchangeID:    strconv.FormatInt(oid, 10),
		Symbol:        symbol,
		Side:          sideFromString(o.Side.String()),
		Type:          typeFromString(o.OrderType.String()),
		Price:         parseDec(o.Price.String()),
		Quantity:      total,
		FilledQty:     filled,
		AvgFillPrice:  avg,
		Fee:           parseDec(o.Fee.String()),
		IsMaker:       bool(o.IsMaker),
		State:         state,
		RejectReason:  reason,
		UpdatedAt:     unixMilli(o.TradeTime.Int64()),
	}
}

func toTradeFromWS(t wsAccountTrade, symbol string) order.Trade {
	if symbol == "" {
		symbol = t.Symbol
	}
	return order.Trade{
		ID:            t.TradeID.Int64(),
		ClientOrderID: parseClOrdID(firstNonEmpty(t.ClOrdID.String(), t.ClOrdIDAlt.String())),
		Symbol:        symbol,
		Side:          sideFromString(t.Side.String()),
		Price:         parseDec(t.Price.String()),
		Quantity:      parseDec(t.Quantity.String()),
		Fee:           parseDec(t.Fee.String()),
		IsMaker:       bool(t.IsMaker),
		Time:          unixMilli(t.TradeTime.Int64()),
	}
}

func toPosition(p client.Position, mark decimal.Decimal) position.Position {
	size := parseDec(p.Size)
	switch strings.ToUpper(p.PositionSide) {
	case "SHORT", "3":
		size = size.Abs().Neg()
	case "LONG", "2":
		size = size.Abs()
	}
	entry := parseDec(p.AvgEntryPrice)
	upnl := decimal.Zero
	if size.IsPositive() {
		upnl = mark.Sub(entry).Mul(size)
	} else if size.IsNegative() {
		upnl = entry.Sub(mark).Mul(size.Abs())
	}
	return position.Position{
		Symbol:        p.Symbol,
		Size:          size,
		EntryPrice:    entry,
		MarkPrice:     mark,
		UnrealizedPnL: upnl,
		Leverage:      p.Leverage,
		MarginMode:    marginModeFromString(p.MarginMode),
		UpdatedAt:     unixMilli(int64(p.UpdatedAt)),
	}
}

func unixMilli(ms int64) time.Time {
	if ms <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms)
}

func klineInterval(interval string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(interval)) {
	case "1m", "3m", "5m", "15m", "30m", "1h", "2h", "4h", "6h", "8h", "12h":
		return strings.ToLower(interval), true
	case "1d", "1D":
		return "1D", true
	case "3d", "3D":
		return "3D", true
	case "1w", "1W":
		return "1W", true
	case "1M", "1mo":
		return "1M", true
	default:
		return "", false
	}
}
