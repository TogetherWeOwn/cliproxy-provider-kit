package providerkit

import (
	"context"
	"errors"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
)

// MapError preserves explicit, public RPC errors. Unclassified errors are
// intentionally opaque: transport and parsing errors can contain credentials.
// Providers must put only safe, user-facing text in pluginabi.Error.Message.
func MapError(err error) *pluginabi.Error {
	if err == nil {
		return nil
	}
	var rpc *pluginabi.Error
	if errors.As(err, &rpc) && rpc != nil {
		out := *rpc
		if out.Code == "" {
			out.Code = "plugin_error"
		}
		if out.Message == "" {
			out.Message = "plugin operation failed"
		}
		if out.HTTPStatus < 400 || out.HTTPStatus > 599 {
			out.HTTPStatus = 500
		}
		if out.RetryAfterSeconds < 0 {
			out.RetryAfterSeconds = 0
		}
		return &out
	}
	if errors.Is(err, context.Canceled) {
		return rpcError("canceled", "operation canceled", 499)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return &pluginabi.Error{Code: "timeout", Message: "operation timed out", HTTPStatus: 504, Retryable: true}
	}
	out := rpcError("plugin_error", "plugin operation failed", 500)
	var status interface{ StatusCode() int }
	if errors.As(err, &status) {
		if n := status.StatusCode(); n >= 400 && n <= 599 {
			out.HTTPStatus = n
		}
	}
	var retry interface{ RetryAfter() *time.Duration }
	if errors.As(err, &retry) {
		if d := retry.RetryAfter(); d != nil && *d > 0 {
			out.RetryAfterSeconds = int(math.Ceil(d.Seconds()))
		}
	}
	var retryable interface{ Retryable() bool }
	if errors.As(err, &retryable) {
		out.Retryable = retryable.Retryable()
	}
	return out
}
func rpcError(code, message string, status int) *pluginabi.Error {
	return &pluginabi.Error{Code: code, Message: message, HTTPStatus: status}
}

// RetryAfterSeconds parses HTTP Retry-After seconds or an HTTP date, rounding
// a future date up. Invalid or past values yield zero. No wall clock is hidden.
func RetryAfterSeconds(value string, now time.Time) int {
	value = strings.TrimSpace(value)
	if n, err := strconv.ParseInt(value, 10, 32); err == nil {
		if n > 0 {
			return int(n)
		}
		return 0
	}
	if t, err := http.ParseTime(value); err == nil && t.After(now) {
		seconds := math.Ceil(t.Sub(now).Seconds())
		if seconds > math.MaxInt32 {
			return math.MaxInt32
		}
		return int(seconds)
	}
	return 0
}
