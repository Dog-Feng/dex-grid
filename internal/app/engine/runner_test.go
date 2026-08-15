package engine

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"dex-grid/internal/domain/market"
	"dex-grid/internal/domain/strategy"
	"dex-grid/internal/domain/strategy/grid"
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
		MakerFeeRate:  d("0.0002"),
		TakerFeeRate:  d("0.0005"),
	}
}

func smallParams(dir grid.Direction) grid.Params {
	p := grid.DefaultParams()
	p.Symbol = "BTC"
	p.Direction = dir
	p.Leverage = 5
	p.Grid.LowerPrice = d("100")
	p.Grid.UpperPrice = d("200")
	p.Grid.GridCount = 4
	p.Grid.SizingMode = grid.PerGridQty
	p.Grid.PerGridQty = d("1")
	p.Entry.Mode = strategy.EntryMarket
	p.Entry.SliceCount = 1
	p.Entry.Timeout = strategy.MustParseDuration("1m")
	p.ApplyDefaults()
	return p
}

func newRunner(t *testing.T, dir grid.Direction) (*Runner, *fake.Exchange) {
	t.Helper()
	ex := fake.New(testMarket())
	ex.SetBook(d("149.9"), d("150.1"))
	ex.SetMark(d("150"))

	raw, err := json.Marshal(smallParams(dir))
	if err != nil {
		t.Fatal(err)
	}
	s, err := grid.New(raw)
	if err != nil {
		t.Fatal(err)
	}
	r := New(ex, s, Config{Name: "fake", Slot: 0, TickInterval: time.Second, MaxRetries: 2})
	return r, ex
}

func startOK(t *testing.T, r *Runner, risk strategy.RiskParams) {
	t.Helper()
	ctx := context.Background()
	res := r.Do(ctx, CmdStart, StartPayload{
		Symbol: "BTC",
		Entry:  strategy.DefaultEntryParams(),
		Risk:   risk,
	})
	if !res.OK {
		t.Fatalf("start: %s", res.Message)
	}
	// 覆盖成市价建仓，避免跟价单在测试里等盘口。
	r.entryP.Mode = strategy.EntryMarket
	r.entryP.SliceCount = 1
	r.Drain(ctx)
}

func TestNeutralStartPlacesGrid(t *testing.T) {
	r, ex := newRunner(t, grid.Neutral)
	startOK(t, r, strategy.DefaultRiskParams())
	r.Drain(context.Background())

	if r.Status() != StatusRunning {
		t.Fatalf("status = %s", r.Status())
	}
	if n := len(ex.Resting()); n != 4 {
		t.Fatalf("resting = %d, want 4", n)
	}
	if r.View().Strategy.Phase != strategy.PhaseRunning {
		t.Fatalf("phase = %s", r.View().Strategy.Phase)
	}
}

func TestLongStartEntersThenPlaces(t *testing.T) {
	r, ex := newRunner(t, grid.Long)
	p := strategy.DefaultRiskParams()
	res := r.Do(context.Background(), CmdStart, StartPayload{
		Symbol: "BTC",
		Entry: strategy.EntryParams{
			Mode:          strategy.EntryMarket,
			SliceCount:    1,
			FillTolerance: d("0.01"),
			MaxSlippage:   d("0.01"),
		},
		Risk: p,
	})
	if !res.OK {
		t.Fatalf("start: %s", res.Message)
	}
	r.Drain(context.Background())

	pos, _ := ex.Position(context.Background(), "BTC")
	if !pos.Size.Equal(d("2")) {
		t.Fatalf("position after entry = %s, want 2", pos.Size)
	}
	if n := len(ex.Resting()); n != 4 {
		t.Fatalf("grid resting = %d, want 4", n)
	}
	if r.Status() != StatusRunning {
		t.Fatalf("status = %s", r.Status())
	}
}

