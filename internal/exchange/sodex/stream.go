package sodex

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"dex-grid/internal/domain/market"
	"dex-grid/internal/domain/position"
	"dex-grid/internal/exchange"

	"github.com/coder/websocket"
	"github.com/shopspring/decimal"
)

const (
	keepaliveInterval = 20 * time.Second
	readTimeout       = 75 * time.Second
	eventBuffer       = 256
	maxMessageSize    = 4 << 20
)

var _ exchange.Streamer = (*Adapter)(nil)

func (a *Adapter) Subscribe(ctx context.Context, symbol string) (<-chan exchange.StreamEvent, error) {
	return a.subscribe(ctx, symbol, nil)
}

// SubscribeRaw 与 Subscribe 相同，但额外把每一帧原始 JSON 交给 onFrame。
func (a *Adapter) SubscribeRaw(ctx context.Context, symbol string, onFrame func([]byte)) (<-chan exchange.StreamEvent, error) {
	return a.subscribe(ctx, symbol, onFrame)
}

func (a *Adapter) subscribe(ctx context.Context, symbol string, onFrame func([]byte)) (<-chan exchange.StreamEvent, error) {
	cm, err := a.lookup(ctx, symbol)
	if err != nil {
		return nil, err
	}
	s := &stream{
		adapter: a,
		log:     a.log.With("symbol", cm.symbol.Symbol),
		symbol:  cm.symbol.Symbol,
		out:     make(chan exchange.StreamEvent, eventBuffer),
		onFrame: onFrame,
	}
	go s.run(ctx)
	return s.out, nil
}

type stream struct {
	adapter *Adapter
	log     *slog.Logger
	symbol  string
	out     chan exchange.StreamEvent
	onFrame func([]byte)

	latest exchange.Ticker
}

