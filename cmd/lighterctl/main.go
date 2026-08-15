// Command lighterctl 是 Lighter 适配器的主网验证工具。
//
// 页面接入之前，用它来核对账户、持仓、盘口，并做小额的下单/撤单验证。
// 所有会改变账户状态的操作都必须显式加 -yes，避免误触。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"dex-grid/internal/config"
	"dex-grid/internal/domain/market"
	"dex-grid/internal/domain/order"
	"dex-grid/internal/exchange"
	"dex-grid/internal/exchange/lighter"
	"dex-grid/internal/infra/httpx"

	"github.com/shopspring/decimal"
)

const usage = `lighterctl — Lighter 主网验证工具

本系统只做永续合约网格，所有命令的标的都是永续合约；
现货市场与已下架的合约不会出现在列表里，也不允许开仓。

用法:
  lighterctl [全局参数] <命令> [命令参数]

只读命令:
  markets     列出可交易的永续合约（下拉列表的数据源），支持关键字过滤
  market      查看单个合约的元数据与行情
  book        查看盘口最优档
  account     查看账户资金
  positions   查看非空持仓
  orders      查看某个市场的挂单
  stream      订阅 WebSocket 事件流并打印（-raw 打印原始帧）
  check       校验凭证（auth token + nonce）

写入命令（必须加 -yes）:
  maker       挂一笔 post-only 限价单
  taker       下一笔市价单
  cancel      按客户端订单号撤单
  cancel-all  撤销指定交易对的全部挂单
  close       市价平掉某个市场的仓位

全局参数:
  -config     配置文件路径，默认 config/config.yaml

示例:
  lighterctl markets -q sol
  lighterctl book -m 2
  lighterctl maker -m 2 -side buy -qty 0.15 -offset 0.02 -yes
  lighterctl cancel -m 2 -coid 17592186044417 -yes
`

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "错误:", err)
		os.Exit(1)
	}
}

func run() error {
	fs := flag.NewFlagSet("lighterctl", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	cfgPath := fs.String("config", "config/config.yaml", "配置文件路径")
	fs.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	if err := fs.Parse(os.Args[1:]); err != nil {
		return err
	}
	args := fs.Args()
	if len(args) == 0 {
		fs.Usage()
		return errors.New("缺少命令")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	app, err := newApp(*cfgPath)
	if err != nil {
		return err
	}
	defer app.ex.Close()

	cmd, rest := args[0], args[1:]
	switch cmd {
	case "markets":
		return app.markets(ctx, rest)
	case "market":
		return app.market(ctx, rest)
	case "book":
		return app.book(ctx, rest)
	case "account":
		return app.account(ctx)
	case "positions":
		return app.positions(ctx)
	case "orders":
		return app.orders(ctx, rest)
	case "stream":
		return app.stream(ctx, rest)
	case "check":
		return app.check(ctx)
	case "maker":
		return app.maker(ctx, rest)
	case "taker":
		return app.taker(ctx, rest)
	case "cancel":
		return app.cancel(ctx, rest)
	case "cancel-all":
		return app.cancelAll(ctx, rest)
	case "close":
		return app.closePosition(ctx, rest)
	default:
		fs.Usage()
		return fmt.Errorf("未知命令 %q", cmd)
	}
}

type app struct {
	cfg *config.Config
	ex  *lighter.Adapter
	out *tabwriter.Writer
}

func newApp(cfgPath string) (*app, error) {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return nil, err
	}
	exCfg, ok := cfg.Find(lighter.Name)
	if !ok || !exCfg.Enabled {
		return nil, fmt.Errorf("配置里没有启用的 lighter 交易所")
	}

	httpc, err := httpx.New(cfg.Proxy, exCfg.Timeout.Std())
	if err != nil {
		return nil, err
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))

	ex, err := exchange.New(exCfg, exchange.Deps{Log: log, HTTP: httpc})
	if err != nil {
		return nil, err
	}
	ad, ok := ex.(*lighter.Adapter)
	if !ok {
		return nil, errors.New("内部错误：交易所适配器类型不是 lighter")
	}
	return &app{
		cfg: cfg,
		ex:  ad,
		out: tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0),
	}, nil
}