func TestFillPairsOppositeOrder(t *testing.T) {
	r, ex := newRunner(t, grid.Long)
	res := r.Do(context.Background(), CmdStart, StartPayload{
		Symbol: "BTC",
		Entry: strategy.EntryParams{
			Mode:          strategy.EntryMarket,
			SliceCount:    1,
			FillTolerance: d("0.01"),
			MaxSlippage:   d("0.01"),
		},
		Risk: strategy.DefaultRiskParams(),
	})
	if !res.OK {
		t.Fatalf("start: %s", res.Message)
	}
	r.Drain(context.Background())
	before := len(ex.Resting())

	// 击中 125 的买单，该格应翻转为在 150 挂卖。
	ex.SetBook(d("124.9"), d("125.1"))
	ex.SetMark(d("125"))
	ex.Trade(d("125"))
	r.Drain(context.Background())

	after := len(ex.Resting())
	if after < before-1 {
		t.Fatalf("resting before=%d after=%d, pairing should replace the filled order", before, after)
	}
	if r.View().Strategy.Stats.Fills < 1 {
		t.Fatal("expected at least one fill")
	}
}

func TestStopLossClosesAndStops(t *testing.T) {
	r, ex := newRunner(t, grid.Long)
	riskP := strategy.DefaultRiskParams()
	riskP.StopLossPrice = d("140")
	res := r.Do(context.Background(), CmdStart, StartPayload{
		Symbol: "BTC",
		Entry: strategy.EntryParams{
			Mode:          strategy.EntryMarket,
			SliceCount:    1,
			FillTolerance: d("0.01"),
			MaxSlippage:   d("0.01"),
		},
		Risk: riskP,
	})
	if !res.OK {
		t.Fatalf("start: %s", res.Message)
	}
	r.Drain(context.Background())

	ex.SetBook(d("138.9"), d("139.1"))
	ex.SetMark(d("139"))
	r.Drain(context.Background())

	if r.Status() != StatusStopped && r.Status() != StatusError {
		t.Fatalf("status = %s, want stopped", r.Status())
	}
	if r.View().StopReason != strategy.StopStopLoss.String() {
		t.Fatalf("reason = %s", r.View().StopReason)
	}
	pos, _ := ex.Position(context.Background(), "BTC")
	if !pos.IsFlat() {
		t.Fatalf("position should be flat after stop-loss, got %s", pos.Size)
	}
	if n := len(ex.Resting()); n != 0 {
		t.Fatalf("resting = %d after stop", n)
	}
}

func TestCancelOrdersPauses(t *testing.T) {
	r, ex := newRunner(t, grid.Neutral)
	startOK(t, r, strategy.DefaultRiskParams())
	r.Drain(context.Background())

	res := r.Do(context.Background(), CmdCancelOrders, nil)
	if !res.OK {
		t.Fatalf("cancel: %s", res.Message)
	}
	if r.Status() != StatusPaused {
		t.Fatalf("status = %s, want paused", r.Status())
	}
	if n := len(ex.Resting()); n != 0 {
		t.Fatalf("resting = %d after cancel", n)
	}

	res = r.Do(context.Background(), CmdRefill, nil)
	if !res.OK {
		t.Fatalf("refill: %s", res.Message)
	}
	r.Drain(context.Background())
	if r.Status() != StatusRunning {
		t.Fatalf("status = %s after refill", r.Status())
	}
	if n := len(ex.Resting()); n != 4 {
		t.Fatalf("resting = %d after refill", n)
	}
}

func TestManualStopKeepsPositionAndCancelsOrders(t *testing.T) {
	r, ex := newRunner(t, grid.Long)
	res := r.Do(context.Background(), CmdStart, StartPayload{
		Symbol: "BTC",
		Entry: strategy.EntryParams{
			Mode:          strategy.EntryMarket,
			SliceCount:    1,
			FillTolerance: d("0.01"),
			MaxSlippage:   d("0.01"),
		},
		Risk: strategy.DefaultRiskParams(),
	})
	if !res.OK {
		t.Fatalf("start: %s", res.Message)
	}
	r.Drain(context.Background())
	before, _ := ex.Position(context.Background(), "BTC")
	if before.IsFlat() {
		t.Fatal("expected a long position before stop")
	}

	res = r.Do(context.Background(), CmdStop, nil)
	if !res.OK {
		t.Fatalf("stop: %s", res.Message)
	}
	if r.Status() != StatusStopped {
		t.Fatalf("status = %s", r.Status())
	}
	if n := len(ex.Resting()); n != 0 {
		t.Fatalf("resting = %d after stop, want 0", n)
	}
	pos, _ := ex.Position(context.Background(), "BTC")
	if !pos.Size.Equal(before.Size) {
		t.Fatalf("stop must keep position %s, got %s", before.Size, pos.Size)
	}
}
