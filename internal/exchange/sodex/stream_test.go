package sodex

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"dex-grid/internal/domain/order"
	"dex-grid/internal/exchange"

	"github.com/sodex-tech/sodex-go-sdk-public/client"
)

func TestHandleBookAndMark(t *testing.T) {
	a := &Adapter{
		log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		address:  "0xabc",
		bySymbol: map[string]cachedMarket{},
		byIndex:  map[int]cachedMarket{},
		tickers:  map[string]client.Ticker{},
	}
	s := &stream{
		adapter: a,
		log:     a.log,
		symbol:  "BTC-USD",
		out:     make(chan exchange.StreamEvent, 8),
	}
	book := `{"s":"BTC-USD","b":"80902","B":"0.6","a":"80903","A":"1.0","E":1788491693000}`
	if err := s.handleBookTicker(context.Background(), []byte(book)); err != nil {
		t.Fatal(err)
	}
	mark := `{"s":"BTC-USD","p":"80900","i":"80932","E":1788491693100}`
	if err := s.handleMarkPrice(context.Background(), []byte(mark)); err != nil {
		t.Fatal(err)
	}

	var got exchange.Ticker
	deadline := time.After(time.Second)
	for got.Mark.IsZero() {
		select {
		case ev := <-s.out:
			if ev.Ticker != nil {
				got = *ev.Ticker
			}
		case <-deadline:
			t.Fatal("timeout waiting ticker")
		}
	}
	if !got.Book.Valid() || !got.Mark.IsPositive() {
		t.Fatalf("incomplete ticker: %+v", got)
	}
}

func TestHandleOrderAndTrade(t *testing.T) {
	a := &Adapter{log: slog.New(slog.NewTextHandler(io.Discard, nil)), address: "0xabc"}
	s := &stream{adapter: a, log: a.log, symbol: "BTC-USD", out: make(chan exchange.StreamEvent, 8)}

	coid := order.MustEncode(order.Ref{Slot: 2, Epoch: 1, Cell: 0, Purpose: order.PurposeOpen, Seq: 0})
	orderJSON := `{"s":"BTC-USD","c":"` + clOrdIDString(coid) + `","i":42,"S":"BUY","o":"LIMIT","p":"80000","q":"0.001","X":"NEW","z":"0","m":"true"}`
	if err := s.handleOrderUpdate(context.Background(), []byte(orderJSON)); err != nil {
		t.Fatal(err)
	}
	tradeJSON := `{"s":"BTC-USD","c":"` + clOrdIDString(coid) + `","t":7,"i":42,"S":"BUY","p":"80001","q":"0.001","f":"0.01","m":"1"}`
	if err := s.handleTrade(context.Background(), []byte(tradeJSON)); err != nil {
		t.Fatal(err)
	}

	var sawOrder, sawTrade bool
	deadline := time.After(time.Second)
	for !sawOrder || !sawTrade {
		select {
		case ev := <-s.out:
			if ev.Order != nil {
				sawOrder = true
				if ev.Order.ClientOrderID != coid {
					t.Fatalf("coid %d", ev.Order.ClientOrderID)
				}
			}
			if ev.Trade != nil {
				sawTrade = true
				if !ev.Trade.IsMaker {
					t.Fatal("expected maker fill")
				}
			}
		case <-deadline:
			t.Fatalf("order=%v trade=%v", sawOrder, sawTrade)
		}
	}
}

func TestHandleOrderNumericClOrdIDAndWrapper(t *testing.T) {
	a := &Adapter{log: slog.New(slog.NewTextHandler(io.Discard, nil)), address: "0xabc"}
	s := &stream{adapter: a, log: a.log, symbol: "BTC-USD", out: make(chan exchange.StreamEvent, 8)}
	coid := order.MustEncode(order.Ref{Slot: 2, Epoch: 1, Cell: 0, Purpose: order.PurposeOpen, Seq: 0})
	wrapped := `{"o":{"s":"BTC-USD","c":` + clOrdIDString(coid) + `,"i":"99","S":"BUY","X":"FILLED","z":"0.001","q":"0.001","p":"80000","m":"1"}}`
	if err := s.handleOrderUpdate(context.Background(), []byte(wrapped)); err != nil {
		t.Fatal(err)
	}
	ev := <-s.out
	if ev.Order == nil {
		t.Fatal("expected order event")
	}
	if ev.Order.ClientOrderID != coid {
		t.Fatalf("coid %d want %d", ev.Order.ClientOrderID, coid)
	}
	if ev.Order.State != order.StateFilled {
		t.Fatalf("state %s", ev.Order.State)
	}
	if ev.Order.ExchangeID != "99" {
		t.Fatalf("exchange id %s", ev.Order.ExchangeID)
	}
}

func TestHandleEnvelopePing(t *testing.T) {
	s := &stream{out: make(chan exchange.StreamEvent, 1), symbol: "BTC-USD", log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if err := s.handle(context.Background(), []byte(`{"op":"pong"}`)); err != nil {
		t.Fatal(err)
	}
}
