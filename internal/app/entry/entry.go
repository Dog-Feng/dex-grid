// Package entry 实现三种建仓触发器：市价、maker 跟价、指定价格。
//
// 触发器是纯状态机：时间来自事件，输出是 Action。Runner 负责把
// Action 交给 Executor，再把成交回报喂回来。建仓完成或失败后
// Runner 向策略回发 EntryDoneEvent / EntryFailedEvent。
package entry

import (
	"fmt"
	"time"

	"dex-grid/internal/domain/market"
	"dex-grid/internal/domain/order"
	"dex-grid/internal/domain/strategy"

	"github.com/shopspring/decimal"
)

// Phase 是建仓触发器的内部阶段。
type Phase uint8

const (
	PhaseIdle Phase = iota
	PhasePlacing
	PhaseResting
	PhaseRepricing
	PhaseDone
	PhaseFailed
)

func (p Phase) String() string {
	switch p {
	case PhasePlacing:
		return "placing"
	case PhaseResting:
		return "resting"
	case PhaseRepricing:
		return "repricing"
	case PhaseDone:
		return "done"
	case PhaseFailed:
		return "failed"
	default:
		return "idle"
	}
}

// Trigger 把仓位从当前值调整到绝对目标。
type Trigger struct {
	params strategy.EntryParams
	mkt    market.Market
	slot   uint8
	epoch  uint16

	target  decimal.Decimal
	start   decimal.Decimal
	filled  decimal.Decimal
	phase   Phase
	reason  string
	started time.Time

	coid        order.ClientOrderID
	seq         uint8
	orderPrice  decimal.Decimal
	orderQty    decimal.Decimal
	orderFill   decimal.Decimal // 当前挂单已计入 filled 的数量
	repriceN    int
	lastReprice time.Time
	sliceIdx    int
	lastSlice   time.Time
	book        market.BookTicker
	mark        decimal.Decimal
	forceMarket bool // 超时后转市价补齐，不改原始配置
}

// New 构造一个尚未启动的触发器。
func New(params strategy.EntryParams, mkt market.Market, slot uint8, epoch uint16) *Trigger {
	return &Trigger{params: params, mkt: mkt, slot: slot, epoch: epoch}
}

// Start 开始把仓位调整到 target（带符号的绝对值）。current 是启动时的仓位。
func (t *Trigger) Start(target, current decimal.Decimal, book market.BookTicker, mark decimal.Decimal, now time.Time) []strategy.Action {
	t.target = target
	t.start = current
	t.filled = decimal.Zero
	t.started = now
	t.book = book
	t.mark = mark
	t.repriceN = 0
	t.sliceIdx = 0
	t.seq = 0
	t.coid = 0
	t.reason = ""
	t.forceMarket = false

	if t.done() {
		t.phase = PhaseDone
		return nil
	}
	return t.placeNext(now)
}

// OnEvent 推进状态机。done/failed 同时为假时表示还在进行。
func (t *Trigger) OnEvent(ev strategy.Event) (acts []strategy.Action, done, failed bool) {
	if t.phase == PhaseDone || t.phase == PhaseFailed || t.phase == PhaseIdle {
		return nil, t.phase == PhaseDone, t.phase == PhaseFailed
	}

	switch e := ev.(type) {
	case strategy.BookEvent:
		t.book = e.Book
		if e.Mark.IsPositive() {
			t.mark = e.Mark
		}
		acts = t.onBook(e.Now)
	case strategy.OrderEvent:
		acts = t.onOrder(e)
	case strategy.TickEvent:
		acts = t.onTick(e.Now)
	case strategy.PositionEvent:
		// 仓位事件只作参考，成交量以订单回报为准，避免双边计数。
	}

	return acts, t.phase == PhaseDone, t.phase == PhaseFailed
}

// Result 返回本次建仓实际成交的仓位增量（带符号）与失败原因。
func (t *Trigger) Result() (filled decimal.Decimal, reason string) {
	return t.filled, t.reason
}

// Active 表示触发器正在工作。
func (t *Trigger) Active() bool {
	return t.phase == PhasePlacing || t.phase == PhaseResting || t.phase == PhaseRepricing
}

func (t *Trigger) remaining() decimal.Decimal {
	return t.target.Sub(t.start.Add(t.filled))
}

