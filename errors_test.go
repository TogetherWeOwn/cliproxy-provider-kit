package providerkit

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
)

type transportError struct {
	status    int
	after     *time.Duration
	retryable bool
}

func (e transportError) Error() string              { return "POST https://example.com/v1?key=example-key failed" }
func (e transportError) StatusCode() int            { return e.status }
func (e transportError) RetryAfter() *time.Duration { return e.after }
func (e transportError) Retryable() bool            { return e.retryable }

func TestMapErrorNormalizesPublicErrors(t *testing.T) {
	if MapError(nil) != nil {
		t.Fatal("nil error mapped")
	}
	for _, tc := range []struct {
		in   pluginabi.Error
		want pluginabi.Error
	}{
		{pluginabi.Error{}, pluginabi.Error{Code: "plugin_error", Message: "plugin operation failed", HTTPStatus: 500}},
		{pluginabi.Error{Code: "c", Message: "m", HTTPStatus: 200}, pluginabi.Error{Code: "c", Message: "m", HTTPStatus: 500}},
		{pluginabi.Error{Code: "c", Message: "m", HTTPStatus: 399}, pluginabi.Error{Code: "c", Message: "m", HTTPStatus: 500}},
		{pluginabi.Error{Code: "c", Message: "m", HTTPStatus: 600}, pluginabi.Error{Code: "c", Message: "m", HTTPStatus: 500}},
		{pluginabi.Error{Code: "c", Message: "m", HTTPStatus: 400}, pluginabi.Error{Code: "c", Message: "m", HTTPStatus: 400}},
		{pluginabi.Error{Code: "c", Message: "m", HTTPStatus: 599}, pluginabi.Error{Code: "c", Message: "m", HTTPStatus: 599}},
		{pluginabi.Error{Code: "c", Message: "m", HTTPStatus: 429, RetryAfterSeconds: -5, Retryable: true}, pluginabi.Error{Code: "c", Message: "m", HTTPStatus: 429, Retryable: true}},
	} {
		in := tc.in
		if got := MapError(&in); *got != tc.want {
			t.Fatalf("MapError(%+v) = %+v, want %+v", tc.in, *got, tc.want)
		}
		// Normalization must not rewrite a provider-owned (possibly shared) error value.
		if in != tc.in {
			t.Fatalf("input mutated: %+v", in)
		}
	}
}

func TestMapErrorContextAndOpaqueTransportErrors(t *testing.T) {
	e := MapError(fmt.Errorf("dial https://example.com/?key=example-key: %w", context.Canceled))
	if e.Code != "canceled" || e.HTTPStatus != 499 || e.Retryable || strings.Contains(e.Message, "example-key") {
		t.Fatalf("canceled %+v", e)
	}
	e = MapError(fmt.Errorf("read https://example.com/?key=example-key: %w", context.DeadlineExceeded))
	if e.Code != "timeout" || e.HTTPStatus != 504 || !e.Retryable || strings.Contains(e.Message, "example-key") {
		t.Fatalf("deadline %+v", e)
	}
	after := 1500 * time.Millisecond
	e = MapError(fmt.Errorf("wrapped: %w", transportError{status: 503, after: &after, retryable: true}))
	if e.Code != "plugin_error" || e.Message != "plugin operation failed" || e.HTTPStatus != 503 || e.RetryAfterSeconds != 2 || !e.Retryable {
		t.Fatalf("classified transport error %+v", e)
	}
	zero := time.Duration(0)
	for _, in := range []transportError{{status: 200}, {status: 302, after: &zero}, {status: 700}} {
		if e = MapError(in); e.HTTPStatus != 500 || e.RetryAfterSeconds != 0 || e.Retryable || strings.Contains(e.Message, "example-key") {
			t.Fatalf("MapError(%+v) = %+v", in, e)
		}
	}
}

func TestRetryAfterSecondsEdges(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		s    string
		want int
	}{
		{" 30 ", 30},
		{"0", 0},
		{"", 0},
		{now.Add(-time.Hour).Format(http.TimeFormat), 0},
		{now.Format(http.TimeFormat), 0},
		{now.Add(100 * 365 * 24 * time.Hour).Format(http.TimeFormat), math.MaxInt32},
	} {
		if got := RetryAfterSeconds(tc.s, now); got != tc.want {
			t.Fatalf("RetryAfterSeconds(%q) = %d, want %d", tc.s, got, tc.want)
		}
	}
	// HTTP dates have one-second resolution; a fraction of a second left still means wait.
	if got := RetryAfterSeconds(now.Add(5*time.Second).Format(http.TimeFormat), now.Add(200*time.Millisecond)); got != 5 {
		t.Fatalf("partial second rounded down: %d", got)
	}
}
