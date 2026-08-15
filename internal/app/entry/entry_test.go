package entry

import (
	"testing"
	"time"

	"dex-grid/internal/domain/market"
	"dex-grid/internal/domain/order"
	"dex-grid/internal/domain/strategy"

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

func book(bid, ask string) market.BookTicker {
	return market.BookTicker{Bid: d(bid), Ask: d(ask)}
}

var t0 = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func firstPlace(t *testing.T, acts []strategy.Action) strategy.PlaceOrder {
	t.Helper()
	for _, a := range acts {
		if p, ok := a.(strategy.PlaceOrder); ok {
			return p
		}
	}
	t.Fatalf("no PlaceOrder in %d actions", len(acts))
	return strategy.PlaceOrder{}
}

func TestMarketEntryFillsInOneShot(t *testing.T) {
	p := strategy.DefaultEntryParams()
	p.Mode = strategy.EntryMarket
	p.SliceCount = 1
	tr := New(p, testMarket(), 0, 1)

	acts := tr.Start(d("2"), d("0"), book("149.9", "150.1"), d("150"), t0)
	po := firstPlace(t, acts)
	if po.Type != order.Market || po.Side != order.Buy {
		t.Fatalf("got %+v", po)
	}
	if !po.Quantity.Equal(d("2")) {
		t.Fatalf("qty = %s", po.Quantity)
	}

	_, done, failed := tr.OnEvent(strategy.OrderEvent{
		Order: order.Order{
			ClientOrderID: po.ClientOrderID,
			Side:          order.Buy,
			FilledQty:     d("2"),
			Quantity:      d("2"),
			State:         order.StateFilled,
		},
		Now: t0,
	})
	if failed || !done {
		t.Fatalf("done=%v failed=%v reason=%s", done, failed, tr.reason)
	}
	filled, _ := tr.Result()
	if !filled.Equal(d("2")) {
		t.Fatalf("filled = %s", filled)
	}
}

func TestMarketEntrySlices(t *testing.T) {
	p := strategy.DefaultEntryParams()
	p.Mode = strategy.EntryMarket
	p.SliceCount = 2
	p.SliceInterval = strategy.MustParseDuration("1s")
	tr := New(p, testMarket(), 0, 1)

	acts := tr.Start(d("2"), d("0"), book("149.9", "150.1"), d("150"), t0)
	po := firstPlace(t, acts)
	if !po.Quantity.Equal(d("1")) {
		t.Fatalf("first slice qty = %s, want 1", po.Quantity)
	}

	acts, done, _ := tr.OnEvent(strategy.OrderEvent{
		Order: order.Order{
			ClientOrderID: po.ClientOrderID, Side: order.Buy,
			FilledQty: d("1"), Quantity: d("1"), State: order.StateFilled,
		},
		Now: t0,
	})
	if done {
		t.Fatal("should wait for second slice")
	}
	if len(acts) != 0 {
		t.Fatal("second slice must wait for slice interval")
	}

	acts, done, _ = tr.OnEvent(strategy.TickEvent{Now: t0.Add(time.Second)})
	if done {
		t.Fatal("second slice just placed")
	}
	po2 := firstPlace(t, acts)
	if !po2.Quantity.Equal(d("1")) {
		t.Fatalf("second slice qty = %s", po2.Quantity)
	}
}

func TestMakerFollowReprices(t *testing.T) {
	p := strategy.DefaultEntryParams()
	p.Mode = strategy.EntryMakerFollow
	p.RepriceTicks = 1
	p.RepriceInterval = 0
	tr := New(p, testMarket(), 0, 1)

	acts := tr.Start(d("1"), d("0"), book("149.9", "150.1"), d("150"), t0)
	po := firstPlace(t, acts)
	if po.TIF != order.PostOnly || !po.Price.Equal(d("149.9")) {
		t.Fatalf("follow price = %s tif = %s", po.Price, po.TIF)
	}

	tr.OnEvent(strategy.OrderEvent{
		Order: order.Order{ClientOrderID: po.ClientOrderID, State: order.StateOpen, Quantity: d("1")},
		Now:   t0,
	})

	acts, _, _ = tr.OnEvent(strategy.BookEvent{
		Book: book("149.7", "149.9"),
		Mark: d("149.8"),
		Now:  t0.Add(time.Second),
	})
	if len(acts) != 1 {
		t.Fatalf("expected cancel, got %d actions", len(acts))
	}
	if _, ok := acts[0].(strategy.CancelOrder); !ok {
		t.Fatalf("expected CancelOrder, got %T", acts[0])
	}

	acts, _, _ = tr.OnEvent(strategy.OrderEvent{
		Order: order.Order{ClientOrderID: po.ClientOrderID, State: order.StateCanceled, Quantity: d("1")},
		Now:   t0.Add(time.Second),
	})
	po2 := firstPlace(t, acts)
	if !po2.Price.Equal(d("149.7")) {
		t.Fatalf("repriced to %s, want 149.7", po2.Price)
	}
}

func TestLimitPriceWaitsAndAbortsOnTimeout(t *testing.T) {
	p := strategy.DefaultEntryParams()
	p.Mode = strategy.EntryLimitPrice
	p.Price = d("140")
	p.Timeout = strategy.MustParseDuration("5s")
	p.OnTimeout = strategy.TimeoutAbort
	tr := New(p, testMarket(), 0, 1)

	acts := tr.Start(d("1"), d("0"), book("149.9", "150.1"), d("150"), t0)
	po := firstPlace(t, acts)
	if !po.Price.Equal(d("140")) || po.TIF != order.PostOnly {
		t.Fatalf("limit order = %+v", po)
	}
	tr.OnEvent(strategy.OrderEvent{
		Order: order.Order{ClientOrderID: po.ClientOrderID, State: order.StateOpen, Quantity: d("1")},
		Now:   t0,
	})

	acts, done, failed := tr.OnEvent(strategy.TickEvent{Now: t0.Add(5 * time.Second)})
	if done || !failed {
		t.Fatalf("done=%v failed=%v", done, failed)
	}
	if len(acts) != 1 {
		t.Fatalf("expected cancel on abort, got %d", len(acts))
	}
}

func TestAlreadyAtTargetIsDone(t *testing.T) {
	tr := New(strategy.DefaultEntryParams(), testMarket(), 0, 1)
	acts := tr.Start(d("2"), d("2"), book("149.9", "150.1"), d("150"), t0)
	if len(acts) != 0 {
		t.Fatalf("expected no actions, got %d", len(acts))
	}
	if tr.phase != PhaseDone {
		t.Fatalf("phase = %s", tr.phase)
	}
}

func TestShortEntrySells(t *testing.T) {
	p := strategy.DefaultEntryParams()
	p.Mode = strategy.EntryMarket
	tr := New(p, testMarket(), 0, 1)
	acts := tr.Start(d("-2"), d("0"), book("149.9", "150.1"), d("150"), t0)
	po := firstPlace(t, acts)
	if po.Side != order.Sell {
		t.Fatalf("side = %s, want sell", po.Side)
	}
}

func TestPartialFillAccumulates(t *testing.T) {
	p := strategy.DefaultEntryParams()
	p.Mode = strategy.EntryLimitPrice
	p.Price = d("140")
	p.FillTolerance = d("0.01")
	tr := New(p, testMarket(), 0, 1)
	acts := tr.Start(d("1"), d("0"), book("149.9", "150.1"), d("150"), t0)
	po := firstPlace(t, acts)

	_, done, _ := tr.OnEvent(strategy.OrderEvent{
		Order: order.Order{
			ClientOrderID: po.ClientOrderID, Side: order.Buy,
			FilledQty: d("0.4"), Quantity: d("1"), State: order.StatePartiallyFilled,
		},
		Now: t0,
	})
	if done {
		t.Fatal("0.4/1 should not finish")
	}
	_, done, _ = tr.OnEvent(strategy.OrderEvent{
		Order: order.Order{
			ClientOrderID: po.ClientOrderID, Side: order.Buy,
			FilledQty: d("1"), Quantity: d("1"), State: order.StateFilled,
		},
		Now: t0,
	})
	if !done {
		t.Fatal("full fill should finish")
	}
	filled, _ := tr.Result()
	if !filled.Equal(d("1")) {
		t.Fatalf("filled = %s (double-counted?)", filled)
	}
}
