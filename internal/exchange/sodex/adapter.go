// Package sodex 是 SODEx（Bolt 永续引擎）的交易所适配器。
//
// 只做永续合约：现货 Spark 引擎不接入。交易走官方 Go SDK 的 EIP-712 签名
// REST（domain name = "futures"），行情与账户走公开 REST，订单/仓位回报走
// WebSocket。签名与 nonce 由 github.com/sodex-tech/sodex-go-sdk-public 处理。
//
// 不在 init 里注册，由 main 在 lighter、rh_lighter 之后追加，slot 固定为 2。
package sodex

import (
	"context"
	"crypto/ecdsa"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"dex-grid/internal/config"
	"dex-grid/internal/domain/account"
	"dex-grid/internal/domain/market"
	"dex-grid/internal/domain/order"
	"dex-grid/internal/domain/position"
	"dex-grid/internal/exchange"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/shopspring/decimal"
	"github.com/sodex-tech/sodex-go-sdk-public/client"
	"github.com/sodex-tech/sodex-go-sdk-public/common/enums"
	ctypes "github.com/sodex-tech/sodex-go-sdk-public/common/types"
	ptypes "github.com/sodex-tech/sodex-go-sdk-public/perps/types"
)

// Name 是交易所在注册表中的名称。
const Name = "sodex"

const marketCacheTTL = 10 * time.Minute

// 单批上限：官方 batch weight = 1 + floor(N/40)，20 笔仍是 1 个 weight。
const defaultBatch = 20

// Options 是 config.yaml 中 exchanges[].options 的内容。
type Options struct {
	ChainID     uint64 `yaml:"chain_id"`
	APIKeyName  string `yaml:"api_key_name"`
	UserAddress string `yaml:"user_address"`
}

type cachedMarket struct {
	symbol client.Symbol
	model  market.Market
}

// Adapter 实现 exchange.Exchange 与 exchange.Streamer。
type Adapter struct {
	log       *slog.Logger
	rest      *rest
	accountID uint64
	address   string

	wsURL            string
	http             *http.Client
	reconnectInitial time.Duration
	reconnectMax     time.Duration

	mu       sync.RWMutex
	bySymbol map[string]cachedMarket
	byIndex  map[int]cachedMarket
	tickers  map[string]client.Ticker
	loadedAt time.Time
}

var _ exchange.Exchange = (*Adapter)(nil)

// New 从配置构造适配器。不在 init 里注册，由 main 在 rh_lighter 之后追加。
func New(cfg config.Exchange, deps exchange.Deps) (exchange.Exchange, error) {
	var opts Options
	if err := cfg.DecodeOptions(&opts); err != nil {
		return nil, err
	}

	baseURL, wsURL := cfg.BaseURL, cfg.WSURL
	chainID := opts.ChainID
	if baseURL == "" {
		baseURL = defaultREST(cfg.Network)
	}
	if wsURL == "" {
		wsURL = defaultWS(cfg.Network)
	}
	if chainID == 0 {
		chainID = defaultChainID(cfg.Network)
	}

	pk, err := parsePrivateKey(cfg.Credentials.APIKeyPrivateKey)
	if err != nil {
		return nil, err
	}
	apiKeyName := strings.TrimSpace(opts.APIKeyName)
	if apiKeyName == "" {
		apiKeyName = strings.TrimSpace(cfg.Credentials.APIKeyName)
	}
	if apiKeyName == "" {
		return nil, fmt.Errorf("sodex: api_key_name 必填（写在 credentials 或 options；X-API-Key 是密钥的 name，不是私钥）")
	}
	addr := normalizeAddress(opts.UserAddress)
	if addr == "" {
		addr = normalizeAddress(cfg.Credentials.AccountAddress)
	}
	if addr == "" {
		return nil, fmt.Errorf("sodex: user_address / account_address 必填（主钱包地址，账户查询与 WS 用户频道用它，不是 API key 地址）")
	}

	httpc := deps.HTTP
	if httpc == nil {
		httpc = http.DefaultClient
	}
	cli := client.New(client.Config{
		BaseURL:    strings.TrimRight(baseURL, "/"),
		ChainID:    chainID,
		PrivateKey: pk,
		APIKeyName: apiKeyName,
		HTTPClient: httpc,
	})
	rest := newREST(cli, cfg.RateLimit.RPS, cfg.RateLimit.Burst, cfg.MaxRetries)

	accountID := uint64(cfg.Credentials.AccountIDOrIndex())
	if accountID == 0 {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		info, err := rest.accountInfo(ctx, addr)
		if err != nil {
			return nil, fmt.Errorf("sodex: account_index 为空且无法按地址查询 accountID: %w", err)
		}
		accountID = info.AccountID
	}

	log := deps.Log
	if log == nil {
		log = slog.Default()
	}
	return &Adapter{
		log:              log.With("exchange", Name),
		rest:             rest,
		accountID:        accountID,
		address:          addr,
		wsURL:            wsURL,
		http:             httpc,
		reconnectInitial: cfg.Reconnect.Initial.Std(),
		reconnectMax:     cfg.Reconnect.Max.Std(),
		bySymbol:         map[string]cachedMarket{},
		byIndex:          map[int]cachedMarket{},
		tickers:          map[string]client.Ticker{},
	}, nil
}