func (t *Trigger) done() bool {
	rem := t.remaining().Abs()
	if rem.IsZero() {
		return true
	}
	need := t.target.Sub(t.start).Abs()
	if need.IsZero() {
		return rem.LessThanOrEqual(t.mkt.LotSize)
	}
	tol := t.params.FillTolerance
	if tol.IsZero() {
		tol = decimal.RequireFromString("0.01")
	}
	threshold := need.Mul(tol)
	if t.mkt.LotSize.GreaterThan(threshold) {
		threshold = t.mkt.LotSize
	}
	return rem.LessThanOrEqual(threshold)
}

func (t *Trigger) fail(reason string) []strategy.Action {
	t.phase = PhaseFailed
	t.reason = reason
	if t.coid != 0 {
		id := t.coid
		t.coid = 0
		return []strategy.Action{strategy.CancelOrder{ClientOrderID: id}}
	}
	return nil
}

func (t *Trigger) finish() []strategy.Action {
	t.phase = PhaseDone
	return nil
}

func (t *Trigger) onOrder(e strategy.OrderEvent) []strategy.Action {
	o := e.Order
	if o.ClientOrderID != t.coid {
		return nil
	}
	switch o.State {
	case order.StateOpen, order.StatePartiallyFilled:
		if o.FilledQty.IsPositive() {
			t.noteFill(o)
		}
		if t.done() {
			return t.finish()
		}
		t.phase = PhaseResting
		return nil

	case order.StateFilled:
		t.noteFill(o)
		t.coid = 0
		if t.done() {
			return t.finish()
		}
		if t.marketMode() && t.params.SliceInterval > 0 {
			t.phase = PhasePlacing
			return nil
		}
		return t.placeNext(e.Now)

	case order.StateRejected:
		t.coid = 0
		t.phase = PhasePlacing
		return t.placeNext(e.Now)

	case order.StateCanceled, order.StateExpired:
		if o.FilledQty.IsPositive() {
			t.noteFill(o)
		}
		t.coid = 0
		if t.phase == PhaseRepricing {
			return t.placeNext(e.Now)
		}
		if t.done() {
			return t.finish()
		}
		t.phase = PhasePlacing
		return t.placeNext(e.Now)
	}
	return nil
}

func (t *Trigger) noteFill(o order.Order) {
	// 订单回报里的 FilledQty 是该单累计成交。减去本单已计入的部分，避免部分成交被加两次。
	t.applySignedFill(o.Side, o.FilledQty.Sub(t.orderFill))
	t.orderFill = o.FilledQty
}

func (t *Trigger) applySignedFill(side order.Side, qty decimal.Decimal) {
	if !qty.IsPositive() {
		return
	}
	if side == order.Sell {
		t.filled = t.filled.Sub(qty)
		return
	}
	t.filled = t.filled.Add(qty)
}

func (t *Trigger) onBook(now time.Time) []strategy.Action {
	if t.params.Mode != strategy.EntryMakerFollow || t.phase != PhaseResting || t.coid == 0 {
		return nil
	}
	want := t.followPrice()
	if !want.IsPositive() {
		return nil
	}
	ticks := t.params.RepriceTicks
	if ticks <= 0 {
		ticks = 1
	}
	diff := want.Sub(t.orderPrice).Abs()
	if diff.LessThan(t.mkt.TickSize.Mul(decimal.NewFromInt(int64(ticks)))) {
		return nil
	}
	if t.params.RepriceInterval > 0 && !t.lastReprice.IsZero() &&
		now.Sub(t.lastReprice) < t.params.RepriceInterval.Std() {
		return nil
	}
	if t.params.MaxReprice > 0 && t.repriceN >= t.params.MaxReprice {
		return t.onTimeout(now)
	}
	t.phase = PhaseRepricing
	t.lastReprice = now
	t.repriceN++
	id := t.coid
	return []strategy.Action{strategy.CancelOrder{ClientOrderID: id}}
}

func (t *Trigger) onTick(now time.Time) []strategy.Action {
	if t.params.Timeout > 0 && !t.started.IsZero() && now.Sub(t.started) >= t.params.Timeout.Std() {
		return t.onTimeout(now)
	}
	if t.marketMode() && t.phase == PhasePlacing && t.coid == 0 {
		if t.params.SliceInterval > 0 && !t.lastSlice.IsZero() &&
			now.Sub(t.lastSlice) < t.params.SliceInterval.Std() {
			return nil
		}
		return t.placeNext(now)
	}
	return nil
}

