package sodex

import (
	"context"
	"errors"
	"strings"

	"dex-grid/internal/exchange"

	"github.com/sodex-tech/sodex-go-sdk-public/client"
)

func classify(op string, err error) error {
	if err == nil {
		return nil
	}
	return exchange.Classify(classOf(err), op, err)
}

func classOf(err error) exchange.ErrorClass {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return exchange.ClassRetryable
	}

	var ae *client.ErrAPI
	if errors.As(err, &ae) {
		if ae.Code == 429 || ae.Code >= 500 {
			return exchange.ClassRetryable
		}
		if c := matchMessage(ae.Message); c != exchange.ClassUnknown {
			return c
		}
		if ae.Code == 401 || ae.Code == 403 {
			return exchange.ClassFatal
		}
		if ae.Code >= 400 && ae.Code < 500 {
			return exchange.ClassInvalidParam
		}
		return matchMessage(ae.Message)
	}

	msg := err.Error()
	lower := strings.ToLower(msg)
	if strings.Contains(lower, "http 429") || strings.Contains(lower, "http 5") {
		return exchange.ClassRetryable
	}
	if c := matchMessage(msg); c != exchange.ClassUnknown {
		return c
	}
	if strings.Contains(lower, "http 401") || strings.Contains(lower, "http 403") {
		return exchange.ClassFatal
	}
	if strings.Contains(lower, "http 4") {
		return exchange.ClassInvalidParam
	}
	return exchange.ClassUnknown
}

var messagePatterns = []struct {
	class    exchange.ErrorClass
	keywords []string
}{
	{exchange.ClassPostOnlyRejected, []string{
		"post only", "post-only", "postonly", "gtx",
		"would match", "immediately match", "would cross", "crosses the book",
		"maker order would", "taker order not allowed",
	}},
	{exchange.ClassDuplicate, []string{
		"duplicate", "already exists", "order id exists", "clordid",
	}},
	{exchange.ClassInsufficientMargin, []string{
		"insufficient", "not enough", "exceeds available", "margin requirement",
		"collateral", "below maintenance", "insufficient margin",
	}},
	{exchange.ClassNonceStale, []string{
		"nonce",
	}},
	{exchange.ClassRetryable, []string{
		"rate limit", "too many requests", "timeout", "timed out",
		"temporarily", "try again", "busy",
	}},
	{exchange.ClassFatal, []string{
		"signature", "unauthorized", "invalid api key", "forbidden",
		"not authenticated",
	}},
	{exchange.ClassInvalidParam, []string{
		"invalid", "must be", "out of range", "not found", "unknown market",
		"reduce only", "reduce-only", "min ", "max ", "trailing zero",
	}},
}

func matchMessage(msg string) exchange.ErrorClass {
	lower := strings.ToLower(msg)
	for _, p := range messagePatterns {
		for _, kw := range p.keywords {
			if strings.Contains(lower, kw) {
				return p.class
			}
		}
	}
	return exchange.ClassUnknown
}