func (a *app) flush() { a.out.Flush() }

// resolveSymbol 把 -m 指定的 market_index 解析成交易对符号。
func (a *app) resolveSymbol(ctx context.Context, index int) (string, market.Market, error) {
	m, err := a.ex.LookupByIndex(ctx, index)
	if err != nil {
		return "", market.Market{}, err
	}
	return m.Symbol, m, nil
}

// --- 只读命令 ---

func (a *app) markets(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("markets", flag.ContinueOnError)
	q := fs.String("q", "", "按符号关键字过滤（不区分大小写）")
	limit := fs.Int("n", 30, "最多显示多少条，0 表示全部")
	all := fs.Bool("all", false, "把已下架的合约也列出来（正常选择交易对时不需要）")
	if err := fs.Parse(args); err != nil {
		return err
	}

	var (
		ms  []exchange.MarketInfo
		err error
	)
	if *all {
		ms, err = a.ex.AllPerpMarkets(ctx)
	} else {
		ms, err = a.ex.Markets(ctx)
	}
	if err != nil {
		return err
	}

	needle := strings.ToUpper(*q)
	shown := 0
	fmt.Fprintln(a.out, "INDEX\tSYMBOL\t类型\t状态\t标记价\t最大杠杆\t24h成交额(USDC)")
	for _, m := range ms {
		if needle != "" && !strings.Contains(strings.ToUpper(m.Symbol), needle) {
			continue
		}
		fmt.Fprintf(a.out, "%d\t%s\t%s\t%s\t%s\t%dx\t%s\n",
			m.MarketIndex, m.Symbol, m.Type, m.Status,
			m.MarkPrice, m.MaxLeverage, m.DailyQuoteVolume.Round(0))
		shown++
		if *limit > 0 && shown >= *limit {
			break
		}
	}
	a.flush()

	scope := "个可交易的永续合约"
	if *all {
		scope = "个永续合约（含已下架）"
	}
	fmt.Printf("\n共 %d %s", len(ms), scope)
	if needle != "" {
		fmt.Printf("，匹配 %q 的显示了 %d 个", *q, shown)
	} else if *limit > 0 && len(ms) > shown {
		fmt.Printf("，按 24h 成交额降序显示前 %d 个（-n 0 看全部）", shown)
	}
	fmt.Println()
	return nil
}

func (a *app) market(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("market", flag.ContinueOnError)
	idx := fs.Int("m", -1, "market_index")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *idx < 0 {
		return errors.New("需要 -m 指定 market_index")
	}
	symbol, m, err := a.resolveSymbol(ctx, *idx)
	if err != nil {
		return err
	}
	t, err := a.ex.Ticker(ctx, symbol)
	if err != nil {
		return err
	}

	fmt.Fprintf(a.out, "交易对\t%s (market_index=%d)\n", m.Symbol, *idx)
	fmt.Fprintf(a.out, "最小报价单位\t%s\n", m.TickSize)
	fmt.Fprintf(a.out, "最小数量单位\t%s\n", m.LotSize)
	fmt.Fprintf(a.out, "最小下单数量\t%s\n", m.MinQty)
	fmt.Fprintf(a.out, "最小下单名义\t%s USDC\n", m.MinNotional)
	fmt.Fprintf(a.out, "最大杠杆\t%dx\n", m.MaxLeverage)
	fmt.Fprintf(a.out, "maker/taker 费率\t%s%% / %s%%\n",
		m.MakerFeeRate.Mul(decimal.NewFromInt(100)), m.TakerFeeRate.Mul(decimal.NewFromInt(100)))
	fmt.Fprintf(a.out, "维持保证金率\t%s%%\n", m.MaintMarginRate.Mul(decimal.NewFromInt(100)))
	fmt.Fprintf(a.out, "标记价 / 指数价\t%s / %s\n", t.Mark, t.Index)
	fmt.Fprintf(a.out, "买一 / 卖一\t%s / %s\n", t.Book.Bid, t.Book.Ask)
	a.flush()

	// 最小可下单量取「最小数量」与「最小名义换算出的数量」中的较大者。
	minByNotional := m.RoundQty(m.MinNotional.Div(t.Mark)).Add(m.LotSize)
	minQty := m.MinQty
	if minByNotional.GreaterThan(minQty) {
		minQty = minByNotional
	}
	fmt.Printf("\n按当前标记价，最小可下单量约 %s %s（名义 %s USDC）\n",
		minQty, m.Symbol, minQty.Mul(t.Mark).Round(2))
	return nil
}