func parsePrivateKey(hexKey string) (*ecdsa.PrivateKey, error) {
	s := strings.TrimSpace(hexKey)
	s = strings.TrimPrefix(s, "0x")
	s = strings.TrimPrefix(s, "0X")
	if s == "" {
		return nil, fmt.Errorf("sodex: credentials.api_key_private_key 为空")
	}
	pk, err := crypto.HexToECDSA(s)
	if err != nil {
		return nil, fmt.Errorf("sodex: 无法解析 API key 私钥: %w", err)
	}
	return pk, nil
}

func normalizeAddress(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if !strings.HasPrefix(s, "0x") && !strings.HasPrefix(s, "0X") {
		s = "0x" + s
	}
	if !common.IsHexAddress(s) {
		return s
	}
	return common.HexToAddress(s).Hex()
}

func (a *Adapter) httpClient() *http.Client {
	if a.http == nil {
		return nil
	}
	c := *a.http
	c.Timeout = 0
	return &c
}

func (a *Adapter) Name() string { return Name }

func (a *Adapter) Capabilities() exchange.Capabilities {
	return exchange.Capabilities{
		BatchPlace:    defaultBatch,
		BatchCancel:   defaultBatch,
		ModifyOrder:   true,
		PostOnly:      true,
		ReduceOnly:    true,
		NativeTPSL:    false,
		MaxOpenOrders: 0,
	}
}

func (a *Adapter) Close() error { return nil }

func (a *Adapter) refreshMarkets(ctx context.Context) error {
	symbols, err := a.rest.symbols(ctx)
	if err != nil {
		return err
	}
	ticks, err := a.rest.tickers(ctx)
	if err != nil {
		return err
	}
	tickers := make(map[string]client.Ticker, len(ticks))
	for _, t := range ticks {
		tickers[strings.ToUpper(t.Symbol)] = t
	}

	bySymbol := make(map[string]cachedMarket, len(symbols))
	byIndex := make(map[int]cachedMarket, len(symbols))
	for _, s := range symbols {
		cm := cachedMarket{symbol: s, model: toMarket(s)}
		bySymbol[strings.ToUpper(s.Symbol)] = cm
		byIndex[int(s.SymbolID)] = cm
	}

	a.mu.Lock()
	a.bySymbol, a.byIndex, a.tickers, a.loadedAt = bySymbol, byIndex, tickers, time.Now()
	a.mu.Unlock()

	a.log.Debug("市场元数据已刷新", "perp", len(byIndex))
	return nil
}

func (a *Adapter) ensureMarkets(ctx context.Context) error {
	a.mu.RLock()
	fresh := time.Since(a.loadedAt) < marketCacheTTL && len(a.bySymbol) > 0
	a.mu.RUnlock()
	if fresh {
		return nil
	}
	return a.refreshMarkets(ctx)
}