func (s *stream) run(ctx context.Context) {
	defer close(s.out)

	backoff := s.adapter.reconnectInitial
	if backoff <= 0 {
		backoff = time.Second
	}
	maxBackoff := s.adapter.reconnectMax
	if maxBackoff < backoff {
		maxBackoff = 30 * time.Second
	}

	for {
		err := s.session(ctx)
		if ctx.Err() != nil {
			return
		}
		if err != nil && !errors.Is(err, context.Canceled) {
			s.log.Warn("事件流断开，准备重连", "err", err, "退避", backoff)
			s.emit(ctx, exchange.StreamEvent{Err: err, Time: time.Now()})
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff *= 2
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

func (s *stream) session(ctx context.Context) error {
	conn, _, err := websocket.Dial(ctx, s.adapter.wsURL, &websocket.DialOptions{
		HTTPClient: s.adapter.httpClient(),
	})
	if err != nil {
		return fmt.Errorf("连接 %s 失败: %w", s.adapter.wsURL, err)
	}
	conn.SetReadLimit(maxMessageSize)
	defer conn.CloseNow()

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	if err := s.subscribe(ctx, conn); err != nil {
		return err
	}

	s.emit(ctx, exchange.StreamEvent{Resync: true, Time: time.Now()})
	s.log.Info("事件流已连接", "symbol", s.symbol)

	go s.keepalive(ctx, conn)

	for {
		readCtx, cancelRead := context.WithTimeout(ctx, readTimeout)
		_, data, err := conn.Read(readCtx)
		cancelRead()
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("读取消息失败: %w", err)
		}
		if s.onFrame != nil {
			s.onFrame(data)
		}
		if err := s.handle(ctx, data); err != nil {
			return err
		}
	}
}

func (s *stream) subscribe(ctx context.Context, conn *websocket.Conn) error {
	subs := []map[string]any{
		{"op": "subscribe", "params": map[string]any{
			"channel": channelBookTicker,
			"symbol":  s.symbol,
			"symbols": []string{s.symbol},
		}},
		{"op": "subscribe", "params": map[string]any{
			"channel": channelMarkPrice,
			"symbol":  s.symbol,
			"symbols": []string{s.symbol},
		}},
		accountSub(s.adapter.address, s.adapter.accountID, channelAccountOrderUpd),
		accountSub(s.adapter.address, s.adapter.accountID, channelAccountTrade),
		accountSub(s.adapter.address, s.adapter.accountID, channelAccountUpdate),
	}
	for _, sub := range subs {
		payload, err := json.Marshal(sub)
		if err != nil {
			return err
		}
		if err := conn.Write(ctx, websocket.MessageText, payload); err != nil {
			return fmt.Errorf("发送订阅失败: %w", err)
		}
	}
	return nil
}

func accountSub(user string, accountID uint64, channel string) map[string]any {
	params := map[string]any{"channel": channel, "user": user}
	if accountID != 0 {
		params["accountID"] = accountID
	}
	return map[string]any{"op": "subscribe", "params": params}
}

func (s *stream) keepalive(ctx context.Context, conn *websocket.Conn) {
	t := time.NewTicker(keepaliveInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := conn.Write(ctx, websocket.MessageText, []byte(`{"op":"ping"}`)); err != nil {
				return
			}
		}
	}
}

func (s *stream) emit(ctx context.Context, ev exchange.StreamEvent) {
	if ev.Time.IsZero() {
		ev.Time = time.Now()
	}
	select {
	case s.out <- ev:
	case <-ctx.Done():
	}
}

type wsEnvelope struct {
	Op      string          `json:"op"`
	Channel string          `json:"channel"`
	Type    string          `json:"type"`
	Success *bool           `json:"success"`
	Error   string          `json:"error"`
	Code    any             `json:"code"`
	Data    json.RawMessage `json:"data"`
}

func (s *stream) handle(ctx context.Context, data []byte) error {
	var env wsEnvelope
	if err := json.Unmarshal(data, &env); err != nil {
		return fmt.Errorf("无法解析消息外层: %w", err)
	}

	switch {
	case env.Op == "pong" || env.Op == "ping" || env.Type == "pong":
		return nil
	case env.Error != "" || (env.Success != nil && !*env.Success):
		return fmt.Errorf("sodex 事件流返回错误: %s", firstNonEmpty(env.Error, string(data)))
	}

	ch := env.Channel
	switch {
	case channelIs(ch, channelBookTicker):
		return s.handleBookTicker(ctx, env.Data)
	case channelIs(ch, channelMarkPrice):
		return s.handleMarkPrice(ctx, env.Data)
	case channelIs(ch, channelAccountOrderUpd):
		return s.handleOrderUpdate(ctx, env.Data)
	case channelIs(ch, channelAccountTrade):
		return s.handleTrade(ctx, env.Data)
	case channelIs(ch, channelAccountUpdate):
		return s.handleAccountUpdate(ctx, env.Data)
	default:
		return nil
	}
}

func channelIs(got, want string) bool {
	got = strings.ToLower(got)
	want = strings.ToLower(want)
	return got == want || strings.HasPrefix(got, want+"@") || strings.Contains(got, want)
}

func unmarshalOneOrMany[T any](data json.RawMessage) ([]T, error) {
	data = bytes.TrimSpace(data)
	if len(data) == 0 || string(data) == "null" {
		return nil, nil
	}
	if data[0] == '[' {
		var xs []T
		if err := json.Unmarshal(data, &xs); err != nil {
			return nil, err
		}
		return xs, nil
	}
	var x T
	if err := json.Unmarshal(data, &x); err != nil {
		return nil, err
	}
	return []T{x}, nil
}

func (s *stream) handleBookTicker(ctx context.Context, data json.RawMessage) error {
	items, err := unmarshalOneOrMany[wsBookTicker](data)
	if err != nil {
		return fmt.Errorf("无法解析 bookTicker: %w", err)
	}
	for _, t := range items {
		if t.Symbol != "" && !strings.EqualFold(t.Symbol, s.symbol) {
			continue
		}
		bid := parseDec(t.BidPrice)
		ask := parseDec(t.AskPrice)
		if !bid.IsPositive() && !ask.IsPositive() {
			continue
		}
		ts := time.Now()
		if t.EventTime > 0 {
			ts = time.UnixMilli(t.EventTime)
		}
		s.latest.Symbol = s.symbol
		s.latest.Book = market.BookTicker{
			Bid:     bid,
			Ask:     ask,
			BidSize: parseDec(t.BidQty),
			AskSize: parseDec(t.AskQty),
			Time:    ts,
		}
		s.latest.Last = parseDec(t.BidPrice) // 占位，真正 last 来自 mark/ticker
		s.emitTicker(ctx, ts)
	}
	return nil
}

func (s *stream) handleMarkPrice(ctx context.Context, data json.RawMessage) error {
	items, err := unmarshalOneOrMany[wsMarkPrice](data)
	if err != nil {
		return fmt.Errorf("无法解析 markPrice: %w", err)
	}
	for _, m := range items {
		if m.Symbol != "" && !strings.EqualFold(m.Symbol, s.symbol) {
			continue
		}
		ts := time.Now()
		if m.EventTime > 0 {
			ts = time.UnixMilli(m.EventTime)
		}
		s.latest.Symbol = s.symbol
		s.latest.Mark = parseDec(m.MarkPx)
		s.latest.Index = parseDec(m.IndexPx)
		if !s.latest.Book.Valid() {
			continue
		}
		s.emitTicker(ctx, ts)
	}
	return nil
}

func (s *stream) emitTicker(ctx context.Context, ts time.Time) {
	snapshot := s.latest
	snapshot.Time = ts
	if !snapshot.Mark.IsPositive() {
		snapshot.Mark = snapshot.Book.Mid()
	}
	s.emit(ctx, exchange.StreamEvent{Ticker: &snapshot, Time: ts})
}

func peelObjectArray(data json.RawMessage, keys ...string) json.RawMessage {
	data = bytes.TrimSpace(data)
	if len(data) == 0 || data[0] != '{' {
		return data
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(data, &obj) != nil {
		return data
	}
	for _, k := range keys {
		raw, ok := obj[k]
		if !ok {
			continue
		}
		raw = bytes.TrimSpace(raw)
		if len(raw) > 0 && (raw[0] == '{' || raw[0] == '[') {
			return raw
		}
	}
	return data
}

func (s *stream) handleOrderUpdate(ctx context.Context, data json.RawMessage) error {
	items, err := unmarshalOneOrMany[wsAccountOrderUpdate](peelObjectArray(data, "o", "order", "orders"))
	if err != nil {
		return fmt.Errorf("无法解析 accountOrderUpdate: %w", err)
	}
	for i := range items {
		o := items[i]
		if o.Symbol != "" && !strings.EqualFold(o.Symbol, s.symbol) {
			continue
		}
		ord := toOrderFromWS(o, s.symbol)
		s.emit(ctx, exchange.StreamEvent{Order: &ord})
	}
	return nil
}

func (s *stream) handleTrade(ctx context.Context, data json.RawMessage) error {
	items, err := unmarshalOneOrMany[wsAccountTrade](peelObjectArray(data, "t", "trade", "trades"))
	if err != nil {
		return fmt.Errorf("无法解析 accountTrade: %w", err)
	}
	for i := range items {
		t := items[i]
		if t.Symbol != "" && !strings.EqualFold(t.Symbol, s.symbol) {
			continue
		}
		tr := toTradeFromWS(t, s.symbol)
		s.emit(ctx, exchange.StreamEvent{Trade: &tr, Time: tr.Time})
	}
	return nil
}

type wsAccountPos struct {
	Symbol        string `json:"symbol"`
	S             string `json:"s"`
	Size          string `json:"size"`
	Sz            string `json:"sz"`
	PositionSide  string `json:"positionSide"`
	Ps            string `json:"ps"`
	AvgEntryPrice string `json:"avgEntryPrice"`
	Entry         string `json:"entryPrice"`
	UnrealizedPnL string `json:"unrealizedPnl"`
	Upnl          string `json:"upnl"`
	Leverage      int    `json:"leverage"`
	MarginMode    string `json:"marginMode"`
	LiquidationPx string `json:"liquidationPrice"`
}

func (s *stream) handleAccountUpdate(ctx context.Context, data json.RawMessage) error {
	data = bytes.TrimSpace(data)
	if len(data) == 0 || string(data) == "null" {
		return nil
	}

	var wrapper struct {
		Positions json.RawMessage `json:"positions"`
		P         json.RawMessage `json:"P"`
		B         json.RawMessage `json:"B"`
		Balances  json.RawMessage `json:"balances"`
	}
	_ = json.Unmarshal(data, &wrapper)
	rawPos := wrapper.Positions
	if len(rawPos) == 0 {
		rawPos = wrapper.P
	}
	if len(rawPos) == 0 {
		// 整段就是仓位对象/数组
		rawPos = data
	}
	items, err := unmarshalOneOrMany[wsAccountPos](rawPos)
	if err != nil {
		return nil
	}
	for _, p := range items {
		sym := firstNonEmpty(p.Symbol, p.S)
		if sym != "" && !strings.EqualFold(sym, s.symbol) {
			continue
		}
		if sym == "" {
			sym = s.symbol
		}
		size := parseDec(firstNonEmpty(p.Size, p.Sz))
		side := firstNonEmpty(p.PositionSide, p.Ps)
		if strings.EqualFold(side, "SHORT") || side == "3" {
			size = size.Abs().Neg()
		} else if size.IsPositive() || strings.EqualFold(side, "LONG") {
			size = size.Abs()
		}
		if size.IsZero() && firstNonEmpty(p.Size, p.Sz) == "" {
			continue
		}
		mark := s.latest.Mark
		entry := parseDec(firstNonEmpty(p.AvgEntryPrice, p.Entry))
		upnl := parseDec(firstNonEmpty(p.UnrealizedPnL, p.Upnl))
		pos := toPositionFromFields(sym, size, entry, mark, upnl, p.Leverage, p.MarginMode, p.LiquidationPx)
		s.emit(ctx, exchange.StreamEvent{Position: &pos})
	}
	return nil
}

func toPositionFromFields(symbol string, size, entry, mark, upnl decimal.Decimal, lev int, mode, liq string) position.Position {
	if !upnl.IsZero() {
		// 已由交易所给出
	} else if size.IsPositive() {
		upnl = mark.Sub(entry).Mul(size)
	} else if size.IsNegative() {
		upnl = entry.Sub(mark).Mul(size.Abs())
	}
	return position.Position{
		Symbol:           symbol,
		Size:             size,
		EntryPrice:       entry,
		MarkPrice:        mark,
		UnrealizedPnL:    upnl,
		LiquidationPrice: parseDec(liq),
		Leverage:         lev,
		MarginMode:       marginModeFromString(mode),
	}
}