func (a *app) book(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("book", flag.ContinueOnError)
	idx := fs.Int("m", -1, "market_index")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *idx < 0 {
		return errors.New("需要 -m 指定 market_index")
	}
	symbol, _, err := a.resolveSymbol(ctx, *idx)
	if err != nil {
		return err
	}
	t, err := a.ex.Ticker(ctx, symbol)
	if err != nil {
		return err
	}
	fmt.Fprintf(a.out, "%s\t价格\t数量\n", symbol)
	fmt.Fprintf(a.out, "卖一\t%s\t%s\n", t.Book.Ask, t.Book.AskSize)
	fmt.Fprintf(a.out, "买一\t%s\t%s\n", t.Book.Bid, t.Book.BidSize)
	fmt.Fprintf(a.out, "价差\t%s\t\n", t.Book.Spread())
	fmt.Fprintf(a.out, "标记价\t%s\t\n", t.Mark)
	a.flush()
	return nil
}

func (a *app) account(ctx context.Context) error {
	snap, err := a.ex.Account(ctx)
	if err != nil {
		return err
	}
	fmt.Fprintf(a.out, "账户余额（抵押品）\t%s USDC\n", snap.Balance)
	fmt.Fprintf(a.out, "权益（总资产）\t%s USDC\n", snap.Equity)
	fmt.Fprintf(a.out, "可用保证金\t%s USDC\n", snap.Available)
	fmt.Fprintf(a.out, "已占用保证金\t%s USDC\n", snap.MarginUsed)
	fmt.Fprintf(a.out, "未实现盈亏\t%s USDC\n", snap.UnrealizedPnL)
	if ratio := snap.MarginRatio(); ratio.IsPositive() {
		fmt.Fprintf(a.out, "保证金率\t%s\n", ratio.Round(2))
	}
	a.flush()
	return nil
}

func (a *app) positions(ctx context.Context) error {
	ps, err := a.ex.Positions(ctx)
	if err != nil {
		return err
	}
	if len(ps) == 0 {
		fmt.Println("当前没有持仓")
		return nil
	}
	fmt.Fprintln(a.out, "交易对\t方向\t数量\t均价\t标记价\t未实现盈亏\t强平价\t杠杆\t模式")
	for _, p := range ps {
		fmt.Fprintf(a.out, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%dx\t%s\n",
			p.Symbol, p.Direction(), p.AbsSize(), p.EntryPrice, p.MarkPrice,
			p.UnrealizedPnL, p.LiquidationPrice, p.Leverage, p.MarginMode)
	}
	a.flush()
	return nil
}

func (a *app) orders(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("orders", flag.ContinueOnError)
	idx := fs.Int("m", -1, "market_index")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *idx < 0 {
		return errors.New("需要 -m 指定 market_index")
	}
	symbol, _, err := a.resolveSymbol(ctx, *idx)
	if err != nil {
		return err
	}
	os_, err := a.ex.OpenOrders(ctx, symbol)
	if err != nil {
		return err
	}
	if len(os_) == 0 {
		fmt.Printf("%s 当前没有挂单\n", symbol)
		return nil
	}
	fmt.Fprintln(a.out, "客户端订单号\t交易所订单号\t方向\t价格\t数量\t已成交\t有效期\treduceOnly\t状态")
	for _, o := range os_ {
		fmt.Fprintf(a.out, "%d\t%s\t%s\t%s\t%s\t%s\t%s\t%v\t%s\n",
			o.ClientOrderID, o.ExchangeID, o.Side, o.Price, o.Quantity,
			o.FilledQty, o.TIF, o.ReduceOnly, o.State)
	}
	a.flush()
	return nil
}