func (a *Adapter) lookup(ctx context.Context, symbol string) (cachedMarket, error) {
	if err := a.ensureMarkets(ctx); err != nil {
		return cachedMarket{}, err
	}
	a.mu.RLock()
	cm, ok := a.bySymbol[strings.ToUpper(symbol)]
	a.mu.RUnlock()
	if !ok {
		return cachedMarket{}, exchange.Classify(exchange.ClassInvalidParam, "lookup_market",
			fmt.Errorf("sodex: 未找到永续合约交易对 %q（本系统只做合约网格，不支持现货）", symbol))
	}
	return cm, nil
}

func (a *Adapter) tradable(ctx context.Context, symbol string) (cachedMarket, error) {
	cm, err := a.lookup(ctx, symbol)
	if err != nil {
		return cm, err
	}
	if !isActiveStatus(cm.symbol.Status) {
		return cm, exchange.Classify(exchange.ClassInvalidParam, "lookup_market",
			fmt.Errorf("sodex: 合约 %s 当前状态为 %q，不可交易", cm.symbol.Symbol, cm.symbol.Status))
	}
	return cm, nil
}

// LookupByIndex 按 symbolID 查找永续合约市场。
func (a *Adapter) LookupByIndex(ctx context.Context, index int) (market.Market, error) {
	if err := a.ensureMarkets(ctx); err != nil {
		return market.Market{}, err
	}
	a.mu.RLock()
	cm, ok := a.byIndex[index]
	a.mu.RUnlock()
	if !ok {
		return market.Market{}, fmt.Errorf("sodex: 未找到 symbolID %d 对应的永续合约", index)
	}
	return cm.model, nil
}

func (a *Adapter) tickerOf(symbol string) client.Ticker {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.tickers[strings.ToUpper(symbol)]
}

// freshMarks 拉取最新 ticker 并提取各交易对标记价（REST 持仓不含 unrealizedPnl，需用 mark 自算）。
func (a *Adapter) freshMarks(ctx context.Context) (map[string]decimal.Decimal, error) {
	ticks, err := a.rest.tickers(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[string]decimal.Decimal, len(ticks))
	for _, t := range ticks {
		sym := strings.ToUpper(t.Symbol)
		mark := parseDec(ptrStr(t.MarkPrice))
		if !mark.IsPositive() {
			mark = parseDec(t.LastPrice)
		}
		if mark.IsPositive() {
			out[sym] = mark
		}
	}
	return out, nil
}

func (a *Adapter) freshMark(ctx context.Context, symbol string) decimal.Decimal {
	tick, err := a.Ticker(ctx, symbol)
	if err != nil {
		return decimal.Zero
	}
	if tick.Mark.IsPositive() {
		return tick.Mark
	}
	if tick.Book.Valid() {
		return tick.Book.Mid()
	}
	return decimal.Zero
}

func (a *Adapter) Markets(ctx context.Context) ([]exchange.MarketInfo, error) {
	if err := a.ensureMarkets(ctx); err != nil {
		return nil, err
	}
	a.mu.RLock()
	out := make([]exchange.MarketInfo, 0, len(a.byIndex))
	for _, cm := range a.byIndex {
		if !isActiveStatus(cm.symbol.Status) {
			continue
		}
		out = append(out, toMarketInfo(cm.symbol, a.tickers[strings.ToUpper(cm.symbol.Symbol)]))
	}
	a.mu.RUnlock()
	sortByVolume(out)
	return out, nil
}

func sortByVolume(ms []exchange.MarketInfo) {
	sort.Slice(ms, func(i, j int) bool {
		if !ms[i].DailyQuoteVolume.Equal(ms[j].DailyQuoteVolume) {
			return ms[i].DailyQuoteVolume.GreaterThan(ms[j].DailyQuoteVolume)
		}
		return ms[i].Symbol < ms[j].Symbol
	})
}

func (a *Adapter) Market(ctx context.Context, symbol string) (market.Market, error) {
	cm, err := a.lookup(ctx, symbol)
	if err != nil {
		return market.Market{}, err
	}
	return cm.model, nil
}

