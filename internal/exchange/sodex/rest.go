package sodex

import (
	"context"
	"time"

	"dex-grid/internal/exchange"

	"github.com/sodex-tech/sodex-go-sdk-public/client"
	ctypes "github.com/sodex-tech/sodex-go-sdk-public/common/types"
	ptypes "github.com/sodex-tech/sodex-go-sdk-public/perps/types"
	"golang.org/x/time/rate"
)

// rest 在官方 SDK 客户端外包一层限流与可重试错误的退避。
type rest struct {
	cli     *client.Client
	limiter *rate.Limiter
	retries int
}

func newREST(cli *client.Client, rps, burst, retries int) *rest {
	if rps <= 0 {
		rps = 10
	}
	if burst <= 0 {
		burst = rps
	}
	if retries < 0 {
		retries = 0
	}
	return &rest{
		cli:     cli,
		limiter: rate.NewLimiter(rate.Limit(rps), burst),
		retries: retries,
	}
}

func call[T any](r *rest, ctx context.Context, op string, fn func() (T, error)) (T, error) {
	var zero T
	var last error
	for attempt := 0; attempt <= r.retries; attempt++ {
		if attempt > 0 {
			delay := time.Duration(200<<(attempt-1)) * time.Millisecond
			select {
			case <-ctx.Done():
				return zero, ctx.Err()
			case <-time.After(delay):
			}
		}
		if err := r.limiter.Wait(ctx); err != nil {
			return zero, err
		}
		v, err := fn()
		if err == nil {
			return v, nil
		}
		last = classify(op, err)
		if exchange.ClassOf(last) != exchange.ClassRetryable {
			return zero, last
		}
	}
	return zero, last
}

func (r *rest) symbols(ctx context.Context) ([]client.Symbol, error) {
	return call(r, ctx, "symbols", func() ([]client.Symbol, error) {
		return r.cli.PerpsSymbols(ctx)
	})
}

func (r *rest) tickers(ctx context.Context) ([]client.Ticker, error) {
	return call(r, ctx, "tickers", func() ([]client.Ticker, error) {
		return r.cli.PerpsTickers(ctx)
	})
}

func (r *rest) orderBook(ctx context.Context, symbol string, depth int) (*client.OrderBook, error) {
	return call(r, ctx, "orderbook", func() (*client.OrderBook, error) {
		return r.cli.PerpsOrderBook(ctx, symbol, depth)
	})
}

func (r *rest) klines(ctx context.Context, symbol, interval string, limit int) ([]client.Candle, error) {
	return call(r, ctx, "klines", func() ([]client.Candle, error) {
		return r.cli.PerpsKlines(ctx, symbol, interval, client.HistoryFilter{Limit: limit})
	})
}

func (r *rest) balances(ctx context.Context, address string) ([]client.Balance, error) {
	return call(r, ctx, "balances", func() ([]client.Balance, error) {
		return r.cli.PerpsBalances(ctx, address)
	})
}

func (r *rest) orders(ctx context.Context, address string) ([]client.Order, error) {
	return call(r, ctx, "orders", func() ([]client.Order, error) {
		return r.cli.PerpsOrders(ctx, address)
	})
}

func (r *rest) positions(ctx context.Context, address string) ([]client.Position, error) {
	return call(r, ctx, "positions", func() ([]client.Position, error) {
		return r.cli.PerpsPositions(ctx, address)
	})
}

func (r *rest) accountInfo(ctx context.Context, address string) (*client.AccountInfo, error) {
	return call(r, ctx, "account_info", func() (*client.AccountInfo, error) {
		return r.cli.SpotAccountInfo(ctx, address)
	})
}

func (r *rest) place(ctx context.Context, req *ptypes.NewOrderRequest) ([]client.PlaceOrderResult, error) {
	return call(r, ctx, "place_order", func() ([]client.PlaceOrderResult, error) {
		return r.cli.PlacePerpsOrder(ctx, req)
	})
}

func (r *rest) cancel(ctx context.Context, req *ptypes.CancelOrderRequest) ([]client.CancelOrderResult, error) {
	return call(r, ctx, "cancel_order", func() ([]client.CancelOrderResult, error) {
		return r.cli.CancelPerpsOrders(ctx, req)
	})
}

func (r *rest) modify(ctx context.Context, req *ptypes.ModifyOrderRequest) (*client.ModifyOrderResult, error) {
	return call(r, ctx, "modify_order", func() (*client.ModifyOrderResult, error) {
		return r.cli.ModifyPerpsOrder(ctx, req)
	})
}

// replace 改 GTC/GTX 限价单。/trade/orders/modify 只接受止盈止损单。
func (r *rest) replace(ctx context.Context, req *ctypes.ReplaceOrderRequest) ([]client.PlaceOrderResult, error) {
	return call(r, ctx, "modify_order", func() ([]client.PlaceOrderResult, error) {
		return r.cli.ReplacePerpsOrders(ctx, req)
	})
}

func (r *rest) leverage(ctx context.Context, req *ptypes.UpdateLeverageRequest) (*client.LeverageResult, error) {
	return call(r, ctx, "set_leverage", func() (*client.LeverageResult, error) {
		return r.cli.UpdateLeverage(ctx, req)
	})
}