func (a *app) stream(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("stream", flag.ContinueOnError)
	idx := fs.Int("m", -1, "market_index")
	dur := fs.Duration("d", 30*time.Second, "运行时长，0 表示一直跑到 Ctrl+C")
	raw := fs.Bool("raw", false, "打印原始 JSON 帧而不是解析后的事件")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *idx < 0 {
		return errors.New("需要 -m 指定 market_index")
	}
	symbol, _, err := a.resolveSymbol(ctx, *idx)
	if err != nil {
		return err
	}

	if *dur > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, *dur)
		defer cancel()
	}

	var events <-chan exchange.StreamEvent
	if *raw {
		events, err = a.ex.SubscribeRaw(ctx, symbol, func(frame []byte) {
			fmt.Printf("%s  %s\n", time.Now().Format("15:04:05.000"), frame)
		})
	} else {
		events, err = a.ex.Subscribe(ctx, symbol)
	}
	if err != nil {
		return err
	}

	fmt.Printf("已订阅 %s 的事件流，按 Ctrl+C 退出\n\n", symbol)
	counts := map[string]int{}
	for ev := range events {
		stamp := ev.Time.Format("15:04:05.000")
		switch {
		case ev.Err != nil:
			counts["error"]++
			fmt.Printf("%s  [错误] %v\n", stamp, ev.Err)
		case ev.Resync:
			counts["resync"]++
			fmt.Printf("%s  [重连] 需要重新对账\n", stamp)
		case ev.Ticker != nil:
			counts["ticker"]++
			if !*raw {
				fmt.Printf("%s  [行情] 买一 %s (%s) / 卖一 %s (%s) 标记价 %s\n", stamp,
					ev.Ticker.Book.Bid, ev.Ticker.Book.BidSize,
					ev.Ticker.Book.Ask, ev.Ticker.Book.AskSize, ev.Ticker.Mark)
			}
		case ev.Order != nil:
			counts["order"]++
			o := ev.Order
			// 市价单的 Price 是滑点保护上限而不是成交价，成交后要看均价。
			shown := o.Price
			label := "挂单价"
			if o.FilledQty.IsPositive() && o.AvgFillPrice.IsPositive() {
				shown, label = o.AvgFillPrice, "成交均价"
			}
			fmt.Printf("%s  [订单] coid=%d %s %s %s %s 已成交 %s 状态 %s\n", stamp,
				o.ClientOrderID, o.Side, o.Quantity, label, shown, o.FilledQty, o.State)
		case ev.Position != nil:
			counts["position"]++
			p := ev.Position
			fmt.Printf("%s  [仓位] %s %s 均价 %s 浮盈 %s\n", stamp,
				p.Direction(), p.AbsSize(), p.EntryPrice, p.UnrealizedPnL)
		}
	}

	fmt.Printf("\n事件统计: 行情 %d，订单 %d，仓位 %d，重连 %d，错误 %d\n",
		counts["ticker"], counts["order"], counts["position"], counts["resync"], counts["error"])
	return nil
}

func (a *app) check(ctx context.Context) error {
	if err := a.ex.CheckCredentials(ctx); err != nil {
		return err
	}
	fmt.Println("凭证校验通过：auth token 可生成，nonce 可获取")
	return nil
}

// --- 写入命令 ---

// orderFlags 是下单类命令的公共参数。
type orderFlags struct {
	marketIndex int
	side        string
	qty         string
	notional    string
	reduceOnly  bool
	cell        int
	yes         bool
}