func (a *Adapter) Ticker(ctx context.Context, symbol string) (exchange.Ticker, error) {
	cm, err := a.lookup(ctx, symbol)
	if err != nil {
		return exchange.Ticker{}, err
	}
	ticks, err := a.rest.tickers(ctx)
	if err != nil {
		return exchange.Ticker{}, err
	}
	var t client.Ticker
	found := false
	for _, x := range ticks {
		if strings.EqualFold(x.Symbol, cm.symbol.Symbol) {
			t = x
			found = true
			break
		}
	}
	if !found {
		return exchange.Ticker{}, fmt.Errorf("sodex: %s 没有返回行情", symbol)
	}

	out := exchange.Ticker{
		Symbol: cm.symbol.Symbol,
		Mark:   parseDec(ptrStr(t.MarkPrice)),
		Index:  parseDec(ptrStr(t.IndexPrice)),
		Last:   parseDec(t.LastPrice),
		Book: market.BookTicker{
			Bid:     parseDec(t.BidPrice),
			Ask:     parseDec(t.AskPrice),
			BidSize: parseDec(t.BidSize),
			AskSize: parseDec(t.AskSize),
			Time:    time.Now(),
		},
		Time: time.Now(),
	}
	if !out.Book.Valid() {
		book, err := a.rest.orderBook(ctx, cm.symbol.Symbol, 1)
		if err == nil && book != nil {
			if len(book.Bids) > 0 {
				out.Book.Bid = parseDec(book.Bids[0].Price)
				out.Book.BidSize = parseDec(book.Bids[0].Quantity)
			}
			if len(book.Asks) > 0 {
				out.Book.Ask = parseDec(book.Asks[0].Price)
				out.Book.AskSize = parseDec(book.Asks[0].Quantity)
			}
			out.Book.Time = time.Now()
		}
	}
	if !out.Mark.IsPositive() {
		out.Mark = out.Book.Mid()
	}
	return out, nil
}

func (a *Adapter) Klines(ctx context.Context, symbol, interval string, limit int) ([]market.Kline, error) {
	cm, err := a.lookup(ctx, symbol)
	if err != nil {
		return nil, err
	}
	res, ok := klineInterval(interval)
	if !ok {
		return nil, fmt.Errorf("sodex: 不支持的 K 线周期 %q", interval)
	}
	if limit <= 0 {
		limit = 72
	}
	if limit > 1500 {
		limit = 1500
	}
	raw, err := a.rest.klines(ctx, cm.symbol.Symbol, res, limit)
	if err != nil {
		return nil, err
	}
	out := make([]market.Kline, 0, len(raw))
	for _, c := range raw {
		cl := parseDec(c.Close)
		if c.StartTime == 0 || !cl.IsPositive() {
			continue
		}
		out = append(out, market.Kline{
			OpenTime: time.UnixMilli(int64(c.StartTime)),
			Open:     parseDec(c.Open),
			High:     parseDec(c.High),
			Low:      parseDec(c.Low),
			Close:    cl,
			Volume:   parseDec(c.BaseVolume),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].OpenTime.Before(out[j].OpenTime) })
	return out, nil
}

func (a *Adapter) Account(ctx context.Context) (account.Snapshot, error) {
	// 隔离保证金不锁在余额的 locked 字段里；浮盈要用标记价，缓存未加载时 mark=0
	// 会把「均价×仓位」当成浮亏。
	_ = a.ensureMarkets(ctx)

	bals, err := a.rest.balances(ctx, a.address)
	if err != nil {
		return account.Snapshot{}, err
	}
	total, locked := decimal.Zero, decimal.Zero
	for _, b := range bals {
		coin := strings.ToUpper(b.Coin)
		if coin != "USDC" && coin != "VUSDC" && coin != "USD" {
			continue
		}
		total = total.Add(parseDec(b.Total))
		locked = locked.Add(parseDec(b.Locked))
	}
	if total.IsZero() && locked.IsZero() {
		for _, b := range bals {
			total = total.Add(parseDec(b.Total))
			locked = locked.Add(parseDec(b.Locked))
		}
	}
	upnl, im := decimal.Zero, decimal.Zero
	positions, err := a.rest.positions(ctx, a.address)
	if err == nil {
		marks, _ := a.freshMarks(ctx)
		for _, p := range positions {
			mark := marks[strings.ToUpper(p.Symbol)]
			pos := toPosition(p, mark)
			if pos.IsFlat() {
				continue
			}
			if mark.IsPositive() {
				upnl = upnl.Add(pos.UnrealizedPnL)
			}
			im = im.Add(parseDec(p.InitialMargin))
		}
	}
	marginUsed := locked.Add(im)
	avail := total.Sub(marginUsed)
	if avail.IsNegative() {
		avail = decimal.Zero
	}
	return account.Snapshot{
		Balance:       total,
		Equity:        total.Add(upnl),
		Available:     avail,
		MarginUsed:    marginUsed,
		UnrealizedPnL: upnl,
		UpdatedAt:     time.Now(),
	}, nil
}

