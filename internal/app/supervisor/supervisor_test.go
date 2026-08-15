package supervisor

import (
	"context"
	"encoding/json"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"dex-grid/internal/app/engine"
	"dex-grid/internal/config"
	"dex-grid/internal/domain/market"
	"dex-grid/internal/exchange/fake"
	"dex-grid/internal/infra/store"

	"github.com/shopspring/decimal"
)

func d(s string) decimal.Decimal { return decimal.RequireFromString(s) }

func TestStartStopNeutralGrid(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "gridbot.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	cfg := &config.Config{}
	sup := New(cfg, st, nil, slog.Default())
	ex := fake.New(market.Market{
		Symbol: "BTC", TickSize: d("0.1"), LotSize: d("0.001"),
		MinQty: d("0.001"), MinNotional: d("10"), MaxLeverage: 50,
		PriceDecimals: 1, SizeDecimals: 3,
		MakerFeeRate: d("0.0002"), TakerFeeRate: d("0.0005"),
	})
	ex.SetBook(d("149.9"), d("150.1"))
	ex.SetMark(d("150"))
	sup.Attach(config.Exchange{Name: "fake", Enabled: true, MaxRetries: 2}, ex, 0)
	t.Cleanup(func() { sup.Close(context.Background()) })

	params := map[string]any{
		"symbol": "BTC", "direction": "neutral", "leverage": 5,
		"grid": map[string]any{
			"lower_price": "100", "upper_price": "200", "grid_count": 4,
			"sizing_mode": "per_grid_qty", "per_grid_qty": "1",
		},
		"entry": map[string]any{"mode": "market", "slice_count": 1},
	}
	raw, _ := json.Marshal(params)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if _, err := sup.PutConfig(ctx, "fake", raw); err != nil {
		t.Fatal(err)
	}
	view, err := sup.Start(ctx, "fake")
	if err != nil {
		t.Fatal(err)
	}
	if view.Status != engine.StatusRunning.String() && view.Status != engine.StatusStarting.String() {
		t.Fatalf("status after start = %s", view.Status)
	}
	// 给事件流一点时间确认挂单。
	time.Sleep(50 * time.Millisecond)
	if n := len(ex.Resting()); n == 0 {
		t.Fatal("expected grid orders after start")
	}

	view, err = sup.Stop(ctx, "fake")
	if err != nil {
		t.Fatal(err)
	}
	if view.Status != engine.StatusStopped.String() {
		t.Fatalf("status after stop = %s", view.Status)
	}
}