func (o *orderFlags) bind(fs *flag.FlagSet) {
	fs.IntVar(&o.marketIndex, "m", -1, "market_index")
	fs.StringVar(&o.side, "side", "", "buy | sell")
	fs.StringVar(&o.qty, "qty", "", "下单数量（币），与 -notional 二选一")
	fs.StringVar(&o.notional, "notional", "", "下单名义价值（USDC），按现价折算成数量")
	fs.BoolVar(&o.reduceOnly, "reduce-only", false, "只减仓")
	fs.IntVar(&o.cell, "cell", 0, "写进客户端订单号的格子索引，用于区分多笔测试单")
	fs.BoolVar(&o.yes, "yes", false, "确认执行（不加则只做演练，不发送）")
}

// resolve 把参数解析成可下单的数量与市场信息。
func (o *orderFlags) resolve(ctx context.Context, a *app) (string, market.Market, order.Side, decimal.Decimal, error) {
	var zero decimal.Decimal
	if o.marketIndex < 0 {
		return "", market.Market{}, 0, zero, errors.New("需要 -m 指定 market_index")
	}
	side, err := order.ParseSide(o.side)
	if err != nil {
		return "", market.Market{}, 0, zero, fmt.Errorf("需要 -side buy 或 -side sell")
	}
	symbol, m, err := a.resolveSymbol(ctx, o.marketIndex)
	if err != nil {
		return "", market.Market{}, 0, zero, err
	}
	// 先问清楚能不能交易，否则下架合约会先撞上「盘口为空」这种令人困惑的报错。
	if !o.reduceOnly {
		if err := a.ex.Tradable(ctx, symbol); err != nil {
			return "", m, side, zero, err
		}
	}

	var qty decimal.Decimal
	switch {
	case o.qty != "":
		qty, err = decimal.NewFromString(o.qty)
		if err != nil {
			return "", m, side, zero, fmt.Errorf("-qty 不是合法数字: %w", err)
		}
	case o.notional != "":
		n, err := decimal.NewFromString(o.notional)
		if err != nil {
			return "", m, side, zero, fmt.Errorf("-notional 不是合法数字: %w", err)
		}
		t, err := a.ex.Ticker(ctx, symbol)
		if err != nil {
			return "", m, side, zero, err
		}
		qty = n.Div(t.Mark)
	default:
		return "", m, side, zero, errors.New("需要 -qty 或 -notional 指定下单规模")
	}

	qty = m.RoundQty(qty)
	return symbol, m, side, qty, nil
}

// newCOID 生成一个本次测试用的客户端订单号。
//
// epoch 取当前分钟数，保证多次运行不会撞号；cell 由 -cell 区分同一轮里的多笔单。
func newCOID(cell int, purpose order.Purpose) (order.ClientOrderID, error) {
	slot, err := exchange.Slot(lighter.Name)
	if err != nil {
		return 0, err
	}
	return order.Encode(order.Ref{
		Slot:    slot,
		Epoch:   uint16(time.Now().Unix() / 60 % 65536),
		Cell:    uint16(cell),
		Purpose: purpose,
	})
}