func (a *Adapter) Position(ctx context.Context, symbol string) (position.Position, error) {
	cm, err := a.lookup(ctx, symbol)
	if err != nil {
		return position.Position{}, err
	}
	ps, err := a.rest.positions(ctx, a.address)
	if err != nil {
		return position.Position{}, err
	}
	mark := a.freshMark(ctx, cm.symbol.Symbol)
	for _, p := range ps {
		if strings.EqualFold(p.Symbol, cm.symbol.Symbol) {
			return toPosition(p, mark), nil
		}
	}
	return position.Position{Symbol: cm.symbol.Symbol, MarkPrice: mark}, nil
}

// Positions 返回全部非空持仓。
func (a *Adapter) Positions(ctx context.Context) ([]position.Position, error) {
	if err := a.ensureMarkets(ctx); err != nil {
		return nil, err
	}
	ps, err := a.rest.positions(ctx, a.address)
	if err != nil {
		return nil, err
	}
	marks, _ := a.freshMarks(ctx)
	var out []position.Position
	for _, p := range ps {
		mark := marks[strings.ToUpper(p.Symbol)]
		pos := toPosition(p, mark)
		if pos.IsFlat() {
			continue
		}
		out = append(out, pos)
	}
	return out, nil
}

func (a *Adapter) OpenOrders(ctx context.Context, symbol string) ([]order.Order, error) {
	cm, err := a.lookup(ctx, symbol)
	if err != nil {
		return nil, err
	}
	raw, err := a.rest.orders(ctx, a.address)
	if err != nil {
		return nil, err
	}
	out := make([]order.Order, 0, len(raw))
	for _, o := range raw {
		if !strings.EqualFold(o.Symbol, cm.symbol.Symbol) {
			continue
		}
		out = append(out, toOrderFromREST(o, cm.symbol.Symbol))
	}
	return out, nil
}

func (a *Adapter) SetLeverage(ctx context.Context, symbol string, leverage int, mode market.MarginMode) error {
	cm, err := a.tradable(ctx, symbol)
	if err != nil {
		return err
	}
	if leverage < 1 || leverage > cm.model.MaxLeverage {
		return exchange.Classify(exchange.ClassInvalidParam, "set_leverage",
			fmt.Errorf("sodex: %s 的杠杆必须在 1 - %d 之间，收到 %d",
				symbol, cm.model.MaxLeverage, leverage))
	}
	_, err = a.rest.leverage(ctx, &ptypes.UpdateLeverageRequest{
		AccountID:  a.accountID,
		SymbolID:   cm.symbol.SymbolID,
		Leverage:   uint32(leverage),
		MarginMode: toMarginMode(mode),
	})
	return err
}