func (t *Trigger) onTimeout(now time.Time) []strategy.Action {
	switch t.params.OnTimeout {
	case strategy.TimeoutKeep:
		return nil
	case strategy.TimeoutAbort:
		return t.fail("entry timeout")
	default:
		// 转市价补齐剩余。
		t.forceMarket = true
		if t.coid != 0 {
			t.phase = PhaseRepricing
			id := t.coid
			return []strategy.Action{strategy.CancelOrder{ClientOrderID: id}}
		}
		return t.placeNext(now)
	}
}

func (t *Trigger) placeNext(now time.Time) []strategy.Action {
	if t.done() {
		return t.finish()
	}
	rem := t.remaining()
	side := order.Buy
	if rem.IsNegative() {
		side = order.Sell
	}
	qty := rem.Abs()

	if t.marketMode() {
		n := t.params.SliceCount
		if n < 1 {
			n = 1
		}
		leftSlices := n - t.sliceIdx
		if leftSlices < 1 {
			leftSlices = 1
		}
		qty = t.mkt.RoundQty(qty.Div(decimal.NewFromInt(int64(leftSlices))))
		if qty.IsZero() {
			qty = t.mkt.RoundQty(rem.Abs())
		}
	}
	if qty.IsZero() {
		return t.finish()
	}

	price, typ, tif, err := t.quote(side)
	if err != nil {
		// 盘口还没到，继续等。
		t.phase = PhasePlacing
		return nil
	}
	if err := t.mkt.CheckOrder(price, qty); err != nil {
		return t.fail(err.Error())
	}

	t.seq = order.NextSeq(t.seq)
	coid, err := order.Encode(order.Ref{
		Slot:    t.slot,
		Epoch:   t.epoch,
		Purpose: order.PurposeEntry,
		Seq:     t.seq,
	})
	if err != nil {
		return t.fail(err.Error())
	}

	t.coid = coid
	t.orderPrice = price
	t.orderQty = qty
	t.orderFill = decimal.Zero
	t.phase = PhasePlacing
	t.lastSlice = now
	t.sliceIdx++

	return []strategy.Action{strategy.PlaceOrder{
		ClientOrderID: coid,
		Side:          side,
		Type:          typ,
		Price:         price,
		Quantity:      qty,
		TIF:           tif,
	}}
}

func (t *Trigger) marketMode() bool {
	return t.forceMarket || t.params.Mode == strategy.EntryMarket
}

func (t *Trigger) quote(side order.Side) (decimal.Decimal, order.Type, order.TIF, error) {
	if t.marketMode() {
		px := t.protectionPrice(side)
		if !px.IsPositive() {
			return decimal.Zero, 0, 0, fmt.Errorf("no price for market entry")
		}
		return px, order.Market, order.IOC, nil
	}
	switch t.params.Mode {
	case strategy.EntryLimitPrice:
		px := t.mkt.RoundPrice(t.params.Price, market.RoundNearest)
		if !px.IsPositive() {
			return decimal.Zero, 0, 0, fmt.Errorf("limit entry price missing")
		}
		return px, order.Limit, order.PostOnly, nil

	default:
		px := t.followPrice()
		if !px.IsPositive() {
			return decimal.Zero, 0, 0, fmt.Errorf("no book for maker follow")
		}
		return px, order.Limit, order.PostOnly, nil
	}
}

func (t *Trigger) followPrice() decimal.Decimal {
	if !t.book.Valid() {
		return decimal.Zero
	}
	if t.remaining().IsNegative() {
		return t.mkt.RoundPrice(t.book.Ask, market.RoundNearest)
	}
	return t.mkt.RoundPrice(t.book.Bid, market.RoundNearest)
}

func (t *Trigger) protectionPrice(side order.Side) decimal.Decimal {
	slip := t.params.MaxSlippage
	if slip.IsZero() {
		slip = decimal.RequireFromString("0.005")
	}
	ref := t.mark
	if t.book.Valid() {
		if side == order.Buy {
			ref = t.book.Ask
		} else {
			ref = t.book.Bid
		}
	}
	if !ref.IsPositive() {
		return decimal.Zero
	}
	if side == order.Buy {
		return t.mkt.RoundPrice(ref.Mul(decimal.NewFromInt(1).Add(slip)), market.RoundUp)
	}
	return t.mkt.RoundPrice(ref.Mul(decimal.NewFromInt(1).Sub(slip)), market.RoundDown)
}
