package executor

import (
	"context"
	"testing"
	"time"

	"dex-grid/internal/domain/market"
	"dex-grid/internal/domain/order"
	"dex-grid/internal/domain/position"
	"dex-grid/internal/domain/strategy"
	"dex-grid/internal/exchange"
	"dex-grid/internal/exchange/fake"

	"github.com/shopspring/decimal"
)

func d(s string) decimal.Decimal { return decimal.RequireFromString(s) }

func testMarket() market.Market {
	return market.Market{
		Symbol:        "BTC",
		TickSize:      d("0.1"),
		LotSize:       d("0.001"),
		MinQty:        d("0.001"),
		MinNotional:   d("10"),
		MaxLeverage:   50,
		PriceDecimals: 1,
		SizeDecimals:  3,
	}
}

func coid(cell uint16, seq uint8) order.ClientOrderID {
	return order.MustEncode(order.Ref{
		Slot: 0, Epoch: 1, Cell: cell, Purpose: order.PurposeOpen, Seq: seq,
	})
}

func newExec(ex exchange.Exchange) *Executor {
	return New(ex, Options{
		Symbol:     "BTC",
		Slot:       0,
		Epoch:      1,
		MaxRetries: 2,
		RetryWait:  time.Millisecond,
		Sleep:      func(context.Context, time.Duration) error { return nil },
	})
}

func TestApplyPlacesAndCountsProgress(t *testing.T) {
	ex := fake.New(testMarket())
	ex.SetBook(d("149.9"), d("150.1"))
	ex.SetMark(d("150"))
	e := newExec(ex)

	res := e.Apply(context.Background(), []strategy.Action{
		strategy.SetLeverage{Leverage: 5, Mode: market.MarginIsolated},
		strategy.PlaceOrder{
			ClientOrderID: coid(0, 0), Side: order.Buy, Type: order.Limit,
			Price: d("125"), Quantity: d("1"), TIF: order.PostOnly,
		},
		strategy.PlaceOrder{
			ClientOrderID: coid(1, 0), Side: order.Sell, Type: order.Limit,
			Price: d("175"), Quantity: d("1"), TIF: order.PostOnly,
		},
	})
	if res.Fatal != nil {
		t.Fatal(res.Fatal)
	}
	if res.Failures != 0 {
		t.Fatalf("failures = %d", res.Failures)
	}
	p := e.Progress()
	if p.Target != 2 || p.Confirmed != 2 || p.Retrying != 0 {
		t.Fatalf("progress = %+v", p)
	}
	if lev, _ := ex.Leverage(); lev != 5 {
		t.Fatalf("leverage = %d", lev)
	}
	if n := len(ex.Resting()); n != 2 {
		t.Fatalf("resting = %d", n)
	}
}

func TestApplyRetriesThenSucceeds(t *testing.T) {
	ex := fake.New(testMarket())
	ex.SetBook(d("149.9"), d("150.1"))
	ex.FailNextPlaces(1)
	e := newExec(ex)

	res := e.Apply(context.Background(), []strategy.Action{
		strategy.PlaceOrder{
			ClientOrderID: coid(0, 0), Side: order.Buy, Type: order.Limit,
			Price: d("125"), Quantity: d("1"), TIF: order.PostOnly,
		},
	})
	if res.Fatal != nil {
		t.Fatal(res.Fatal)
	}
	if e.Progress().Confirmed != 1 {
		t.Fatalf("confirmed = %d after retry", e.Progress().Confirmed)
	}
	if e.ConsecutiveFails() != 0 {
		t.Fatalf("fails should reset after success, got %d", e.ConsecutiveFails())
	}
}

func TestApplyCancelBeforePlace(t *testing.T) {
	ex := fake.New(testMarket())
	ex.SetBook(d("149.9"), d("150.1"))
	e := newExec(ex)
	old := coid(0, 0)
	e.Apply(context.Background(), []strategy.Action{
		strategy.PlaceOrder{
			ClientOrderID: old, Side: order.Buy, Type: order.Limit,
			Price: d("120"), Quantity: d("1"), TIF: order.PostOnly,
		},
	})

	res := e.Apply(context.Background(), []strategy.Action{
		strategy.CancelOrder{ClientOrderID: old},
		strategy.PlaceOrder{
			ClientOrderID: coid(0, 1), Side: order.Buy, Type: order.Limit,
			Price: d("125"), Quantity: d("1"), TIF: order.PostOnly,
		},
	})
	if res.Fatal != nil {
		t.Fatal(res.Fatal)
	}
	resting := ex.Resting()
	if len(resting) != 1 || resting[0].ClientOrderID != coid(0, 1) {
		t.Fatalf("resting = %+v", resting)
	}
}

func TestApplyExtractsEnsureAndStop(t *testing.T) {
	ex := fake.New(testMarket())
	e := newExec(ex)
	res := e.Apply(context.Background(), []strategy.Action{
		strategy.EnsurePosition{Target: d("2")},
		strategy.Stop{Reason: strategy.StopManual},
	})
	if res.Ensure == nil || !res.Ensure.Target.Equal(d("2")) {
		t.Fatalf("ensure = %+v", res.Ensure)
	}
	if res.Stop == nil || res.Stop.Reason != strategy.StopManual {
		t.Fatalf("stop = %+v", res.Stop)
	}
}

func TestClosePositionMarket(t *testing.T) {
	ex := fake.New(testMarket())
	ex.SetBook(d("149.9"), d("150.1"))
	ex.SetMark(d("150"))
	ex.SetPosition(position.Position{Symbol: "BTC", Size: d("2"), MarkPrice: d("150")})
	e := newExec(ex)

	res := e.Apply(context.Background(), []strategy.Action{
		strategy.ClosePosition{Urgency: strategy.UrgencyMarket},
	})
	if res.Fatal != nil {
		t.Fatal(res.Fatal)
	}
	pos, _ := ex.Position(context.Background(), "BTC")
	if !pos.IsFlat() {
		t.Fatalf("position still %s", pos.Size)
	}
}

func TestInvalidParamEmitsRejectedAndCountsFailure(t *testing.T) {
	ex := fake.New(testMarket())
	ex.SetBook(d("149.9"), d("150.1"))
	e := newExec(ex)
	res := e.Apply(context.Background(), []strategy.Action{
		strategy.PlaceOrder{
			ClientOrderID: coid(0, 0), Side: order.Buy, Type: order.Limit,
			Price: d("125"), Quantity: d("0.0001"), TIF: order.PostOnly, // below min qty
		},
	})
	if res.Failures != 1 {
		t.Fatalf("failures = %d", res.Failures)
	}
	if len(res.Events) != 1 {
		t.Fatalf("events = %d", len(res.Events))
	}
	oe, ok := res.Events[0].(strategy.OrderEvent)
	if !ok || oe.Order.State != order.StateRejected {
		t.Fatalf("event = %+v", res.Events[0])
	}
}