func (a *Adapter) PlaceOrders(ctx context.Context, reqs []exchange.PlaceRequest) ([]exchange.PlaceResult, error) {
	results := make([]exchange.PlaceResult, len(reqs))
	type group struct {
		symbolID uint64
		idxs     []int
		orders   []*ptypes.RawOrder
	}
	var groups []group
	bySym := map[uint64]int{}

	for i, req := range reqs {
		results[i] = exchange.PlaceResult{ClientOrderID: req.ClientOrderID}
		raw, cm, err := a.buildRaw(ctx, req)
		if err != nil {
			results[i].Err = err
			continue
		}
		gidx, ok := bySym[cm.symbol.SymbolID]
		if !ok {
			gidx = len(groups)
			bySym[cm.symbol.SymbolID] = gidx
			groups = append(groups, group{symbolID: cm.symbol.SymbolID})
		}
		groups[gidx].idxs = append(groups[gidx].idxs, i)
		groups[gidx].orders = append(groups[gidx].orders, raw)
	}

	for _, g := range groups {
		if len(g.orders) == 0 {
			continue
		}
		placed, err := a.rest.place(ctx, &ptypes.NewOrderRequest{
			AccountID: a.accountID,
			SymbolID:  g.symbolID,
			Orders:    g.orders,
		})
		if err != nil {
			for _, i := range g.idxs {
				if results[i].Err == nil {
					results[i].Err = err
				}
			}
			continue
		}
		byCl := map[string]client.PlaceOrderResult{}
		for _, p := range placed {
			byCl[p.ClOrdID] = p
		}
		for j, i := range g.idxs {
			cl := g.orders[j].ClOrdID
			p, ok := byCl[cl]
			if !ok {
				if j < len(placed) {
					p = placed[j]
				} else {
					results[i].Err = classify("place_order", fmt.Errorf("sodex: 下单响应缺少 clOrdID %s", cl))
					continue
				}
			}
			if placeRejected(p, g.orders[j].TimeInForce == enums.TimeInForceIOC) {
				msg := p.Message
				if msg == "" {
					msg = p.Status
				}
				results[i].Err = classify("place_order", fmt.Errorf("sodex: %s", msg))
				continue
			}
			if p.OrderID != 0 {
				id := strconv.FormatUint(p.OrderID, 10)
				results[i].ExchangeID = id
				results[i].TxHash = id
			}
		}
	}
	return results, nil
}

func placeRejected(p client.PlaceOrderResult, iocExpireOK bool) bool {
	st := strings.ToUpper(strings.TrimSpace(p.Status))
	if st == "REJECTED" {
		return true
	}
	if st == "EXPIRED" {
		// 市价/IOC 剩余过期是正常结束；GTX 过期表示会吃单，按拒单处理。
		return !iocExpireOK
	}
	return p.Message != "" && (st == "" || st == "UNKNOWN")
}

func (a *Adapter) buildRaw(ctx context.Context, req exchange.PlaceRequest) (*ptypes.RawOrder, cachedMarket, error) {
	lookup := a.tradable
	if req.ReduceOnly {
		lookup = a.lookup
	}
	cm, err := lookup(ctx, req.Symbol)
	if err != nil {
		return nil, cm, err
	}
	m := cm.model
	invalid := func(err error) (*ptypes.RawOrder, cachedMarket, error) {
		return nil, cm, exchange.Classify(exchange.ClassInvalidParam, "place_order", err)
	}
	if !req.ClientOrderID.Valid() {
		return invalid(fmt.Errorf("sodex: 客户端订单号 %d 非法", req.ClientOrderID))
	}
	qty := m.RoundQty(req.Quantity)
	raw := &ptypes.RawOrder{
		ClOrdID:      clOrdIDString(req.ClientOrderID),
		Modifier:     enums.OrderModifierNormal,
		Side:         toSide(req.Side),
		ReduceOnly:   req.ReduceOnly,
		PositionSide: enums.PositionSideBoth,
		Quantity:     &qty,
	}

	switch req.Type {
	case order.Market:
		raw.Type = enums.OrderTypeMarket
		raw.TimeInForce = enums.TimeInForceIOC
		// 市价单不带 price。滑点保护由交易所市价 IOC 执行，不是限价保护。
		if !qty.IsPositive() {
			return invalid(fmt.Errorf("sodex: %s 市价单数量必须为正", req.Symbol))
		}
	case order.Limit:
		tif, err := toTIF(req.TIF)
		if err != nil {
			return invalid(err)
		}
		price := m.RoundPrice(req.Price, market.RoundNearest)
		if err := m.CheckOrder(price, qty); err != nil {
			return invalid(fmt.Errorf("sodex: %s 订单不满足市场限制: %w", req.Symbol, err))
		}
		raw.Type = enums.OrderTypeLimit
		raw.TimeInForce = tif
		raw.Price = &price
	default:
		return invalid(fmt.Errorf("sodex: 不支持的订单类型 %s", req.Type))
	}
	return raw, cm, nil
}

