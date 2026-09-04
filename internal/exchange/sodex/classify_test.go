package sodex

import (
	"errors"
	"testing"

	"dex-grid/internal/exchange"

	"github.com/sodex-tech/sodex-go-sdk-public/client"
)

func TestClassify(t *testing.T) {
	cases := []struct {
		err  error
		want exchange.ErrorClass
	}{
		{&client.ErrAPI{Code: 429, Message: "slow down"}, exchange.ClassRetryable},
		{&client.ErrAPI{Code: 401, Message: "not authenticated"}, exchange.ClassFatal},
		{&client.ErrAPI{Code: 400, Message: "post-only would match"}, exchange.ClassPostOnlyRejected},
		{&client.ErrAPI{Code: 400, Message: "insufficient margin"}, exchange.ClassInsufficientMargin},
		{&client.ErrAPI{Code: 400, Message: "trailing zeros not allowed"}, exchange.ClassInvalidParam},
		{errors.New("client: HTTP 503 from POST /trade/orders: busy"), exchange.ClassRetryable},
		{errors.New("nonce is invalid"), exchange.ClassNonceStale},
	}
	for _, c := range cases {
		if got := classOf(c.err); got != c.want {
			t.Errorf("%v: got %s want %s", c.err, got, c.want)
		}
	}
}