func (a *app) maker(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("maker", flag.ContinueOnError)
	var of orderFlags
	of.bind(fs)
	price := fs.String("price", "", "挂单价格；不填则按 -offset 相对盘口计算")
	offset := fs.String("offset", "0.02", "相对买一/卖一的偏离比例，0.02 表示挂在离盘口 2% 的位置")
	force := fs.Bool("force", false, "跳过本地的 post-only 穿价检查（诊断用：故意让交易所拒单以核对错误分类）")
	if err := fs.Parse(args); err != nil {
		return err
	}

	symbol, m, side, qty, err := of.resolve(ctx, a)
	if err != nil {
		return err
	}
	t, err := a.ex.Ticker(ctx, symbol)
	if err != nil {
		return err
	}

	var limit decimal.Decimal
	if *price != "" {
		if limit, err = decimal.NewFromString(*price); err != nil {
			return fmt.Errorf("-price 不是合法数字: %w", err)
		}
	} else {
		off, err := decimal.NewFromString(*offset)
		if err != nil {
			return fmt.Errorf("-offset 不是合法数字: %w", err)
		}
		one := decimal.NewFromInt(1)
		if side == order.Buy {
			limit = t.Book.Bid.Mul(one.Sub(off))
		} else {
			limit = t.Book.Ask.Mul(one.Add(off))
		}
	}
	limit = m.RoundPrice(limit, market.RoundNearest)

	// post-only 单如果会立即成交会被交易所直接拒绝，这里先自己拦一道。
	if !*force {
		if side == order.Buy && limit.GreaterThanOrEqual(t.Book.Ask) {
			return fmt.Errorf("买单价 %s 不低于卖一价 %s，post-only 会被拒绝（-force 可强行发送）", limit, t.Book.Ask)
		}
		if side == order.Sell && limit.LessThanOrEqual(t.Book.Bid) {
			return fmt.Errorf("卖单价 %s 不高于买一价 %s，post-only 会被拒绝（-force 可强行发送）", limit, t.Book.Bid)
		}
	}

	coid, err := newCOID(of.cell, order.PurposeOpen)
	if err != nil {
		return err
	}
	req := exchange.PlaceRequest{
		Symbol:        symbol,
		ClientOrderID: coid,
		Side:          side,
		Type:          order.Limit,
		Price:         limit,
		Quantity:      qty,
		TIF:           order.PostOnly,
		ReduceOnly:    of.reduceOnly,
	}

	fmt.Printf("post-only %s %s %s @ %s（名义 %s USDC，客户端订单号 %d）\n",
		side, qty, symbol, limit, qty.Mul(limit).Round(2), coid)
	if err := m.CheckOrder(limit, qty); err != nil {
		return fmt.Errorf("不满足市场限制: %w", err)
	}
	if !of.yes {
		fmt.Println("演练模式，未发送。确认无误后加 -yes 执行。")
		return nil
	}
	return a.submit(ctx, req)
}

func (a *app) taker(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("taker", flag.ContinueOnError)
	var of orderFlags
	of.bind(fs)
	slippage := fs.String("slippage", "0.005", "可接受的最大滑点，0.005 表示 0.5%")
	if err := fs.Parse(args); err != nil {
		return err
	}

	symbol, m, side, qty, err := of.resolve(ctx, a)
	if err != nil {
		return err
	}
	t, err := a.ex.Ticker(ctx, symbol)
	if err != nil {
		return err
	}
	slip, err := decimal.NewFromString(*slippage)
	if err != nil {
		return fmt.Errorf("-slippage 不是合法数字: %w", err)
	}

	// 市价单的价格字段是「可接受的最差成交价」，不是挂单价。
	one := decimal.NewFromInt(1)
	worst := t.Book.Ask.Mul(one.Add(slip))
	if side == order.Sell {
		worst = t.Book.Bid.Mul(one.Sub(slip))
	}
	worst = m.RoundPrice(worst, market.RoundNearest)

	coid, err := newCOID(of.cell, order.PurposeEntry)
	if err != nil {
		return err
	}
	req := exchange.PlaceRequest{
		Symbol:        symbol,
		ClientOrderID: coid,
		Side:          side,
		Type:          order.Market,
		Price:         worst,
		Quantity:      qty,
		TIF:           order.IOC,
		ReduceOnly:    of.reduceOnly,
	}

	fmt.Printf("市价 %s %s %s（最差可接受价 %s，预计名义 %s USDC，客户端订单号 %d）\n",
		side, qty, symbol, worst, qty.Mul(t.Mark).Round(2), coid)
	if err := m.CheckOrder(worst, qty); err != nil {
		return fmt.Errorf("不满足市场限制: %w", err)
	}
	if !of.yes {
		fmt.Println("演练模式，未发送。确认无误后加 -yes 执行。")
		return nil
	}
	return a.submit(ctx, req)
}

func (a *app) submit(ctx context.Context, req exchange.PlaceRequest) error {
	results, err := a.ex.PlaceOrders(ctx, []exchange.PlaceRequest{req})
	if err != nil {
		return err
	}
	r := results[0]
	if r.Err != nil {
		return r.Err
	}
	fmt.Printf("已提交，tx_hash=%s\n", r.TxHash)
	return nil
}