func (a *Adapter) ModifyOrders(ctx context.Context, reqs []exchange.ModifyRequest) ([]exchange.ModifyResult, error) {
	results := make([]exchange.ModifyResult, len(reqs))
	type pending struct {
		idx int
		cl  string
		p   *ctypes.ReplaceParams
	}
	ready := make([]pending, 0, len(reqs))
	for i, req := range reqs {
		results[i] = exchange.ModifyResult{ClientOrderID: req.ClientOrderID}
		p, err := a.buildReplace(ctx, req)
		if err != nil {
			results[i].Err = err
			continue
		}
		ready = append(ready, pending{idx: i, cl: p.ClOrdID, p: p})
	}
	for start := 0; start < len(ready); start += defaultBatch {
		end := start + defaultBatch
		if end > len(ready) {
			end = len(ready)
		}
		chunk := ready[start:end]
		orders := make([]*ctypes.ReplaceParams, len(chunk))
		for j, it := range chunk {
			orders[j] = it.p
		}
		placed, err := a.rest.replace(ctx, &ctypes.ReplaceOrderRequest{
			AccountID: a.accountID,
			Orders:    orders,
		})
		if err != nil {
			for _, it := range chunk {
				results[it.idx].Err = err
			}
			continue
		}
		byCl := map[string]client.PlaceOrderResult{}
		for _, p := range placed {
			byCl[p.ClOrdID] = p
		}
		for j, it := range chunk {
			p, ok := byCl[it.cl]
			if !ok {
				if j < len(placed) {
					p = placed[j]
				} else {
					results[it.idx].Err = classify("modify_order", fmt.Errorf("sodex: 改单响应缺少 clOrdID %s", it.cl))
					continue
				}
			}
			if placeRejected(p, false) {
				msg := p.Message
				if msg == "" {
					msg = p.Status
				}
				results[it.idx].Err = classify("modify_order", fmt.Errorf("sodex: %s", msg))
				continue
			}
			if p.OrderID != 0 {
				results[it.idx].TxHash = strconv.FormatUint(p.OrderID, 10)
			} else {
				results[it.idx].TxHash = it.cl
			}
		}
	}
	return results, nil
}

func (a *Adapter) buildReplace(ctx context.Context, req exchange.ModifyRequest) (*ctypes.ReplaceParams, error) {
	cm, err := a.lookup(ctx, req.Symbol)
	if err != nil {
		return nil, err
	}
	m := cm.model
	invalid := func(err error) (*ctypes.ReplaceParams, error) {
		return nil, exchange.Classify(exchange.ClassInvalidParam, "modify_order", err)
	}
	if !req.ClientOrderID.Valid() {
		return invalid(fmt.Errorf("sodex: 客户端订单号 %d 非法", req.ClientOrderID))
	}
	price := m.RoundPrice(req.Price, market.RoundNearest)
	qty := m.RoundQty(req.Quantity)
	if err := m.CheckOrder(price, qty); err != nil {
		return invalid(fmt.Errorf("sodex: %s 改单不满足市场限制: %w", req.Symbol, err))
	}
	cl := clOrdIDString(req.ClientOrderID)
	orig := cl
	return &ctypes.ReplaceParams{
		SymbolID:    cm.symbol.SymbolID,
		ClOrdID:     cl,
		OrigClOrdID: &orig,
		Price:       &price,
		Quantity:    &qty,
	}, nil
}

