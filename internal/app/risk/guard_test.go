package risk

import (
	"errors"
	"testing"
	"time"

	"dex-grid/internal/domain/account"
	"dex-grid/internal/domain/market"
	"dex-grid/internal/domain/order"
	"dex-grid/internal/domain/position"
	"dex-grid/internal/domain/strategy"
	"dex-grid/internal/exchange"

	"github.com/shopspring/decimal"
)

func d(s string) decimal.Decimal { return decimal.RequireFromString(s) }

var t0 = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func longGuard(sl, tp string) *Guard {
	p := strategy.DefaultRiskParams()
	p.StopLossPrice = d(sl)
	p.TakeProfitPrice = d(tp)
	g := New(p)
	g.SetPosition(position.Position{Symbol: "BTC", Size: d("1"), MarkPrice: d("150")})
	g.SetMark(d("150"), market.BookTicker{Bid: d("149.9"), Ask: d("150.1")}, t0)
	return g
}

func TestStopLossLong(t *testing.T) {
	g := longGuard("140", "180")
	v := g.Check(strategy.BookEvent{
		Book: market.BookTicker{Bid: d("139.9"), Ask: d("140.1")},
		Mark: d("139.5"),
		Now:  t0.Add(time.Second),
	})
	if !v.Stop || v.Reason != strategy.StopStopLoss {
		t.Fatalf("verdict = %+v", v)
	}
	if len(v.Actions) != 3 {
		t.Fatalf("actions = %d, want cancel+market close+stop", len(v.Actions))
	}
	cp, ok := v.Actions[1].(strategy.ClosePosition)
	if !ok || cp.Urgency != strategy.UrgencyMarket {
		t.Fatalf("second action = %#v, want market close", v.Actions[1])
	}
}

func TestTakeProfitLong(t *testing.T) {
	g := longGuard("140", "180")
	v := g.Check(strategy.BookEvent{
		Book: market.BookTicker{Bid: d("180"), Ask: d("180.2")},
		Mark: d("180.1"),
		Now:  t0.Add(time.Second),
	})
	if !v.Stop || v.Reason != strategy.StopTakeProfit {
		t.Fatalf("verdict = %+v", v)
	}
	if len(v.Actions) != 2 {
		t.Fatalf("tp actions = %d, want cancel + maker flatten", len(v.Actions))
	}
	if _, ok := v.Actions[1].(strategy.EnsurePosition); !ok {
		t.Fatalf("tp second action = %T, want EnsurePosition", v.Actions[1])
	}
}

func TestStopLossShort(t *testing.T) {
	p := strategy.DefaultRiskParams()
	p.StopLossPrice = d("160")
	g := New(p)
	g.SetPosition(position.Position{Symbol: "BTC", Size: d("-1"), MarkPrice: d("150")})
	g.SetMark(d("150"), market.BookTicker{Bid: d("149.9"), Ask: d("150.1")}, t0)
	v := g.Check(strategy.BookEvent{
		Book: market.BookTicker{Bid: d("160"), Ask: d("160.2")},
		Mark: d("160.5"),
		Now:  t0.Add(time.Second),
	})
	if !v.Stop || v.Reason != strategy.StopStopLoss {
		t.Fatalf("verdict = %+v", v)
	}
}

func TestFlatDoesNotTriggerTPSL(t *testing.T) {
	g := longGuard("140", "180")
	g.SetPosition(position.Position{Symbol: "BTC"})
	v := g.Check(strategy.BookEvent{
		Book: market.BookTicker{Bid: d("100"), Ask: d("100.2")},
		Mark: d("100"),
		Now:  t0.Add(time.Second),
	})
	if v.Stop {
		t.Fatal("flat position must not trigger tp/sl")
	}
}

func TestMaxNotionalBlocksOpens(t *testing.T) {
	p := strategy.DefaultRiskParams()
	p.MaxPositionNotional = d("100")
	g := New(p)
	g.SetPosition(position.Position{Symbol: "BTC", Size: d("1"), MarkPrice: d("150")})
	g.SetMark(d("150"), market.BookTicker{Bid: d("149.9"), Ask: d("150.1")}, t0)
	v := g.Check(strategy.TickEvent{Now: t0.Add(time.Second)})
	if v.Stop || !v.BlockOpens {
		t.Fatalf("verdict = %+v", v)
	}

	filtered := FilterOpens([]strategy.Action{
		strategy.PlaceOrder{Side: order.Buy, Quantity: d("1")},
		strategy.PlaceOrder{Side: order.Sell, Quantity: d("1")},
		strategy.PlaceOrder{ReduceOnly: true, Quantity: d("1")},
		strategy.CancelAll{},
	}, position.Position{Symbol: "BTC", Size: d("1"), MarkPrice: d("150")})
	if len(filtered) != 3 {
		t.Fatalf("filtered = %d, want 3 (flattening sell + reduce-only + cancel)", len(filtered))
	}
}

func TestCircuitOnConsecutiveFailures(t *testing.T) {
	p := strategy.DefaultRiskParams()
	p.MaxConsecutiveErrors = 3
	g := New(p)
	if v := g.RecordFailures(2, nil); v.Stop {
		t.Fatal("2 < 3 should not trip")
	}
	v := g.RecordFailures(1, nil)
	if !v.Stop || v.Reason != strategy.StopCircuit {
		t.Fatalf("verdict = %+v", v)
	}
	if _, ok := v.Actions[1].(strategy.ClosePosition); ok {
		t.Fatal("circuit must not close the position")
	}
}

