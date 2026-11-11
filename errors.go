package providerkit

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
)

// MapError preserves explicit, public RPC errors. Unclassified errors are
// intentionally opaque: transport and parsing errors can contain credentials.
// Providers must put only safe, user-facing text in pluginabi.Error.Message.
//
// An unclassified error may additionally assert account-wide scope with an
// optional CredentialScoped() bool method (the host's native naming,
// IsCredentialScoped() bool, is honored as well). When either returns true,
// the mapped error is credential-scoped, so the host cools the whole
// credential across all models instead of only the reporting model. Whether a
// given upstream failure is account-wide is the provider's per-provider
// classification; MapError only plumbs the claim through.
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
	var scoped interface{ CredentialScoped() bool }
	if errors.As(err, &scoped) && scoped.CredentialScoped() {
		out.CredentialScoped = true
	}
	var hostScoped interface{ IsCredentialScoped() bool }
	if errors.As(err, &hostScoped) && hostScoped.IsCredentialScoped() {
		out.CredentialScoped = true
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

// ResetRetryAfterSeconds extracts an absolute quota-reset timestamp from an
// upstream error body and converts it to a retry-after hint in seconds,
// relative to now. It recognizes a numeric or RFC3339 "resets_at"/"reset_at"
// field anywhere in the JSON body, or an RFC3339 timestamp following "resets
// at" in a message string (for example "Your usage window resets at
// 2026-11-11T02:59:37Z."). A non-JSON body is scanned as raw text. The first
// future candidate in deterministic order wins; expired candidates are
// skipped, so a stale reset field cannot mask a later one. Missing or
// entirely past timestamps yield zero. No wall clock is hidden.
func ResetRetryAfterSeconds(body []byte, now time.Time) int {
	if len(body) == 0 {
		return 0
	}
	var decoded any
	if err := json.Unmarshal(body, &decoded); err != nil {
		decoded = string(body)
	}
	if t, ok := findResetTime(decoded, now); ok {
		return retryAfterFromTime(t, now)
	}
	return 0
}

// resetAtPattern matches an RFC3339 timestamp following "resets at".
var resetAtPattern = regexp.MustCompile(`(?i)\bresets?\s+at\s+([0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(?:\.[0-9]+)?(?:Z|[+-][0-9]{2}:[0-9]{2}))`)

func retryAfterFromTime(t, now time.Time) int {
	if !t.After(now) {
		return 0
	}
	seconds := math.Ceil(t.Sub(now).Seconds())
	if seconds > math.MaxInt32 {
		return math.MaxInt32
	}
	return int(seconds)
}

func findResetTime(v any, now time.Time) (time.Time, bool) {
	if t, ok := findResetField(v, now); ok {
		return t, true
	}
	return scanResetText(v, now)
}

// findResetField locates the first future "resets_at"/"reset_at" value in
// the decoded body, walking map keys in sorted order for determinism.
// Expired values are skipped so a stale field cannot hide a later reset.
func findResetField(v any, now time.Time) (time.Time, bool) {
	switch n := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(n))
		for k := range n {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if k == "resets_at" || k == "reset_at" {
				if t, ok := parseResetValue(n[k], now); ok && t.After(now) {
					return t, true
				}
			}
		}
		for _, k := range keys {
			if t, ok := findResetField(n[k], now); ok {
				return t, true
			}
		}
	case []any:
		for _, e := range n {
			if t, ok := findResetField(e, now); ok {
				return t, true
			}
		}
	}
	return time.Time{}, false
}

// epochFloatToTime converts a numeric Unix epoch to a time, preserving any
// fractional second. Non-finite and non-positive values are not timestamps.
// Epochs more than math.MaxInt32 seconds after now degrade to a sentinel at
// now+math.MaxInt32, which retryAfterFromTime reports as math.MaxInt32;
// casting them to int64 first would be implementation-defined.
func epochFloatToTime(n float64, now time.Time) (time.Time, bool) {
	if math.IsNaN(n) || math.IsInf(n, 0) {
		return time.Time{}, false
	}
	if n <= 0 {
		return time.Time{}, false
	}
	if n-float64(now.Unix()) > math.MaxInt32 {
		return now.Add(time.Duration(math.MaxInt32) * time.Second), true
	}
	// n is within math.MaxInt32 of now, so it fits in int64 and the
	// duration to now cannot overflow.
	seconds := int64(n)
	nanoseconds := int64((n - float64(seconds)) * 1e9)
	return time.Unix(seconds, nanoseconds), true
}

func parseResetValue(v any, now time.Time) (time.Time, bool) {
	switch n := v.(type) {
	case float64:
		return epochFloatToTime(n, now)
	case string:
		s := strings.TrimSpace(n)
		if s == "" {
			return time.Time{}, false
		}
		if secs, err := strconv.ParseInt(s, 10, 64); err == nil && secs > 0 {
			return time.Unix(secs, 0), true
		}
		if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
			return t, true
		}
		if f, err := strconv.ParseFloat(s, 64); err == nil {
			return epochFloatToTime(f, now)
		}
	}
	return time.Time{}, false
}

// scanResetText locates a future RFC3339 timestamp following "resets at" in
// any string value of the decoded body, walking map keys in sorted order.
// Expired matches are skipped like expired field values.
func scanResetText(v any, now time.Time) (time.Time, bool) {
	switch n := v.(type) {
	case string:
		for _, m := range resetAtPattern.FindAllStringSubmatch(n, -1) {
			if len(m) == 2 {
				if t, err := time.Parse(time.RFC3339Nano, m[1]); err == nil && t.After(now) {
					return t, true
				}
			}
		}
	case map[string]any:
		keys := make([]string, 0, len(n))
		for k := range n {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if t, ok := scanResetText(n[k], now); ok {
				return t, true
			}
		}
	case []any:
		for _, e := range n {
			if t, ok := scanResetText(e, now); ok {
				return t, true
			}
		}
	}
	return time.Time{}, false
}