func (a *Adapter) CancelOrders(ctx context.Context, reqs []exchange.CancelRequest) ([]exchange.CancelResult, error) {
	results := make([]exchange.CancelResult, len(reqs))
	cancels := make([]*ptypes.CancelOrder, 0, len(reqs))
	idxs := make([]int, 0, len(reqs))
	for i, req := range reqs {
		results[i] = exchange.CancelResult{ClientOrderID: req.ClientOrderID}
		cm, err := a.lookup(ctx, req.Symbol)
		if err != nil {
			results[i].Err = err
			continue
		}
		c := &ptypes.CancelOrder{SymbolID: cm.symbol.SymbolID}
		if req.ExchangeID != "" {
			if id, err := strconv.ParseUint(req.ExchangeID, 10, 64); err == nil {
				c.OrderID = &id
			}
		}
		if c.OrderID == nil {
			cl := clOrdIDString(req.ClientOrderID)
			c.ClOrdID = &cl
		}
		cancels = append(cancels, c)
		idxs = append(idxs, i)
	}
	if len(cancels) == 0 {
		return results, nil
	}
	raw, err := a.rest.cancel(ctx, &ptypes.CancelOrderRequest{
		AccountID: a.accountID,
		Cancels:   cancels,
	})
	if err != nil {
		for _, i := range idxs {
			if results[i].Err == nil {
				results[i].Err = err
			}
		}
		return results, nil
	}
	byCl := map[string]client.CancelOrderResult{}
	for _, r := range raw {
		byCl[r.ClOrdID] = r
	}
	for j, i := range idxs {
		cl := ""
		if cancels[j].ClOrdID != nil {
			cl = *cancels[j].ClOrdID
		}
		r, ok := byCl[cl]
		if !ok && j < len(raw) {
			r = raw[j]
		}
		if strings.EqualFold(r.Status, "REJECTED") && r.Message != "" {
			results[i].Err = classify("cancel_order", fmt.Errorf("sodex: %s", r.Message))
			continue
		}
		if r.OrderID != nil {
			results[i].TxHash = strconv.FormatUint(*r.OrderID, 10)
		} else {
			results[i].TxHash = cl
		}
	}
	return results, nil
}

func (a *Adapter) CancelAll(ctx context.Context, symbol string) error {
	if symbol == "" {
		return exchange.Classify(exchange.ClassInvalidParam, "cancel_all",
			fmt.Errorf("sodex: 撤单必须指定交易对，禁止账户级全撤"))
	}
	orders, err := a.OpenOrders(ctx, symbol)
	if err != nil {
		return err
	}
	if len(orders) == 0 {
		return nil
	}
	reqs := make([]exchange.CancelRequest, 0, len(orders))
	for _, o := range orders {
		if !o.State.IsActive() {
			continue
		}
		reqs = append(reqs, exchange.CancelRequest{
			Symbol:        symbol,
			ClientOrderID: o.ClientOrderID,
			ExchangeID:    o.ExchangeID,
		})
	}
	if len(reqs) == 0 {
		return nil
	}
	results, err := a.CancelOrders(ctx, reqs)
	if err != nil {
		return err
	}
	for _, r := range results {
		if r.Err != nil {
			return r.Err
		}
	}
	return nil
}

// Tradable 检查该交易对当前是否可以开仓。
func (a *Adapter) Tradable(ctx context.Context, symbol string) error {
	_, err := a.tradable(ctx, symbol)
	return err
}

// AllPerpMarkets 返回全部永续合约，包含已暂停的。
func (a *Adapter) AllPerpMarkets(ctx context.Context) ([]exchange.MarketInfo, error) {
	if err := a.ensureMarkets(ctx); err != nil {
		return nil, err
	}
	a.mu.RLock()
	out := make([]exchange.MarketInfo, 0, len(a.byIndex))
	for _, cm := range a.byIndex {
		out = append(out, toMarketInfo(cm.symbol, a.tickers[strings.ToUpper(cm.symbol.Symbol)]))
	}
	a.mu.RUnlock()
	sortByVolume(out)
	return out, nil
}

// CheckCredentials 校验主钱包地址、accountID 与 API key 是否匹配。
func (a *Adapter) CheckCredentials(ctx context.Context) error {
	info, err := a.rest.accountInfo(ctx, a.address)
	if err != nil {
		return fmt.Errorf("sodex: 无法按地址查询账户: %w", err)
	}
	if info.AccountID != a.accountID {
		return fmt.Errorf("sodex: 配置 account_index=%d 与地址 %s 的 aid=%d 不一致",
			a.accountID, a.address, info.AccountID)
	}
	if _, err := a.rest.balances(ctx, a.address); err != nil {
		return fmt.Errorf("sodex: 无法查询余额: %w", err)
	}
	return nil
}