func TestInsufficientMarginBlocksOpensNotCircuit(t *testing.T) {
	g := New(strategy.DefaultRiskParams())
	g.NoteInsufficientMargin()
	if !g.BlockOpens() {
		t.Fatal("insufficient margin should block opens")
	}
	if v := g.RecordFailures(0, nil); v.Stop {
		t.Fatal("clearing failures must not circuit")
	}
	if g.BlockOpens() {
		t.Fatal("successful apply should clear the margin block")
	}
}

func TestFatalTripsImmediately(t *testing.T) {
	g := New(strategy.DefaultRiskParams())
	v := g.RecordFailures(0, exchange.Classify(exchange.ClassFatal, "place", errors.New("bad key")))
	if !v.Stop || v.Reason != strategy.StopError {
		t.Fatalf("verdict = %+v", v)
	}
}

func TestStaleTriggersReconnect(t *testing.T) {
	p := strategy.DefaultRiskParams()
	p.StaleTimeout = strategy.MustParseDuration("10s")
	g := New(p)
	g.SetMark(d("150"), market.BookTicker{Bid: d("149.9"), Ask: d("150.1")}, t0)
	v := g.Check(strategy.TickEvent{Now: t0.Add(10 * time.Second)})
	if !v.Reconnect {
		t.Fatal("expected reconnect on stale market data")
	}
}

func TestMarginRatioBlocksOpens(t *testing.T) {
	p := strategy.DefaultRiskParams()
	p.MinMarginRatio = d("1.5")
	g := New(p)
	g.SetAccount(account.Snapshot{Equity: d("100"), MarginUsed: d("80")})
	g.SetMark(d("150"), market.BookTicker{Bid: d("149.9"), Ask: d("150.1")}, t0)
	v := g.Check(strategy.TickEvent{Now: t0})
	if !v.BlockOpens {
		t.Fatalf("ratio %s should block opens", d("100").Div(d("80")))
	}
}

func TestTPSLTriggerSidesMatrix(t *testing.T) {
	// 方向由触发边决定，与仓位正负无关。价格未穿越时不得 Stop。
	type tc struct {
		name            string
		sl, tp          string
		slBelow, tpAbove bool
		size, price     string
		wantStop        bool
		wantReason      strategy.StopReason
	}
	cases := []tc{
		{name: "long sl below hit", sl: "95", tp: "115", slBelow: true, tpAbove: true, size: "1", price: "94", wantStop: true, wantReason: strategy.StopStopLoss},
		{name: "long sl below miss", sl: "95", tp: "115", slBelow: true, tpAbove: true, size: "1", price: "105", wantStop: false},
		{name: "long tp above hit", sl: "95", tp: "115", slBelow: true, tpAbove: true, size: "1", price: "116", wantStop: true, wantReason: strategy.StopTakeProfit},
		{name: "short sl below miss at mid", sl: "95", tp: "115", slBelow: true, tpAbove: true, size: "-1", price: "105", wantStop: false},
		{name: "short sl below still hits downside", sl: "95", tp: "115", slBelow: true, tpAbove: true, size: "-1", price: "94", wantStop: true, wantReason: strategy.StopStopLoss},
		{name: "flat never", sl: "95", tp: "115", slBelow: true, tpAbove: true, size: "0", price: "90", wantStop: false},
		{name: "short sl above hit", sl: "115", tp: "95", slBelow: false, tpAbove: false, size: "-1", price: "116", wantStop: true, wantReason: strategy.StopStopLoss},
		{name: "short sl above miss", sl: "115", tp: "95", slBelow: false, tpAbove: false, size: "-1", price: "105", wantStop: false},
		{name: "long sl above would false-positive without sides", sl: "115", tp: "95", slBelow: false, tpAbove: false, size: "1", price: "105", wantStop: false},
		{name: "long tp below hit", sl: "115", tp: "95", slBelow: false, tpAbove: false, size: "1", price: "94", wantStop: true, wantReason: strategy.StopTakeProfit},
		{name: "neutral short in range", sl: "95", tp: "115", slBelow: true, tpAbove: true, size: "-1", price: "105", wantStop: false},
		{name: "neutral long in range", sl: "95", tp: "115", slBelow: true, tpAbove: true, size: "1", price: "105", wantStop: false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := strategy.DefaultRiskParams()
			p.StopLossPrice = d(c.sl)
			p.TakeProfitPrice = d(c.tp)
			g := New(p)
			g.SetTriggerSides(c.slBelow, c.tpAbove)
			g.SetPosition(position.Position{Symbol: "BTC", Size: d(c.size), MarkPrice: d("105")})
			g.SetMark(d("105"), market.BookTicker{Bid: d("104.9"), Ask: d("105.1")}, t0)
			v := g.Check(strategy.BookEvent{
				Book: market.BookTicker{Bid: d(c.price), Ask: d(c.price).Add(d("0.1"))},
				Mark: d(c.price),
				Now:  t0.Add(time.Second),
			})
			if v.Stop != c.wantStop {
				t.Fatalf("stop=%v reason=%s, want stop=%v", v.Stop, v.Reason, c.wantStop)
			}
			if c.wantStop && v.Reason != c.wantReason {
				t.Fatalf("reason=%s, want %s", v.Reason, c.wantReason)
			}
		})
	}
}