func (a *app) cancel(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("cancel", flag.ContinueOnError)
	idx := fs.Int("m", -1, "market_index")
	coid := fs.Int64("coid", 0, "客户端订单号")
	yes := fs.Bool("yes", false, "确认执行")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *idx < 0 || *coid == 0 {
		return errors.New("需要 -m 与 -coid")
	}
	symbol, _, err := a.resolveSymbol(ctx, *idx)
	if err != nil {
		return err
	}
	fmt.Printf("撤销 %s 上客户端订单号 %d 的挂单\n", symbol, *coid)
	if !*yes {
		fmt.Println("演练模式，未发送。确认无误后加 -yes 执行。")
		return nil
	}
	results, err := a.ex.CancelOrders(ctx, []exchange.CancelRequest{{
		Symbol:        symbol,
		ClientOrderID: order.ClientOrderID(*coid),
	}})
	if err != nil {
		return err
	}
	if results[0].Err != nil {
		return results[0].Err
	}
	fmt.Printf("已提交，tx_hash=%s\n", results[0].TxHash)
	return nil
}

func (a *app) cancelAll(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("cancel-all", flag.ContinueOnError)
	idx := fs.Int("m", -1, "market_index")
	yes := fs.Bool("yes", false, "确认执行")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *idx < 0 {
		return errors.New("需要 -m")
	}
	symbol, _, err := a.resolveSymbol(ctx, *idx)
	if err != nil {
		return err
	}
	fmt.Printf("撤销 %s 上的全部挂单（不影响其他交易对）\n", symbol)
	if !*yes {
		fmt.Println("演练模式，未发送。确认无误后加 -yes 执行。")
		return nil
	}
	if err := a.ex.CancelAll(ctx, symbol); err != nil {
		return err
	}
	fmt.Println("已提交")
	return nil
}

func (a *app) closePosition(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("close", flag.ContinueOnError)
	idx := fs.Int("m", -1, "market_index")
	slippage := fs.String("slippage", "0.005", "可接受的最大滑点")
	yes := fs.Bool("yes", false, "确认执行")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *idx < 0 {
		return errors.New("需要 -m 指定 market_index")
	}
	symbol, m, err := a.resolveSymbol(ctx, *idx)
	if err != nil {
		return err
	}
	pos, err := a.ex.Position(ctx, symbol)
	if err != nil {
		return err
	}
	if pos.IsFlat() {
		fmt.Printf("%s 当前没有持仓\n", symbol)
		return nil
	}

	side := order.Sell
	if pos.Size.IsNegative() {
		side = order.Buy
	}
	t, err := a.ex.Ticker(ctx, symbol)
	if err != nil {
		return err
	}
	slip, err := decimal.NewFromString(*slippage)
	if err != nil {
		return fmt.Errorf("-slippage 不是合法数字: %w", err)
	}
	one := decimal.NewFromInt(1)
	worst := t.Book.Ask.Mul(one.Add(slip))
	if side == order.Sell {
		worst = t.Book.Bid.Mul(one.Sub(slip))
	}
	worst = m.RoundPrice(worst, market.RoundNearest)
	qty := m.RoundQty(pos.AbsSize())

	coid, err := newCOID(0, order.PurposeExit)
	if err != nil {
		return err
	}
	fmt.Printf("市价平掉 %s 的 %s 仓位 %s（最差可接受价 %s，客户端订单号 %d）\n",
		symbol, pos.Direction(), qty, worst, coid)
	if !*yes {
		fmt.Println("演练模式，未发送。确认无误后加 -yes 执行。")
		return nil
	}
	return a.submit(ctx, exchange.PlaceRequest{
		Symbol:        symbol,
		ClientOrderID: coid,
		Side:          side,
		Type:          order.Market,
		Price:         worst,
		Quantity:      qty,
		TIF:           order.IOC,
		ReduceOnly:    true,
	})
}
