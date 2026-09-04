package sodex

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/sodex-tech/sodex-go-sdk-public/client"
)

// 公开接口冒烟。默认跳过，设置 SODEX_LIVE=1 时连主网。
func TestLivePublicMarkets(t *testing.T) {
	if os.Getenv("SODEX_LIVE") == "" {
		t.Skip("set SODEX_LIVE=1 to hit mainnet public REST")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cli := client.New(client.Config{BaseURL: client.DefaultBaseURL})
	ss, err := cli.PerpsSymbols(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(ss) == 0 {
		t.Fatal("no perps symbols")
	}
	trading := 0
	for _, s := range ss {
		if isActiveStatus(s.Status) {
			trading++
		}
	}
	if trading == 0 {
		t.Fatal("no TRADING perps")
	}
	ts, err := cli.PerpsTickers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(ts) == 0 {
		t.Fatal("no tickers")
	}
	t.Logf("symbols=%d trading=%d tickers=%d first=%s", len(ss), trading, len(ts), ss[0].Symbol)
}
