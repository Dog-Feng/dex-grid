package sodex

import (
	"encoding/json"
	"testing"

	"dex-grid/internal/domain/order"
	"dex-grid/internal/domain/position"

	"github.com/shopspring/decimal"
	"github.com/sodex-tech/sodex-go-sdk-public/client"
	"github.com/sodex-tech/sodex-go-sdk-public/common/enums"
)

func TestFlexBool(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{`"true"`, true},
		{`true`, true},
		{`"1"`, true},
		{`1`, true},
		{`"false"`, false},
		{`false`, false},
		{`"0"`, false},
		{`"4.968"`, false},
		{`4.968`, false},
	}
	for _, c := range cases {
		var b flexBool
		if err := json.Unmarshal([]byte(c.in), &b); err != nil {
			t.Fatalf("%s: %v", c.in, err)
		}
		if bool(b) != c.want {
			t.Fatalf("%s: got %v want %v", c.in, b, c.want)
		}
	}
}

func TestFlexStringAndInt(t *testing.T) {
	var s flexString
	if err := json.Unmarshal([]byte(`4398046511105`), &s); err != nil {
		t.Fatal(err)
	}
	if s.String() != "4398046511105" {
		t.Fatalf("got %q", s)
	}
	if err := json.Unmarshal([]byte(`"4398046511105"`), &s); err != nil {
		t.Fatal(err)
	}
	if parseClOrdID(s.String()) == 0 {
		t.Fatal("quoted clOrdID should parse")
	}
	var n flexInt64
	if err := json.Unmarshal([]byte(`"42"`), &n); err != nil {
		t.Fatal(err)
	}
	if n.Int64() != 42 {
		t.Fatalf("got %d", n)
	}
}

func TestPlaceRejectedIOCExpired(t *testing.T) {
	expired := client.PlaceOrderResult{Status: "EXPIRED"}
	if !placeRejected(expired, false) {
		t.Fatal("GTX EXPIRED should be rejected")
	}
	if placeRejected(expired, true) {
		t.Fatal("IOC EXPIRED should not be treated as a hard reject")
	}
	if !placeRejected(client.PlaceOrderResult{Status: "REJECTED"}, true) {
		t.Fatal("REJECTED stays rejected even for IOC")
	}
}

func TestClOrdIDRoundTrip(t *testing.T) {
	id := order.MustEncode(order.Ref{Slot: 2, Epoch: 10, Cell: 3, Purpose: order.PurposeOpen, Seq: 1})
	s := clOrdIDString(id)
	if got := parseClOrdID(s); got != id {
		t.Fatalf("roundtrip %s: got %d want %d", s, got, id)
	}
	if parseClOrdID("not-a-number") != 0 {
		t.Fatal("non-numeric clOrdID should be 0")
	}
}

func TestToTIF(t *testing.T) {
	tif, err := toTIF(order.PostOnly)
	if err != nil || tif != enums.TimeInForceGTX {
		t.Fatalf("post-only → GTX, got %v %v", tif, err)
	}
	tif, err = toTIF(order.IOC)
	if err != nil || tif != enums.TimeInForceIOC {
		t.Fatalf("ioc → IOC, got %v %v", tif, err)
	}
}

func TestStateFromStatus(t *testing.T) {
	qty := decimal.NewFromInt(1)
	half := decimal.RequireFromString("0.5")
	cases := []struct {
		status, reason string
		filled, total  decimal.Decimal
		want           order.State
	}{
		{"NEW", "", decimal.Zero, qty, order.StateOpen},
		{"PARTIALLY_FILLED", "", half, qty, order.StatePartiallyFilled},
		{"FILLED", "", qty, qty, order.StateFilled},
		{"CANCELED", "", decimal.Zero, qty, order.StateCanceled},
		{"REJECTED", "post-only would match", decimal.Zero, qty, order.StateRejected},
		{"REJECTED", "GTX remainder", half, qty, order.StateCanceled},
		{"EXPIRED", "", decimal.Zero, qty, order.StateExpired},
	}
	for _, c := range cases {
		got, _ := stateFromStatus(c.status, c.filled, c.total, c.reason)
		if got != c.want {
			t.Errorf("%s filled=%s: got %s want %s", c.status, c.filled, got, c.want)
		}
	}
}

func TestToMarket(t *testing.T) {
	lev := 40
	m := toMarket(client.Symbol{
		SymbolID:          1,
		Symbol:            "BTC-USD",
		Status:            "TRADING",
		TickSize:          "0.1",
		StepSize:          "0.00001",
		MinQuantity:       "0.00001",
		MinNotional:       "10",
		MaxLeverage:       &lev,
		PricePrecision:    1,
		QuantityPrecision: 5,
		MakerFee:          "0.00012",
		TakerFee:          "0.0004",
	})
	if m.Symbol != "BTC-USD" || !m.TickSize.Equal(decimal.RequireFromString("0.1")) {
		t.Fatalf("unexpected market: %+v", m)
	}
	if m.MaxLeverage != 40 {
		t.Fatalf("max leverage %d", m.MaxLeverage)
	}
	if !m.MakerFeeRate.Equal(decimal.RequireFromString("0.00012")) {
		t.Fatalf("maker fee %s", m.MakerFeeRate)
	}
}

func TestToPositionShort(t *testing.T) {
	p := toPosition(client.Position{
		Symbol:        "SOL-USD",
		PositionSide:  "SHORT",
		Size:          "1.5",
		AvgEntryPrice: "100",
		Leverage:      10,
		MarginMode:    "ISOLATED",
	}, decimal.NewFromInt(90))
	if p.Direction() != position.Short {
		t.Fatalf("want short, got %s size=%s", p.Direction(), p.Size)
	}
	// 空 1.5 @100，标记 90 → 浮盈 +15
	want := decimal.NewFromInt(15)
	if !p.UnrealizedPnL.Equal(want) {
		t.Fatalf("upnl %s want %s", p.UnrealizedPnL, want)
	}
}

func TestToOrderFromWS(t *testing.T) {
	o := toOrderFromWS(wsAccountOrderUpdate{
		ClOrdID:   "4398046511105",
		OrderID:   99,
		Symbol:    "BTC-USD",
		Side:      "BUY",
		OrderType: "LIMIT",
		Price:     "80000",
		OrigQty:   "0.001",
		FilledQty: "0",
		Status:    "NEW",
	}, "BTC-USD")
	if o.Side != order.Buy || o.State != order.StateOpen {
		t.Fatalf("unexpected order: %+v", o)
	}
	if o.ExchangeID != "99" {
		t.Fatalf("exchange id %s", o.ExchangeID)
	}
}

func TestIsActiveStatus(t *testing.T) {
	if !isActiveStatus("TRADING") || isActiveStatus("HALT") {
		t.Fatal("TRADING should be active, HALT should not")
	}
}

func TestKlineInterval(t *testing.T) {
	got, ok := klineInterval("1h")
	if !ok || got != "1h" {
		t.Fatalf("1h → %s %v", got, ok)
	}
	got, ok = klineInterval("1d")
	if !ok || got != "1D" {
		t.Fatalf("1d → %s %v", got, ok)
	}
	if _, ok := klineInterval("7m"); ok {
		t.Fatal("7m should be rejected")
	}
}
