package store

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"dex-grid/internal/domain/order"
)

func TestConfigAndRuntimeRoundTrip(t *testing.T) {
	s := openTemp(t)
	raw := json.RawMessage(`{"symbol":"BTC","leverage":5}`)
	if err := s.SaveConfig(Config{
		Exchange: "lighter", Symbol: "BTC", Strategy: "grid",
		Direction: "long", Params: raw,
	}); err != nil {
		t.Fatal(err)
	}
	got, ok, err := s.LoadConfig("lighter")
	if err != nil || !ok {
		t.Fatalf("load config: ok=%v err=%v", ok, err)
	}
	if got.Symbol != "BTC" || got.Direction != "long" || string(got.Params) != string(raw) {
		t.Fatalf("got %+v", got)
	}
	if _, ok, _ = s.LoadConfig("missing"); ok {
		t.Fatal("missing config should not exist")
	}

	if err := s.SaveRuntime(Runtime{
		Exchange: "lighter", Status: "running", Epoch: 3, Snapshot: []byte(`{"epoch":3}`),
	}); err != nil {
		t.Fatal(err)
	}
	rt, ok, err := s.LoadRuntime("lighter")
	if err != nil || !ok {
		t.Fatalf("load runtime: ok=%v err=%v", ok, err)
	}
	if rt.Status != "running" || rt.Epoch != 3 || string(rt.Snapshot) != `{"epoch":3}` {
		t.Fatalf("got %+v", rt)
	}
}

func TestFillsRespectResetAt(t *testing.T) {
	s := openTemp(t)
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	_ = s.InsertFill(Fill{Exchange: "lighter", COID: 1, Side: "buy", Price: "100", Qty: "1", Fee: "0", Time: t0})
	_ = s.InsertFill(Fill{Exchange: "lighter", COID: 2, Side: "sell", Price: "110", Qty: "1", Fee: "0", Time: t0.Add(time.Hour)})
	if err := s.ResetStats("lighter", t0.Add(30*time.Minute)); err != nil {
		t.Fatal(err)
	}
	fills, err := s.ListFills("lighter", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(fills) != 1 || fills[0].COID != order.ClientOrderID(2) {
		t.Fatalf("after reset got %+v", fills)
	}
}

func openTemp(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "gridbot.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}
